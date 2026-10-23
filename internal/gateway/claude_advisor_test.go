package gateway

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

func TestClaudeSubscriptionForwardsAdvisor(t *testing.T) {
	h := newScriptHarness(t, "ok", `echo "advisor_env $CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL" >> $log`)
	body := advisorTurn("claude/claude-sonnet-5-5", "claude/claude-opus-5-5[1m]")
	var u Usage
	w := httptest.NewRecorder()
	code, msg := h.s.serveClaudeSubscription(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, h.p, "claude-sonnet-5-5", []byte(body), &u)
	if code != 200 {
		t.Fatalf("%d: %s", code, msg)
	}
	for _, log := range h.runs() {
		if !strings.Contains(log, "--advisor claude-opus-5-5") || !strings.Contains(log, "advisor_env 1") {
			t.Errorf("Claude Code did not receive the requested advisor: %s", log)
		}
		if !strings.Contains(log, "You have access to an `advisor` tool") || strings.Contains(log, advisorGone) {
			t.Errorf("the available advisor's instructions were removed: %s", log)
		}
	}
}

func TestClaudeSubscriptionAdvisorChanges(t *testing.T) {
	h := newResumeHarness(t)
	var messages []map[string]any
	for i, advisor := range []string{"claude-opus-5-5", "claude-opus-5-5", "", "claude-fable-5-1"} {
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprintf("turn %d", i)})
		tools := []any{map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}}
		if advisor != "" {
			tools = append(tools, map[string]any{"type": "advisor_20260301", "name": "advisor", "model": advisor})
		}
		body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5-5", "max_tokens": 100, "messages": messages, "tools": tools})
		var u Usage
		code, msg := h.s.serveClaudeSubscription(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))), provider.Anthropic, h.p, "claude-sonnet-5-5", body, &u)
		if code != 200 {
			t.Fatalf("turn %d: %d %s", i, code, msg)
		}
		messages = append(messages, map[string]any{"role": "assistant", "content": "ok"})
		want := []int{1, 1, 2, 3}[i]
		if got := len(h.runs()); got != want {
			t.Fatalf("turn %d advisor %q: %d runs, want %d; a changed advisor must not reuse the old setting", i, advisor, got, want)
		}
	}
}

