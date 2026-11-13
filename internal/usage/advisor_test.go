package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

// Read the earlier dev format from disk without rewriting or backfilling it.
func TestHistoricalAdvisorRowsStaySubcalls(t *testing.T) {
	pageHome(t)
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	raw := fmt.Sprintf(`{"t":%q,"route_id":1,"agent":"claude-code","provider":"plan","model":"advisor","operation":"advisor","kind":"advisor","in":30,"out":500,"ttft_ms":100,"ms":1100,"session":"s"}
{"t":%q,"route_id":1,"agent":"claude-code","provider":"plan","model":"executor","in":10,"out":20,"status":200,"session":"s"}
`, at, at)
	if err := os.MkdirAll(filepath.Dir(Path()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		p := QueryPage(All, Filter{}, 0, 100)
		if p.Total != 2 || p.Sum.Calls != 1 || p.Sum.Input != 40 || p.Sum.Output != 520 || p.Sum.Errors != 0 || p.Sum.Retried != 0 || len(p.Speeds) != 0 {
			t.Fatalf("historical advisor became a request: %+v, speeds %+v", p.Sum, p.Speeds)
		}
		s := Summarize(All)
		if s.Calls != 1 || len(s.Models) != 2 || s.Input != 40 || s.Output != 520 {
			t.Fatalf("historical model breakdown: %+v", s)
		}
		calls := 0
		for _, d := range HeatmapOf(Filter{}).Days {
			calls += d.Calls
		}
		if calls != 1 {
			t.Fatalf("historical heatmap calls: %d", calls)
		}
		if v := Vias(time.Time{}).Of(ReportedAs("claude-code") + "|s"); len(v) != 1 || v[0].Model != "executor" {
			t.Fatalf("historical advisor became the session model: %+v", v)
		}
	}
	if b, err := os.ReadFile(Path()); err != nil || string(b) != raw {
		t.Fatalf("reading historical usage changed it: %v", err)
	}
}

func TestAdvisorDoesNotEndOrRetryARequest(t *testing.T) {
	pageHome(t)
	now := time.Now()
	// The subcall can be interleaved with attempts, including an old log
	// whose failed attempt did not carry HandedOn explicitly.
	for _, r := range []Record{
		{RouteID: 10, Time: now, Agent: "claude", Provider: "plan", Model: "executor", Input: 1, Status: 503},
		{RouteID: 10, Time: now, Agent: "claude", Provider: "plan", Model: "advisor", Operation: AdvisorOperation, Kind: "advisor", Input: 2, Output: 3},
		{RouteID: 10, Time: now, Agent: "claude", Provider: "plan", Model: "executor", Input: 4, Output: 5, Status: 200},
		{RouteID: 20, Time: now, Agent: "claude", Provider: "plan", Model: "advisor", Operation: AdvisorOperation, Kind: "advisor", Input: 6, Output: 7},
		{RouteID: 20, Time: now, Agent: "claude", Provider: "plan", Model: "executor", Input: 8, Status: 500},
	} {
		Append(r)
	}
	for _, p := range []RequestPage{QueryPage(All, Filter{}, 0, 100), QueryPage(All, Filter{}, 0, 100)} {
		if p.Total != 5 || p.Sum.Calls != 2 || p.Sum.Errors != 1 || p.Sum.Retried != 1 || p.Sum.Input != 21 || p.Sum.Output != 15 {
			t.Fatalf("advisor affected request outcomes or tokens: %+v", p.Sum)
		}
		for _, r := range p.Rows {
			if r.IsAdvisor() && (r.Tries != 0 || r.HandedOn || r.LostRequest() || r.RetriedRequest()) {
				t.Fatalf("advisor was an outcome or retry: %+v", r)
			}
		}
	}
	if p := QueryPage(All, Filter{Failed: true}, 0, 100); p.Total != 1 || p.Rows[0].RouteID != 20 {
		t.Fatalf("failure filter: %+v", p)
	}
	if p := QueryPage(All, Filter{Retried: true}, 0, 100); p.Total != 1 || p.Rows[0].RouteID != 10 {
		t.Fatalf("retry filter: %+v", p)
	}
}

// An advisor's sub-call ran on the executor's account and fills its
// window as its tokens say, but is no call routed through it.
func TestAdvisorFillsItsWindowWithinItsCall(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	five := now.Add(3 * time.Hour)
	qs := []provider.SubscriptionQuota{{Provider: "plan", User: "key", Windows: []provider.QuotaWindow{
		{Name: "5 hours", Used: 50, ResetsAt: &five, Span: 5 * time.Hour},
	}}}
	recs := []Record{
		{RouteID: 1, Time: now.Add(-time.Hour), Agent: "claude-code", Provider: "plan", ProviderKeyName: "key", Model: "executor", Input: 1000, Output: 500, Status: 200},
		{RouteID: 1, Time: now.Add(-time.Hour), Agent: "claude-code", Provider: "plan", ProviderKeyName: "key", Model: "advisor", Operation: AdvisorOperation, Kind: "advisor", Input: 300, Output: 50},
	}
	got := windowHolds(qs, now, now.Add(-30*24*time.Hour), func(Record) *catalog.Price { return nil }, provider.Renames{}, nil, nil, func(fn func(Record)) {
		for _, r := range recs {
			fn(r)
		}
	})
	if h := got[0].Windows[0].Holds; h == nil || h.Routed.Calls != 1 || h.Routed.Tokens != 1850 {
		t.Fatalf("window holds: %+v (why %q), want 1 call of 1850 tokens", h, got[0].Windows[0].HoldsWhy)
	}
}
