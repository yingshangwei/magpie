package gateway

// Cursor's native plugin (opencode-cursor-auth, 0.2.41+) marks a native
// tool step's usage "magpie_native":{"accounting":"pending"} with no prompt
// count: translated clients are given the current request's estimated
// context so Claude Code can compact, while the ledger and deferred usage
// never charge the estimate. A measured count replaces it, and another
// plugin's zero usage and a reply without the marker stay zero.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const cursorPendingChunk = `{"id":"c","model":"fake-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"magpie_native":{"accounting":"pending"}}}`

func TestCursorPendingContextDoesNotBill(t *testing.T) {
	for status, measured := range map[string]int{
		"pending":    0,
		"markerless": 0,
		"replayed":   123,
	} {
		t.Run(status, func(t *testing.T) {
			d := &chatDecoder{contextEstimate: 800000}
			var got Usage
			chunk := cursorPendingChunk
			switch status {
			case "markerless":
				// no marker: another plugin's zero usage stays an estimate
				// of nothing
				chunk = strings.Replace(chunk, `,"magpie_native":{"accounting":"pending"}`, "", 1)
			case "replayed":
				chunk = strings.Replace(chunk, `"prompt_tokens":0`, `"prompt_tokens":123`, 1)
			}
			if err := d.decode(chunk, func(ev Event) {
				if ev.Kind == KUsage {
					got.add(ev.Usage)
				}
			}); err != nil {
				t.Fatal(err)
			}
			switch status {
			case "pending":
				if got.clientPrompt() != 800000 || got.prompt() != 0 {
					t.Fatalf("pending: client %d billed %d", got.clientPrompt(), got.prompt())
				}
				if b := got.billed(); b.Input+b.CacheRead+b.CacheWrite != 0 {
					t.Fatalf("the estimate was billed: %+v", b)
				}
			case "markerless":
				if got.clientPrompt() != 0 {
					t.Fatalf("a zero usage without the marker gained an estimate: %+v", got)
				}
			case "replayed":
				if got.clientPrompt() != measured || got.ContextEstimate != 0 {
					t.Fatalf("measured usage did not take over: %+v", got)
				}
			}
		})
	}
}

// Every translated wire tells the estimate as the prompt count.
func TestCursorPendingContextOnEveryWire(t *testing.T) {
	u := Usage{ContextEstimate: 42}
	if in := u.chat()["prompt_tokens"]; in != 42 {
		t.Fatalf("chat prompt_tokens = %v", in)
	}
	if in := u.anthropic().InputTokens; in != 42 {
		t.Fatalf("anthropic input_tokens = %d", in)
	}
	if in := u.responses()["input_tokens"]; in != 42 {
		t.Fatalf("responses input_tokens = %v", in)
	}
	if in := u.gemini()["promptTokenCount"]; in != 42 {
		t.Fatalf("gemini promptTokenCount = %v", in)
	}
	// and the measured count still wins everywhere
	u = Usage{Input: 7, ContextEstimate: 42}
	if in := u.chat()["prompt_tokens"]; in != 7 {
		t.Fatalf("measured chat count lost: %v", in)
	}
	if u.anthropic().InputTokens != 7 || u.responses()["input_tokens"] != 7 || u.gemini()["promptTokenCount"] != 7 {
		t.Fatal("the measured count lost on another wire")
	}
	// Anthropic reports uncached input separately; the other wires count
	// its cache in the prompt. An estimate must not double those tokens.
	for _, input := range []int{0, 7} {
		u = Usage{Input: input, CacheRead: 11, CacheWrite: 13, ContextEstimate: 42}
		a := u.anthropic()
		if a.InputTokens != input || a.CacheReadInputTokens != 11 || a.CacheCreationInputTokens != 13 {
			t.Fatalf("Anthropic counted the cache twice: %+v", a)
		}
		want := input + 24
		if u.chat()["prompt_tokens"] != want || u.responses()["input_tokens"] != want || u.gemini()["promptTokenCount"] != want {
			t.Fatalf("the measured prompt including cache was lost: %+v", u)
		}
	}
}

// Through the installed-plugin path and the Anthropic wire, whole and
// streamed, a Cursor plugin's pending step reports a positive input count
// (the estimate), while another plugin's identical chunk reports zero.
func TestCursorPendingContextReachesClaudeCode(t *testing.T) {
	for _, id := range []string{"cursor", "factory"} {
		t.Run(id, func(t *testing.T) {
			up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + cursorPendingChunk + "\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			})
			movedFake(t, id, up)
			for _, streamed := range []bool{false, true} {
				body := `{"model":"` + id + `/fake-1","max_tokens":1024,"messages":[{"role":"user","content":"` +
					strings.Repeat("context ", 500) + `"}]` + func() string {
					if streamed {
						return `,"stream":true`
					}
					return ""
				}() + `,"tools":[{"name":"check","description":"runs a check","input_schema":{"type":"object","properties":{}}}]}`
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
				req.Header.Set("anthropic-version", "2023-06-01")
				New().Handler().ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("%s stream=%v: %d %s", id, streamed, rec.Code, rec.Body)
				}
				parsed, err := parseAnthropic([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				want := estimate(parsed)
				if want == 0 {
					t.Fatal("the estimate was zero")
				}
				var inputTokens int
				for _, m := range regexp.MustCompile(`"input_tokens":(\d+)`).FindAllStringSubmatch(rec.Body.String(), -1) {
					n, _ := strconv.Atoi(m[1])
					inputTokens = max(inputTokens, n)
				}
				if id == "cursor" && inputTokens != want {
					t.Fatalf("the pending estimate reached Claude Code as %d, want %d: %s", inputTokens, want, rec.Body.String())
				}
				if id == "factory" && inputTokens != 0 {
					t.Fatalf("another plugin's zero usage gained the estimate: %s", rec.Body.String())
				}
			}
		})
	}
}