// The block shapes are Claude Code 2.1.296's stream-json output and the
// Messages advisor contract. Payloads are synthetic; encrypted content must
// survive as an opaque string, including when a pause splits the reply.
func TestClaudeSubscriptionAdvisorReply(t *testing.T) {
	call := `{"type":"server_tool_use","id":"srvtoolu_advisor","name":"advisor","input":{}}`
	for _, result := range []string{
		`{"type":"advisor_redacted_result","encrypted_content":"opaque+/=value","stop_reason":"end_turn"}`,
		`{"type":"advisor_result","text":"Add the rates.","stop_reason":"end_turn"}`,
		`{"type":"advisor_tool_result_error","error_code":"unavailable"}`,
	} {
		for _, paused := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pause=%t", gjson.Get(result, "type"), paused), func(t *testing.T) {
				block := `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_advisor","content":` + result + `}`
				event := func(s string) string { return `{"type":"stream_event","event":` + s + `}` }
				lines := []string{
					event(`{"type":"message_start","message":{"id":"m1","model":"claude-sonnet-5-5","usage":{"input_tokens":10}}}`),
					event(`{"type":"content_block_start","index":0,"content_block":` + call + `}`),
					event(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
				}
				if paused {
					lines = append(lines,
						event(`{"type":"message_delta","delta":{"stop_reason":"pause_turn"},"usage":{"output_tokens":2,"iterations":[{"type":"message","input_tokens":10,"output_tokens":2}]}}`),
						event(`{"type":"message_stop"}`),
						event(`{"type":"message_start","message":{"id":"m2","model":"claude-sonnet-5-5","usage":{"input_tokens":20}}}`))
				}
				lines = append(lines,
					event(`{"type":"content_block_start","index":1,"content_block":`+block+`}`),
					event(`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}`),
					event(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3,"iterations":[{"type":"message","input_tokens":20,"output_tokens":3},{"type":"advisor_message","model":"claude-opus-5-5","input_tokens":30,"output_tokens":4}]}}`),
					event(`{"type":"message_stop"}`))
				run := &subscriptionRun{}
				seg := run.attach()
				run.readOutput(strings.NewReader(strings.Join(lines, "\n") + "\n"))
				w := httptest.NewRecorder()
				encoder := &anthropicEncoder{w: newSSEWriter(w)}
				var c collector
				starts, stops, args := 0, 0, 0
				for e := range seg {
					c.add(e)
					encoder.event(e)
					if e.Kind == KStart {
						starts++
					}
					if e.Kind == KStop {
						stops++
					}
					if e.Kind == KToolArgs {
						args++
					}
				}
				encoder.finish()
				whole := string(renderAnthropic(c.res, "claude-sonnet-5-5"))
				if starts != 1 || stops != 1 || args != 0 || !strings.Contains(whole, `"text":"answer"`) {
					t.Fatalf("advisor turn was split or treated as a client tool: starts=%d stops=%d args=%d reply=%s", starts, stops, args, whole)
				}
				wantIterations, wantOutput := 2, 3
				if paused {
					wantIterations, wantOutput = 3, 5
				}
				if got := gjson.Get(whole, "usage.iterations.#").Int(); got != int64(wantIterations) || c.res.Usage.Output != wantOutput {
					t.Errorf("executor totals or advisor iterations changed: %s", whole)
				}
				advisors := advisorRecords(usage.Record{}, c.res.Usage)
				if len(advisors) != 1 || advisors[0].Model != "claude-opus-5-5" || advisors[0].Input != 30 || advisors[0].Output != 4 {
					t.Fatalf("native pause or usage updates duplicated/lost advisor billing: %+v", advisors)
				}
				for name, out := range map[string]string{"whole": whole, "stream": w.Body.String()} {
					if !strings.Contains(out, call) || !strings.Contains(out, block) {
						t.Errorf("%s lost the advisor call or result: %s", name, out)
					}
					if !strings.Contains(out, `"type":"advisor_message"`) || !strings.Contains(out, `"model":"claude-opus-5-5"`) {
						t.Errorf("%s lost the advisor usage breakdown: %s", name, out)
					}
				}
			})
		}
	}
}

func TestClaudeSubscriptionAdvisorWholeOutput(t *testing.T) {
	call := `{"type":"server_tool_use","id":"srvtoolu_whole","name":"advisor","input":{}}`
	result := `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_whole","content":{"type":"advisor_redacted_result","encrypted_content":"whole-opaque"}}`
	run := &subscriptionRun{}
	seg := run.attach()
	lines := `{"type":"assistant","message":{"id":"whole","model":"claude-sonnet-5-5","content":[` + call + `,` + result + `,{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}}` + "\n" + `{"type":"result","subtype":"success","is_error":false}` + "\n"
	run.readOutput(strings.NewReader(lines))
	var c collector
	for e := range seg {
		c.add(e)
	}
	out := string(renderAnthropic(c.res, "claude-sonnet-5-5"))
	if !strings.Contains(out, call) || !strings.Contains(out, result) || !strings.Contains(out, `"text":"answer"`) {
		t.Fatalf("unstreamed Claude Code output lost the advisor: %s", out)
	}
}

func TestClaudeSubscriptionLeavesForeignAdvisorOut(t *testing.T) {
	h := newResumeHarness(t)
	body := advisorTurn("claude/claude-sonnet-5-5", "other/claude-opus-5-5")
	var u Usage
	code, msg := h.s.serveClaudeSubscription(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, h.p, "claude-sonnet-5-5", []byte(body), &u)
	if code != 200 {
		t.Fatalf("%d: %s", code, msg)
	}
	for _, log := range h.runs() {
		if strings.Contains(log, "--advisor") || !strings.Contains(log, advisorGone) {
			t.Errorf("another provider's advisor reached the subscription: %s", log)
		}
	}
}
