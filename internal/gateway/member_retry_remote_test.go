package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestMemberRetryRemoteUsesSavedAddress(t *testing.T) {
	fresh(t)
	up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"sol","choices":[{"index":0,"message":{"role":"assistant","content":"from the new address"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	})))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"sol"}, Chat: up.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	_, secrets := newCaller(t, "B")
	h := New().Handler()
	remote := httptest.NewServer(h)
	t.Cleanup(remote.Close)
	id, err := provider.Add(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Key: secrets[0], Chat: strings.TrimPrefix(remote.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	office, _ := provider.Find(id)
	if _, err := office.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the old address answers busy, and the user types the new one in
	// before the retry
	var oldHits atomic.Int32
	old := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oldHits.Add(1) == 1 {
			q, _ := provider.Find(id)
			q.Chat, q.Responses, q.Anthropic, q.Decide = remote.URL, "", "", ""
			if err := provider.Save(*q); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"overloaded"}}`)
	})))
	t.Cleanup(old.Close)
	office, _ = provider.Find(id)
	office.Chat, office.Responses, office.Anthropic, office.Decide = old.URL, "", "", ""
	if err := provider.Save(*office); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "g", Name: "G", Members: []string{"office/fake/sol"},
		Routing: provider.Ordered, Affinity: provider.AffinityOff,
		RetryBeforeFallback: &provider.RetryPolicy{Count: 1, Interval: 30, MaxWait: 60}}); err != nil {
		t.Fatal(err)
	}
	// Saving a new address wakes the policy pause before its 30-second interval.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "from the new address") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if n := oldHits.Load(); n != 1 {
		t.Errorf("the old address was asked %d times, want once", n)
	}
}
