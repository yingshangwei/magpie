package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Claude Code makes a reply's MCP calls one after another, while the client
// runs them together and sends both results at once: the second result
// waits for its call instead of being lost (#84).
func TestResultForACallNotMadeYetWaitsForIt(t *testing.T) {
	b := newSubscriptionBridge()
	run := &subscriptionRun{bridge: b, token: "tok", pending: map[string]chan mcpToolResult{}}
	b.runs["tok"] = run
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cb/{token}", b.mcpCall)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	call := func(id string) chan string {
		out := make(chan string, 1)
		go func() {
			res, err := http.Post(srv.URL+"/cb/tok", "application/json", strings.NewReader(`{"tool_call_id":"`+id+`","name":"bash","arguments":{}}`))
			if err != nil {
				out <- err.Error()
				return
			}
			defer res.Body.Close()
			var r mcpToolResult
			_ = json.NewDecoder(res.Body).Decode(&r)
			out <- r.Content[0]["text"].(string)
		}()
		return out
	}
	first := call("call_1")
	for deadline := time.Now().Add(2 * time.Second); ; {
		b.mu.Lock()
		n := len(b.calls)
		b.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("call_1 was not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}

	req := &Request{Messages: []Message{
		{Role: "user", Parts: []Part{{Kind: ToolResult, CallID: "old", Text: "earlier"}}},
		{Role: "assistant", Parts: []Part{{Kind: ToolCall, ID: "call_1"}, {Kind: ToolCall, ID: "call_2"}}},
		{Role: "user", Parts: []Part{{Kind: ToolResult, CallID: "call_1", Text: "one"}, {Kind: ToolResult, CallID: "call_2", Text: "two"}}},
	}}
	found, results := b.findRun(req)
	if found != run || len(results) != 2 {
		t.Fatalf("findRun = %v, %d results", found == run, len(results))
	}
	if _, err := run.continueWith(results, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := <-first; got != "one" {
		t.Fatalf("call_1 answered %q", got)
	}
	select {
	case got := <-call("call_2"):
		if got != "two" {
			t.Fatalf("call_2 answered %q", got)
		}
	case <-time.After(2 * time.Second):
		run.finish() // lets the waiting call go, so the server can close
		t.Fatal("call_2 never had its result: the turn is stuck")
	}
}

// A model switch can arrive with results for the previous model's calls.
// Those IDs identify the old run, but must not override the selected model.
func TestClaudeToolResultsKeepTheSelectedModel(t *testing.T) {
	const opus, fable = "claude-opus-5-5", "claude-fable-5-2"
	for _, ids := range []string{"exact", "remapped"} {
		for _, selected := range []string{opus, fable} {
			t.Run(ids+"/"+selected, func(t *testing.T) {
				h := newRemapHarness(t)
				done := make(chan struct{})
				h.first = func(run *subscriptionRun, req *Request) {
					waiters := h.calls(run, true, shownCall{"call-model", "read", `{}`})
					go func() {
						defer close(done)
						if _, ok := <-waiters[0]; ok {
							h.says(run, "answered by "+run.model)
						}
					}()
				}
				initial := remapUser("read the file")
				if got := h.askModel(opus, initial); len(got) != 1 || got[0] != "tool_use call-model" {
					t.Fatalf("first turn: %v", got)
				}
				t.Cleanup(func() {
					h.b.abortAll()
					<-done
				})
				h.first = func(run *subscriptionRun, req *Request) {
					// A newly started run must receive the selected model and
					// the conversation, including the old call's result.
					if req.Model != selected || len(req.Messages) != 3 || req.Messages[2].Parts[0].Text != "file contents" {
						t.Errorf("new run lost the requested model or tool history: model=%s, messages=%d", req.Model, len(req.Messages))
					}
					h.says(run, "answered by "+run.model)
				}
				callID := "call-model"
				if ids == "remapped" {
					callID = "client-model"
				}
				conv := []string{initial, remapCalls(callID + "={}"), remapResults(callID + "=file contents")}
				got := h.askModel(selected, conv...)
				if len(got) != 1 || got[0] != "text answered by "+selected {
					t.Fatalf("selected %s, reply %v", selected, got)
				}
				wantRuns := 1
				if selected != opus {
					wantRuns = 2
					// A fork still on Opus may answer the original calls. The
					// switch must neither claim nor abort that waiting run.
					select {
					case <-done:
						t.Fatal("model switch consumed or ended the old run")
					default:
					}
					if got := h.askModel(opus, conv...); len(got) != 1 || got[0] != "text answered by "+opus {
						t.Fatalf("original model's fork: %v", got)
					}
				}
				if h.runs() != wantRuns {
					t.Fatalf("started %d runs, want %d", h.runs(), wantRuns)
				}
			})
		}
	}
}

func TestClaudeToolResultsAllowUnspecifiedModel(t *testing.T) {
	for _, models := range [][2]string{{"", "claude-fable-5-2"}, {"claude-opus-5-5", ""}, {"", ""}} {
		b := newSubscriptionBridge()
		run := &subscriptionRun{model: models[0]}
		b.calls["call-model"] = run
		req := &Request{Model: models[1], Messages: []Message{
			{Role: "assistant", Parts: []Part{{Kind: ToolCall, ID: "call-model"}}},
			{Role: "user", Parts: []Part{{Kind: ToolResult, CallID: "call-model", Text: "result"}}},
		}}
		if got, results, _ := b.match(req); got != run || len(results) != 1 {
			t.Fatalf("models %q: matched=%v, results=%d", models, got == run, len(results))
		}
	}
}
