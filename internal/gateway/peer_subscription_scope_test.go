package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// A peer can send a shortened last user message and a generic prefix of
// the last reply. That may resume its own rewritten conversation, but not
// another peer's (or a local client's) process holding the private original.
func TestPeerSubscriptionSeparatesRewrittenHistory(t *testing.T) {
	for _, tc := range []struct {
		name, first, next string
	}{
		{"another peer", "alpha", "beta"},
		{"local to peer", "", "alpha"},
		{"peer to local", "alpha", ""},
		{"same peer", "alpha", "alpha"},
		{"local", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaude(t)
			s := New()
			t.Cleanup(s.subscription.abortAll)
			p := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: "u"}}
			ask := func(peer, session string, messages ...string) string {
				t.Helper()
				body := `{"model":"claude-sonnet-5","max_tokens":100,"system":"be brief","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[` + strings.Join(messages, ",") + `]}`
				r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
				if peer != "" {
					r = r.WithContext(context.WithValue(r.Context(), peerScopeKey{}, peerScope{id: peer}))
				}
				// Already distinct labels are not the native bridge's owner.
				r.Header.Set(SessionHeader, session)
				r.Header.Set("X-Session-Id", session)
				rec := httptest.NewRecorder()
				code, msg := s.serveClaudeSubscription(rec, r, provider.Anthropic, p, "claude-sonnet-5", []byte(body), &Usage{})
				if code != 200 {
					t.Fatalf("%d %s", code, msg)
				}
				var reply struct{ Content []struct{ Text string } }
				if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Content) != 1 {
					t.Fatalf("reply %s: %v", rec.Body, err)
				}
				return reply.Content[0].Text
			}
			message := func(role, text string) string {
				return `{"role":"` + role + `","content":` + strconv.Quote(text) + `}`
			}
			first := ask(tc.first, "source-session", message("user", "Public prefix PRIVATE_SOURCE_SECRET"))
			pid, _, ok := strings.Cut(strings.TrimPrefix(first, "pid "), " ")
			if !ok || !strings.HasSuffix(first, "turn 1") {
				t.Fatalf("first reply %q", first)
			}
			next := ask(tc.next, "different-session", message("user", "Public prefix"), message("assistant", "pid "), message("user", "continue"))
			if tc.first == tc.next {
				if next != "pid "+pid+" turn 2" {
					t.Fatalf("the caller's rewritten turn did not resume its process: first %q, next %q", first, next)
				}
				return
			}
			if strings.HasPrefix(next, "pid "+pid+" ") || !strings.HasSuffix(next, "turn 1") {
				t.Fatalf("another caller resumed the source process: first %q, next %q", first, next)
			}
			continued := ask(tc.first, "source-session", message("user", "Public prefix PRIVATE_SOURCE_SECRET"), message("assistant", first), message("user", "continue"))
			if continued != "pid "+pid+" turn 2" {
				t.Fatalf("another caller displaced the source process: first %q, continued %q", first, continued)
			}
		})
	}
}

// The peer domain must be checked before a matching tool result claims
// the run: the later account-change check aborts a run owned by somebody
// else. Both exact tool IDs and client-renamed IDs must leave it untouched.
func TestPeerSubscriptionToolResultsStayInCallerDomain(t *testing.T) {
	for _, ids := range []string{"exact", "remapped"} {
		for _, tc := range []struct {
			name, source, other string
		}{
			{"another peer", "peer:alpha/claude\x00u", "peer:beta/claude\x00u"},
			{"local to peer", "claude\x00u", "peer:alpha/claude\x00u"},
			{"peer to local", "peer:alpha/claude\x00u", "claude\x00u"},
		} {
			t.Run(ids+"/"+tc.name, func(t *testing.T) {
				h := newRemapHarness(t)
				var source *subscriptionRun
				var waiter chan mcpToolResult
				started := 0
				ask := func(owner string, messages ...string) string {
					t.Helper()
					start := func(_ context.Context, _ *Request) (*subscriptionRun, <-chan Event, error) {
						started++
						run := &subscriptionRun{bridge: h.b, owner: owner, token: "run" + strconv.Itoa(started), model: "claude-sonnet-5", pending: map[string]chan mcpToolResult{}}
						h.b.mu.Lock()
						h.b.runs[run.token] = run
						h.b.mu.Unlock()
						events := run.attach()
						if source == nil {
							source = run
							waiter = h.calls(run, true, shownCall{"source-call", "read", `{}`})[0]
						} else {
							h.says(run, "a new process")
						}
						return run, events, nil
					}
					body := `{"model":"claude-sonnet-5","max_tokens":100,"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[` + strings.Join(messages, ",") + `]}`
					rec := httptest.NewRecorder()
					code, msg := h.s.serveSubscription(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, "Claude Code", "claude-sonnet-5", owner, []byte(body), &Usage{}, start)
					if code != 200 {
						t.Fatalf("%d %s", code, msg)
					}
					var reply struct{ Content []struct{ Text, ID string } }
					if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || len(reply.Content) != 1 {
						t.Fatalf("reply %s: %v", rec.Body, err)
					}
					return reply.Content[0].Text + reply.Content[0].ID
				}
				if got := ask(tc.source, remapUser("read")); got != "source-call" {
					t.Fatalf("first reply %q", got)
				}
				callID := "source-call"
				if ids == "remapped" {
					callID = "client-call"
				}
				if got := ask(tc.other, remapUser("read"), remapCalls(callID+"={}"), remapResults(callID+"=foreign result")); got != "a new process" {
					t.Fatalf("another caller's reply %q", got)
				}
				source.mu.Lock()
				closed, resuming := source.closed, source.resuming
				source.mu.Unlock()
				if closed || resuming {
					t.Fatalf("another caller changed the waiting process: closed=%v, resuming=%v", closed, resuming)
				}
				select {
				case result, open := <-waiter:
					t.Fatalf("another caller answered or closed the source tool: open=%v, result=%v", open, result)
				default:
				}
				received := make(chan string, 1)
				go func() {
					result := resultOf(t, waiter)
					h.says(source, "source completed")
					received <- result
				}()
				if got := ask(tc.source, remapUser("read"), remapCalls(callID+"={}"), remapResults(callID+"=legitimate result")); got != "source completed" || started != 2 {
					t.Fatalf("source failed to resume its waiting process: reply %q, starts %d", got, started)
				}
				if got := <-received; got != "legitimate result" {
					t.Fatalf("source tool received %q", got)
				}
			})
		}
	}
}

// The peer prefix must not change the account/home fields used when an
// account moves from a saved directory back to Claude Code's own home.
func TestPeerSubscriptionAccountHandover(t *testing.T) {
	h := newRemapHarness(t)
	add := func(owner string) *subscriptionRun {
		run := &subscriptionRun{bridge: h.b, owner: owner, token: owner, pending: map[string]chan mcpToolResult{}}
		h.b.mu.Lock()
		h.b.runs[owner], h.b.idle[owner] = run, run
		run.idleKey = owner
		h.b.mu.Unlock()
		return run
	}
	moved := add("peer:alpha/claude\x00B@example.com")
	other := add("peer:beta/claude\x00a@example.com")
	own := add("peer:alpha/claude\x00b@example.com\x00" + ownHome)
	provider.ClaudeHandedOver("b@example.com")
	if !moved.closed || other.closed || own.closed {
		t.Fatalf("account handover after peer scoping: moved=%v, other=%v, own home=%v", moved.closed, other.closed, own.closed)
	}
}
