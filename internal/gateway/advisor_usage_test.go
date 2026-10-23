package gateway

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/budget"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/usage"
)

// Counts and shapes come from a native Claude Code advisor reply. The
// second advisor iteration exercises another price and an hour cache write.
const advisorUsageFixture = `{"input_tokens":4,"output_tokens":137,"cache_read_input_tokens":4444,"cache_creation_input_tokens":4759,"iterations":[{"type":"message","input_tokens":4,"output_tokens":137},{"type":"advisor_message","model":"advisor-a","input_tokens":6024,"output_tokens":371,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}},{"type":"advisor_message","model":"advisor-b","input_tokens":100,"output_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":25}}]}`

func TestAdvisorUsageLedger(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			fresh(t)
			budget.Forget()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"executor-reply\",\"model\":\"executor\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":4}}}\n\n")
					fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"ok\"}}\n\n")
					fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":%s}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", advisorUsageFixture)
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":"executor-reply","type":"message","role":"assistant","model":"executor","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":%s}`, advisorUsageFixture)
				}
			}))
			defer up.Close()
			if err := provider.Save(provider.Provider{ID: "plan", Name: "Plan", Key: "fixture", Anthropic: up.URL + "/v1", Models: []string{"executor", "advisor-a", "advisor-b"}}); err != nil {
				t.Fatal(err)
			}
			for model, price := range map[string]catalog.Price{
				"executor":  {Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25},
				"advisor-a": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
				"advisor-b": {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25, CacheWrite1h: 10},
			} {
				if err := provider.SetModelPrice("plan/"+model, &price); err != nil {
					t.Fatal(err)
				}
			}
			keys, secrets := newCaller(t, "Advisor test")
			if _, err := access.Update("limit-key", access.Change{Key: keys[0].ID, Limit: &access.Limit{Period: "day", Tokens: 1000000}}); err != nil {
				t.Fatal(err)
			}
			s := New()
			h := lanGuard(s.Handler())
			req := httptest.NewRequest("POST", "http://127.0.0.1/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"plan/executor","max_tokens":1000,"stream":%t,"messages":[{"role":"user","content":"fixture"}]}`, stream)))
			req.RemoteAddr = "127.0.0.1:5000"
			req.Header.Set("Authorization", "Bearer "+secrets[0])
			req.Header.Set("User-Agent", "claude-cli/fixture")
			req.Header.Set("X-Claude-Code-Session-Id", "advisor-test-session")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("gateway: %d %s", w.Code, w.Body.String())
			}
			if !stream && gjson.Get(w.Body.String(), "usage.input_tokens").Int() != 4 {
				t.Fatal("advisor polluted executor wire totals", w.Body.String())
			}
			records := usage.Load(time.Time{})
			if len(records) != 3 {
				t.Fatalf("advisor ledger rows = %d, want executor plus two advisor rows: %+v", len(records), records)
			}
			for _, r := range records {
				if r.CallerKeyID != keys[0].ID || r.Session != "advisor-test-session" || r.Provider != "plan" || r.RouteID != records[0].RouteID {
					t.Fatalf("advisor lost parent billing identity: %+v", r)
				}
				if r.Model == "executor" {
					if r.Input != 4 || r.Output != 137 || r.CacheRead != 4444 || r.CacheWrite != 4759 || r.Operation != "" {
						t.Fatalf("executor double-counted: %+v", r)
					}
				} else if r.Operation != usage.AdvisorOperation || r.RequestID != "" || r.ResponseID != "" || r.TTFT != 0 || r.Millis != 0 || r.BodyIn != "" || r.BodyOut != "" || r.Requested != "" {
					t.Fatalf("advisor copied executor-only metadata: %+v", r)
				}
			}
			check := func() {
				t.Helper()
				page := usage.QueryPage(usage.All, usage.Filter{}, 0, 100)
				sum := page.Sum
				wantCost := 0.00667115 + 0.023637 + 0.00104125
				if page.Total != 3 || sum.Calls != 1 || sum.Errors != 0 || sum.Retried != 0 || sum.Input != 6128 || sum.Output != 518 || sum.CacheRead != 4464 || sum.CacheWrite != 4789 || sum.CacheWrite1h != 25 || math.Abs(sum.Cost-wantCost) > 1e-9 {
					t.Fatalf("indexed ledger: rows=%d %+v; want cost %g", page.Total, sum, wantCost)
				}
				currentKeys, err := access.List()
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range currentKeys {
					if key.ID != keys[0].ID {
						continue
					}
					if got := budget.Of(key, time.Now()); got == nil || got.Calls != 1 || got.Tokens != 11435 || math.Abs(got.Cost-wantCost) > 1e-9 {
						t.Fatalf("advisor budget: %+v", got)
					}
				}
				calls := 0
				for _, day := range usage.HeatmapOf(usage.Filter{}).Days {
					calls += day.Calls
				}
				if calls != 1 {
					t.Fatalf("advisor inflated heatmap request count: %d", calls)
				}
				summary := usage.Summarize(usage.All)
				if summary.Calls != 1 || len(summary.Models) != 3 || math.Abs(summary.Cost-wantCost) > 1e-9 {
					t.Fatalf("model summary: %+v", summary)
				}
				if v := usage.Vias(time.Time{})[usage.ReportedAs(records[len(records)-1].Agent)+"|advisor-test-session"]; len(v) != 1 || v[0].Model != "executor" {
					t.Fatalf("advisor became the selected session model: %+v", v)
				}
				var exported bytes.Buffer
				if err := usage.WriteCSV(&exported, page.Rows); err != nil {
					t.Fatal(err)
				}
				csvRows, err := csv.NewReader(strings.NewReader(exported.String())).ReadAll()
				if err != nil || len(csvRows) != 4 || !strings.Contains(exported.String(), "advisor-a") || !strings.Contains(exported.String(), "advisor-b") {
					t.Fatal("CSV omitted advisor rows", err, exported.String())
				}
			}
			check()
			// The second query uses the packed ledger, then a new price must reprice it.
			check()
			if err := provider.SetModelPrice("plan/advisor-a", &catalog.Price{}); err != nil {
				t.Fatal(err)
			}
			page := usage.QueryPage(usage.All, usage.Filter{}, 0, 100)
			if math.Abs(page.Sum.Cost-(0.00667115+0.00104125)) > 1e-9 {
				t.Fatalf("advisor price change did not reprice cached ledger: %+v", page.Sum)
			}
		})
	}
}

