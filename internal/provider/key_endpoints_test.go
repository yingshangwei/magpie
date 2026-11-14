package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/netproxy"
)

func endpointPool() Provider {
	return Provider{ID: "pool", Name: "Pool", Key: "agk_one", KeyName: "one", Anthropic: "https://astergate.cc", Chat: "https://default.test/v1", ModelsURL: "https://default.test/catalog", Proxy: "direct", Keys: []KeyAccount{{Name: "two", Key: "agk_two"}}, AccountEndpoints: map[string]KeyEndpoints{keyID("agk_two"): {Anthropic: "https://beta.astergate.cc"}}, AccountProxies: map[string]string{keyID("agk_two"): "http://127.0.0.1:6155"}}
}

func TestKeyEndpointIsolation(t *testing.T) {
	p := endpointPool()
	q := p.WithKey(p.Keys[0])
	if q.Anthropic != "https://beta.astergate.cc" || q.Chat != "" || q.ModelsURL != "" {
		t.Fatalf("second key inherited another site's endpoints: %+v", KeyEndpoints{Chat: q.Chat, Anthropic: q.Anthropic, ModelsURL: q.ModelsURL})
	}
	if q.Key != "agk_two" || q.ProxyChoice() != "http://127.0.0.1:6155" {
		t.Fatal("key or proxy lost")
	}
	if q := p.WithKey(p.first()); q.Anthropic != p.Anthropic || q.Chat != p.Chat || q.ProxyChoice() != "direct" {
		t.Fatal("first key changed")
	}
}

func TestKeyEndpointEditsAndAliases(t *testing.T) {
	keyLimitsHome(t)
	p := endpointPool()
	p.KeyAliases = map[string]string{"old-two": keyID("agk_two")}
	p.Was = []string{"old-two"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	q, m, ok := Resolve("old-two/claude-opus-5-5")
	if !ok || m != "claude-opus-5-5" || q.Key != "agk_two" || len(q.KeysOn()) != 1 || q.Anthropic != "https://beta.astergate.cc" {
		t.Fatal("old reference did not stay on its account")
	}
	if q.RestKey(q.KeyList()[0]) != "pool#"+keyID("agk_two") {
		t.Fatal("alias changed the key's cooldown identity")
	}
	if err := Save(q); err == nil {
		t.Fatal("alias view overwrote the owning pool")
	}
	if RenamedRef("old-two/claude-opus-5-5") != "old-two/claude-opus-5-5" {
		t.Fatal("old account reference became the whole pool")
	}
	if err := UseKey(p.ID, keyID("agk_two")); err != nil {
		t.Fatal(err)
	}
	p2, _ := Find(p.ID)
	if p2.WithKey(p2.first()).Anthropic != "https://beta.astergate.cc" {
		t.Fatal("promotion lost endpoint")
	}
	_, now, err := ReplaceKey(p.ID, keyID("agk_two"), "agk_replaced")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ = Find(p.ID)
	if p2.AccountEndpoints[now].Anthropic != "https://beta.astergate.cc" || p2.KeyAliases["old-two"] != now {
		t.Fatal("replacement lost endpoint or alias")
	}
	if err := SetKeyEndpoints(p.ID, now, KeyEndpoints{Anthropic: "https://user:pass@wrong.test"}); err == nil {
		t.Fatal("accepted credentials in URL")
	}
	if err := SetKeyEndpoints(p.ID, now, KeyEndpoints{}); err != nil {
		t.Fatal(err)
	}
	p2, _ = Find(p.ID)
	if p2.WithKey(p2.first()).Anthropic != p.Anthropic {
		t.Fatal("clear did not restore inheritance")
	}
	if err := UseKey(p.ID, keyID("agk_one")); err != nil {
		t.Fatal(err)
	}
	if err := SetKeyOn(p.ID, now, false); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Resolve("old-two/claude-opus-5-5"); ok {
		t.Fatal("disabled alias fell through to another account")
	}
	if err := RemoveKey(p.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := Resolve("old-two/claude-opus-5-5"); ok {
		t.Fatal("removed alias fell through to another account")
	}
}

type endpointTransports map[string]http.RoundTripper

func (ts endpointTransports) RoundTrip(r *http.Request) (*http.Response, error) {
	if tr := ts[r.URL.Host]; tr != nil {
		return tr.RoundTrip(r)
	}
	return nil, fmt.Errorf("unexpected destination %s", r.URL.Host)
}

func TestKeyEndpointQuotaSites(t *testing.T) {
	keyLimitsHome(t)
	a := asterGateServerAt(t, "https://astergate.cc", "agk_one")
	ta := http.DefaultClient.Transport
	b := asterGateServerAt(t, "https://beta.astergate.cc", "agk_two")
	tb := http.DefaultClient.Transport
	http.DefaultClient.Transport = endpointTransports{"astergate.cc": ta, "beta.astergate.cc": tb}
	p := endpointPool()
	p.Chat = ""
	p.AccountProxies = nil
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	qs := PlanQuotas(context.Background())
	if len(qs) != 2 {
		t.Fatalf("got %d quota cards, want 2", len(qs))
	}
	for _, q := range qs {
		if q.Error != "" || len(q.Windows) != 3 {
			t.Fatalf("account %s: %s, windows=%d", q.User, q.Error, len(q.Windows))
		}
	}
	if a.reports.Load() != 1 || b.reports.Load() != 1 || a.logouts.Load() != 1 || b.logouts.Load() != 1 {
		t.Fatal("quota reads did not use and close each key's own site")
	}
}

func TestKeyEndpointModelSites(t *testing.T) {
	keyLimitsHome(t)
	var calls [2]atomic.Int32
	servers := make([]*httptest.Server, 2)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.URL.Path != "/v1/models" && r.URL.Path != "/models") || r.Header.Get("Authorization") != "Bearer agk_"+[]string{"one", "two"}[i] {
				t.Errorf("model fetch crossed account/site boundary: path=%s authOK=%v", r.URL.Path, r.Header.Get("Authorization") == "Bearer agk_"+[]string{"one", "two"}[i])
				w.WriteHeader(403)
				return
			}
			calls[i].Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":"model-%d"}]}`, i)
		}))
		defer servers[i].Close()
	}
	p := endpointPool()
	p.Anthropic = servers[0].URL
	p.Chat = ""
	p.ModelsURL = ""
	p.AccountProxies = nil
	p.AccountEndpoints[keyID("agk_two")] = KeyEndpoints{Anthropic: servers[1].URL}
	for i, k := range p.KeysOn() {
		q := p.WithKey(k)
		got, err := q.fetchOne(context.Background())
		if err != nil || len(got.models) != 1 || !strings.HasSuffix(got.models[0].ID, fmt.Sprint(i)) {
			t.Fatalf("key %d model discovery failed: %v", i, err)
		}
	}
	if calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatal("each site's catalog was not queried once")
	}
}

