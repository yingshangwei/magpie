package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

const cursorPendingChunk = `{"id":"c","model":"fake-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"magpie_native":{"accounting":"pending"}}}`

func TestCursorPendingContextDoesNotBill(t *testing.T) {
	for _, status := range []string{"pending", "replayed", "estimated", ""} {
		for _, measured := range []int{0, 123} {
			t.Run(fmt.Sprintf("%s/%d", status, measured), func(t *testing.T) {
				var chunk map[string]any
				if err := json.Unmarshal([]byte(cursorPendingChunk), &chunk); err != nil {
					t.Fatal(err)
				}
				usage := chunk["usage"].(map[string]any)
				usage["prompt_tokens"] = measured
				usage["magpie_native"] = map[string]any{"accounting": status}
				raw, _ := json.Marshal(chunk)
				d := &chatDecoder{contextEstimate: 800000}
				var got Usage
				if err := d.decode(string(raw), func(ev Event) {
					if ev.Kind == KUsage {
						got.add(ev.Usage)
					}
				}); err != nil {
					t.Fatal(err)
				}
				want := measured
				if status == "pending" && measured == 0 {
					want = 800000
				}
				if got.clientPrompt() != want || got.prompt() != measured || got.billed().Input != measured || got.billed().ContextEstimate != 0 {
					t.Fatalf("usage %+v, client want %d, billed want %d", got, want, measured)
				}
				if got.anthropic().InputTokens != want || got.chat()["prompt_tokens"] != want || got.responses()["input_tokens"] != want || got.gemini()["promptTokenCount"] != want {
					t.Fatalf("a translated client lost its context estimate: %+v", got)
				}
				got.Deferred = &Usage{Input: 20, Output: 30, CacheRead: 500}
				if billed := got.billed(); billed.Input != measured+20 || billed.CacheRead != 500 || billed.Output != 30 {
					t.Fatalf("estimated context entered deferred billing: %+v", billed)
				}
			})
		}
	}
}

// Exercise the installed-plugin path and the actual Anthropic stream/whole
// rendering. Other plugins' zero usage must not acquire Cursor's estimate.
func TestCursorPendingContextReachesClaudeCode(t *testing.T) {
	for _, id := range []string{"cursor", "factory"} {
		t.Run(id, func(t *testing.T) {
			movedFake(t, id, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(
					`data: {"id":"c","model":"fake-1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"check","arguments":"{}"}}]}}]}`,
					"data: "+cursorPendingChunk,
					"data: [DONE]",
				))
			}))
			s := New()
			for _, stream := range []bool{false, true} {
				body := fmt.Sprintf(`{"model":"%s/fake-1","max_tokens":1024,"stream":%t,"messages":[{"role":"user","content":"%s"}],"tools":[{"name":"check","description":"Check this case.","input_schema":{"type":"object","properties":{}}}]}`, id, stream, strings.Repeat("context ", 500))
				parsed, err := parseAnthropic([]byte(body))
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
				s.Handler().ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "call_1") || !strings.Contains(rec.Body.String(), "tool_use") {
					t.Fatalf("tool response: %d %s", rec.Code, rec.Body)
				}
				want := 0
				if id == "cursor" {
					want = estimate(parsed)
				}
				if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"input_tokens":%d`, want)) {
					t.Fatalf("%s stream=%t: no context %d: %s", id, stream, want, rec.Body)
				}
				if row := lastUsage(t); row.Input != 0 || row.Output != 0 || row.CacheRead != 0 || row.CacheWrite != 0 {
					t.Fatalf("pending estimate was billed: %+v", row)
				}
				// A measured step must take over from an earlier estimate.
				u := Usage{ContextEstimate: want}
				u.add(Usage{Input: 4, CacheRead: 120})
				if u.clientPrompt() != 124 {
					t.Fatalf("actual count did not replace estimate: %+v", u)
				}
			}
		})
	}
}

func TestPendingZeroUsageWithoutCursorContextStaysZero(t *testing.T) {
	d := decoder(provider.Chat)
	var got Usage
	if err := d(cursorPendingChunk, func(ev Event) { got.add(ev.Usage) }); err != nil {
		t.Fatal(err)
	}
	if got.clientPrompt() != 0 {
		t.Fatalf("a non-Cursor reply acquired an estimate: %+v", got)
	}
}
