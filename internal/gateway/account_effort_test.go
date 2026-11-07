package gateway

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestAccountEffortDirectGroupAndFallback(t *testing.T) {
	s, up := fasted(t)
	p, err := provider.Find("oa")
	if err != nil {
		t.Fatal(err)
	}
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "gpt-6.1-sol", Efforts: levels}}); err != nil {
		t.Fatal(err)
	}
	cfg := settings.Load()
	cfg.AccountEfforts = map[string]string{p.ID + "/" + p.AccountID(): "xhigh"}
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	if err := provider.SaveGroup(provider.Group{ID: "fixed", Name: "Fixed", Members: []string{"oa/gpt-6.1-sol:low"}, Routing: provider.Ordered}); err != nil {
		t.Fatal(err)
	}
	asked := func(model string) string {
		return `{"model":"` + model + `","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`
	}
	effort := func() any {
		_, _, _, body := up.last(t)
		if r, ok := body["reasoning"].(map[string]any); ok {
			return r["effort"]
		}
		return body["reasoning_effort"]
	}
	for _, model := range []string{"oa/gpt-6.1-sol", "group/fixed"} {
		r := postFrom(t, s, "claude", "/v1/messages", asked(model))
		if effort() != "xhigh" || r.Tries[len(r.Tries)-1].Fixed != "xhigh" {
			t.Fatalf("%s: sent %v, trace %+v", model, effort(), r.Tries)
		}
	}

	// A different key in the same provider must keep the client's effort.
	// This also catches an override accidentally applied to the shared body
	// before routing, which would leak into the fallback.
	p.Keys = []provider.KeyAccount{{Key: "other-key"}}
	p.Routing = provider.Ordered
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	var rejectFirst atomic.Bool
	rejectFirst.Store(true)
	transport := s.client.Transport
	s.client.Transport = qoderRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer ko" && rejectFirst.Load() {
			return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"temporarily unavailable"}}`)), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})
	r := postFrom(t, s, "claude", "/v1/messages", asked("oa/gpt-6.1-sol"))
	if effort() != "max" || len(r.Tries) < 2 || r.Tries[len(r.Tries)-1].Fixed != "" {
		t.Fatalf("fallback inherited another key's override: effort=%v trace=%+v", effort(), r.Tries)
	}
	rejectFirst.Store(false)

	// The setting is keyed by provider as well as account.
	if err := catalog.SaveLive("rl", "https://relay.example/v1", []catalog.Model{{ID: "gpt-6.1-sol", Efforts: levels}}); err != nil {
		t.Fatal(err)
	}
	postFrom(t, s, "claude", "/v1/messages", asked("rl/gpt-6.1-sol"))
	if effort() != "max" {
		t.Fatalf("another provider was changed: %v", effort())
	}
}