// Catalog refresh uses each key's proxy as well as its endpoint, even when
// only one key remains or the other key becomes the primary account.
func TestKeyEndpointCatalogProxies(t *testing.T) {
	keyLimitsHome(t)
	old := http.DefaultClient.Transport
	tr := &http.Transport{}
	http.DefaultClient.Transport = endpointProxyTransport{tr}
	t.Cleanup(func() { http.DefaultClient.Transport = old; tr.CloseIdleConnections() })
	var calls [2]atomic.Int32
	proxies := make([]*httptest.Server, 2)
	for i := range proxies {
		proxies[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != fmt.Sprintf("site-%d.invalid", i) || r.Header.Get("Authorization") != fmt.Sprintf("Bearer key-%d", i) {
				t.Error("catalog request used the other account's proxy, key or site")
				w.WriteHeader(403)
				return
			}
			calls[i].Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"data":[{"id":"model-%d"}]}`, i)
		}))
		defer proxies[i].Close()
	}
	p := Provider{ID: "endpoint-proxies", Name: "Endpoint proxies", Key: "key-0", Chat: "http://site-0.invalid/v1", ModelsURL: "http://site-0.invalid/v1/models", Keys: []KeyAccount{{Key: "key-1"}},
		AccountEndpoints: map[string]KeyEndpoints{keyID("key-1"): {Chat: "http://site-1.invalid/v1", ModelsURL: "http://site-1.invalid/v1/models"}},
		AccountProxies:   map[string]string{keyID("key-0"): proxies[0].URL, keyID("key-1"): proxies[1].URL},
	}
	ms, err := p.Fetch(context.Background())
	if err != nil || len(ms) != 2 || calls[0].Load() != 1 || calls[1].Load() != 1 {
		t.Fatalf("pooled catalogs: models=%d err=%v calls=%d/%d", len(ms), err, calls[0].Load(), calls[1].Load())
	}
	p.Key = "key-1"
	p.Keys = nil
	ms, err = p.Fetch(context.Background())
	if err != nil || len(ms) != 1 || ms[0].ID != "model-1" || calls[0].Load() != 1 || calls[1].Load() != 2 {
		t.Fatalf("promoted single account: models=%v err=%v calls=%d/%d", ms, err, calls[0].Load(), calls[1].Load())
	}
}

// Redirect to the selected local proxy without ever resolving the fake vendor.
// The normal proxy selector still reads the actual outgoing request context.
type endpointProxyTransport struct{ next http.RoundTripper }

func (tr endpointProxyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	proxy, err := netproxy.Func(r)
	if err != nil {
		return nil, err
	}
	if proxy == nil {
		return nil, fmt.Errorf("catalog request lost its account proxy")
	}
	q := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = proxy.Scheme, proxy.Host
	q.URL = &u
	q.Host = r.URL.Host
	return tr.next.RoundTrip(q)
}