func TestAdvisorUsageIgnoresNonAdvisorIterations(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"iterations":[{"type":"message","input_tokens":100,"output_tokens":10},{"type":"advisor_message","input_tokens":30},{"type":"advisor_message","model":"empty"},{"type":"advisor_message","model":"bad","input_tokens":-5,"output_tokens":10},{"type":"future_message","model":"unknown","input_tokens":50}]}`), &u); err != nil {
		t.Fatal(err)
	}
	if got := advisorRecords(usage.Record{}, u); len(got) != 0 {
		t.Fatalf("inferred billable advisor usage: %+v", got)
	}
}

func TestAdvisorRouteUsageKeepsExecutorPrompt(t *testing.T) {
	var raw aUsage
	if err := json.Unmarshal([]byte(advisorUsageFixture), &raw); err != nil {
		t.Fatal(err)
	}
	u := raw.usage()
	for _, zero := range []bool{false, true} {
		if zero {
			u.Input, u.Output, u.CacheRead, u.CacheWrite = 0, 0, 0, 0
		}
		got := routeUsage("plan", "executor", u)
		if len(got) != 3 || got[0].Model != "advisor-a" || got[1].CacheWrite1h != 25 || got[2].Model != "executor" || promptCounted(got) != u.prompt() {
			t.Fatalf("advisor changed prompt calibration: %+v", got)
		}
		if !got[0].PricingRecord().IsAdvisor() {
			t.Fatal("route pricing lost advisor identity")
		}
	}
}
