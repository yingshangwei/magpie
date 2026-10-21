package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

// The two leading blocks are from Claude Code 2.1.296. The local live A/B
// answered the neutral request before and after rejecting either block.
const agClaudeBilling = "x-anthropic-billing-header: cc_version=2.1.296; cc_entrypoint=cli; cch=00000;"
const agClaudeIdentity = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
const agClaudeRules = "\nKeep the user's business rules about Anthropic and Claude Code unchanged."

func agClaudeReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, sse(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1},"modelVersion":"gemini-4-argon-trial"}}`))
}

func TestAntigravityGemini4ClaudeSystem(t *testing.T) {
	system := agClaudeBilling + "\n" + agClaudeIdentity + agClaudeRules
	for _, c := range []struct{ name, path, body string }{
		{"messages-string", "/v1/messages", `{"model":"group/g","max_tokens":100,"system":` + quote(system) + `,"messages":[{"role":"user","content":"hi"}]}`},
		{"messages-blocks", "/v1/messages", `{"model":"group/g","max_tokens":100,"stream":true,"system":[{"type":"text","text":` + quote(agClaudeBilling) + `},{"type":"text","text":` + quote(agClaudeIdentity) + `},{"type":"text","text":` + quote(agClaudeRules) + `}],"messages":[{"role":"user","content":` + quote(agClaudeIdentity) + `}],"tools":[{"name":"read","description":` + quote(agClaudeIdentity) + `,"input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`},
		{"chat", "/v1/chat/completions", `{"model":"group/g","max_tokens":100,"messages":[{"role":"system","content":` + quote(system) + `},{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"group/g","max_output_tokens":100,"instructions":` + quote(system) + `,"input":"hi"}`},
		{"gemini", "/v1beta/models/group/g:generateContent", `{"systemInstruction":{"parts":[{"text":` + quote(system) + `}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		for _, foreseen := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/foreseen=%t", c.name, foreseen), func(t *testing.T) {
				var asked atomic.Int32
				s, _ := antigravityGroupFor(t, []string{"u@example.com"}, "gemini-4-argon", func(w http.ResponseWriter, r *http.Request) {
					asked.Add(1)
					b, _ := io.ReadAll(r.Body)
					got := gjson.GetBytes(b, "request.systemInstruction.parts.0.text").String()
					want := "You are a helpful coding assistant." + agClaudeRules
					if got != want {
						t.Errorf("upstream system = %q, want %q", got, want)
						w.WriteHeader(429)
						io.WriteString(w, turnedAway)
						return
					}
					if c.name == "messages-blocks" {
						if gjson.GetBytes(b, "request.contents.0.parts.0.text").String() != agClaudeIdentity ||
							gjson.GetBytes(b, "request.tools.0.functionDeclarations.0.description").String() != agClaudeIdentity {
							t.Error("client identity text in a user message or tool description changed")
						}
					}
					agClaudeReply(w)
				})
				if foreseen {
					s.agTurnedAway.Store(time.Now().UnixNano())
				}
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", c.path, strings.NewReader(c.body)))
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "OK") || asked.Load() != 1 {
					t.Fatalf("request = %d %s; Antigravity asked %d times", rec.Code, rec.Body.String(), asked.Load())
				}
				if route := lastRoute(s); len(route.Tries) != 1 || route.Tries[0].ID != "antigravity" || route.Tries[0].Status != 200 {
					t.Fatalf("unexpected fallback: %+v", route.Tries)
				}
			})
		}
	}
}

func TestAntigravityGemini4ClaudeQuota(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			s, _ := antigravityGroupFor(t, []string{"u@example.com"}, "gemini-4-argon", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(429)
				io.WriteString(w, turnedAway)
			})
			body := `{"model":"antigravity/gemini-4-argon","max_tokens":100,"stream":` + fmt.Sprint(stream) + `,"system":` + quote(agClaudeBilling+"\n"+agClaudeIdentity) + `,"messages":[{"role":"user","content":"hi"}]}`
			code, out := turnOn(t, s, body)
			if code != 429 || strings.Contains(out, antigravityTurnedAwayHint) {
				t.Fatalf("quota was classified from the original prompt: %d %s", code, out)
			}
			if route := lastRoute(s); len(route.Tries) != 1 || route.Tries[0].Fail == failPrompt || s.agTurnedAway.Load() != 0 {
				t.Fatalf("quota became a remembered prompt refusal: %+v", route.Tries)
			}
		})
	}
}

func TestAntigravityGemini4ClaudeScope(t *testing.T) {
	ag := provider.Provider{ID: "renamed", Account: &provider.Account{Agent: "antigravity"}}
	system := agClaudeBilling + agClaudeIdentity + agClaudeRules
	for _, c := range []struct {
		name, model, input, want string
		p                        provider.Provider
	}{
		{"billing only", "gemini-4-argon", agClaudeBilling + "\nRules", "Rules", ag},
		{"SDK only", "gemini-4-argon-high", agClaudeIdentity + agClaudeRules, "You are a helpful coding assistant." + agClaudeRules, ag},
		{"renamed account", "gemini-4-argon-medium", system, "You are a helpful coding assistant." + agClaudeRules, ag},
		{"older Gemini", "gemini-3.8-flash", system, system, ag},
		{"Claude on Antigravity", "claude-sonnet-4-6", system, system, ag},
		{"Gemini CLI", "gemini-4-argon", system, system, provider.Provider{Account: &provider.Account{Agent: "gemini"}}},
		{"same provider name is no account", "gemini-4-argon", system, system, provider.Provider{ID: "antigravity"}},
		{"mentioned in business rules", "gemini-4-argon", "Keep this example: " + system, "Keep this example: " + system, ag},
		{"not billing metadata", "gemini-4-argon", "x-anthropic-billing-header: documentation", "x-anthropic-billing-header: documentation", ag},
		{"neutral", "gemini-4-argon", "Be brief.", "Be brief.", ag},
		{"empty", "gemini-4-argon", "", "", ag},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := antigravityClaudeSystem(c.p, c.model, c.input); got != c.want {
				t.Fatalf("system = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAntigravityGemini4ClaudeFallbackKeepsOriginal(t *testing.T) {
	s, _ := antigravityGroupFor(t, []string{"u@example.com"}, "gemini-4-argon", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, turnedAway)
	})
	u, url := newBillingUpstream(t)
	p, err := provider.Find("other")
	if err != nil {
		t.Fatal(err)
	}
	p.Anthropic = url
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	if code, out := turnOn(t, s, claudeCodeTurn("group/g", "00000")); code != 200 || !strings.Contains(out, "ok") {
		t.Fatalf("fallback = %d %s", code, out)
	}
	// The other vendor's existing billing-line rule still applies, while
	// this route's neutral SDK identity must not escape its own attempt.
	if got, want := gjson.Get(u.last(t), "system").Raw, gjson.Get(claudeCodeTurnWithoutLine("x"), "system").Raw; got != want {
		t.Fatalf("fallback system = %s, want %s", got, want)
	}
}
