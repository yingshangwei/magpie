package gateway

// Account-level reasoning effort: settings.accountEfforts forces an effort
// only once the provider account or key the try is sent on is selected. A
// fallback starts each try from the original request, so the second
// account's setting applies, not the first's.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// effortUp fails the named key's first failN calls (or every one when
// failN is negative) and records the reasoning effort each call carried.
type effortUp struct {
	failN  map[string]int
	effort atomic.Value
}

func (u *effortUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	key := r.Header.Get("Authorization")
	if n := u.failN[key]; n != 0 {
		if n > 0 {
			u.failN[key] = n - 1
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
		return
	}
	var body struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	_ = json.Unmarshal(b, &body)
	if body.ReasoningEffort != "" {
		u.effort.Store(body.ReasoningEffort)
	} else {
		u.effort.Store(body.Reasoning.Effort)
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, chatOK)
}

func TestAccountEffortDirectGroupAndFallback(t *testing.T) {
	fresh(t)
	up := &effortUp{failN: map[string]int{}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	to, _ := url.Parse(srv.URL)
	if err := provider.Save(provider.Provider{ID: "oa", Name: "OpenAI", Key: "ko",
		Keys: []provider.KeyAccount{{Key: "other-key"}}, Chat: "https://api.openai.com/v1",
		Responses: "https://api.openai.com/v1", Models: []string{"gpt-6.1-sol"}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("oa", "https://api.openai.com/v1", []catalog.Model{{ID: "gpt-6.1-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max"}}}); err != nil {
		t.Fatal(err)
	}
	cfg := settings.Load()
	cfg.AccountEfforts = map[string]string{
		"oa/" + provider.KeyID("ko"):        "xhigh",
		"oa/" + provider.KeyID("other-key"): "max",
		"oa/unknown-key":                    "bogus", // unknown and invalid: changes nothing
	}
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	if got := provider.AccountEffort(accountEffortProvider(t, "oa")); got != "xhigh" {
		t.Fatalf("the primary key's override = %q, want xhigh", got)
	}
	s := New()
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme, r.URL.Host = to.Scheme, to.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
	asked := func(model string) string {
		t.Helper()
		postFrom(t, s, "claude", "/v1/chat/completions", `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		e, _ := up.effort.Load().(string)
		return e
	}
	// a direct request takes the selected key's override
	if got := asked("oa/gpt-6.1-sol"); got != "xhigh" {
		t.Fatalf("direct request sent %q, want xhigh", got)
	}
	// and so does a request naming a group of it, recorded as fixed
	if err := provider.SaveGroup(provider.Group{ID: "fixed", Name: "Fixed", Members: []string{"oa/gpt-6.1-sol"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	if got := asked("group/fixed"); got != "xhigh" {
		t.Fatalf("group request sent %q, want xhigh", got)
	}
	r := lastRoute(s)
	if r.Tries[len(r.Tries)-1].Fixed != "xhigh" {
		t.Fatalf("the trace didn't record the account effort as fixed: %+v", r.Tries)
	}

	// the primary key fails through its retries and patience: the fallback
	// try is rebuilt from the original request and takes the second key's
	// own override, never the first key's xhigh
	up.failN["Bearer ko"] = -1
	up.effort.Store("")
	postFrom(t, s, "claude", "/v1/chat/completions", `{"model":"oa/gpt-6.1-sol","messages":[{"role":"user","content":"hi again"}]}`)
	if got, _ := up.effort.Load().(string); got != "max" {
		t.Fatalf("the fallback inherited the first key's effort: %q, want max", got)
	}
	// an invalid or absent override changes nothing
	delete(cfg.AccountEfforts, "oa/"+provider.KeyID("other-key"))
	cfg.AccountEfforts["oa/"+provider.KeyID("ko")] = "nonsense"
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	up.failN = map[string]int{}
	postFrom(t, s, "claude", "/v1/chat/completions", `{"model":"oa/gpt-6.1-sol","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	if got, _ := up.effort.Load().(string); got != "low" {
		t.Fatalf("an invalid override replaced the request's effort: %q", got)
	}
}

func accountEffortProvider(t *testing.T, id string) provider.Provider {
	t.Helper()
	p, err := provider.Find(id)
	if err != nil {
		t.Fatal(err)
	}
	return *p
}
