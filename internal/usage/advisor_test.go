package usage

import (
	"testing"
	"time"
)

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
