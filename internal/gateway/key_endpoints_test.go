package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// Old provider names must keep their original key and site, including their
// cooldown identity. A fixed group member must not expand the whole pool.
func TestKeyEndpointRoutes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	var seen []string
	var refuseFirst bool
	serve := func(name, key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key || r.URL.Path != "/v1/chat/completions" {
				t.Error("account key sent to the wrong site or path")
				w.WriteHeader(403)
				return
			}
			mu.Lock()
			seen = append(seen, name)
			fail := refuseFirst && name == "first"
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if fail {
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":{"message":"quota exhausted"}}`)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"choices":[{"message":{"role":"assistant","content":"The selected account answered."},"finish_reason":"stop"}]}`, name)
		}))
	}
	first, second, middle := serve("first", "key-one"), serve("second", "key-two"), serve("middle", "key-middle")
	defer first.Close()
	defer second.Close()
	defer middle.Close()
	one, two := provider.KeyID("key-one"), provider.KeyID("key-two")
	p := provider.Provider{ID: "endpoint-pool", Name: "Endpoint pool", Chat: first.URL + "/v1", Key: "key-one", KeyName: "first", Models: []string{"model"}, Proxy: "direct",
		Keys:             []provider.KeyAccount{{Key: "key-two", Name: "second"}},
		AccountEndpoints: map[string]provider.KeyEndpoints{two: {Chat: second.URL + "/v1"}},
		KeyAliases:       map[string]string{"old-endpoint-two": two}, Was: []string{"old-endpoint-two"},
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "endpoint-middle", Name: "Middle", Key: "key-middle", Chat: middle.URL + "/v1", Models: []string{"model"}, Proxy: "direct"}); err != nil {
		t.Fatal(err)
	}
	s := New()
	send := func(model string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "Explain what selects an API account."}}})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
		return rec
	}
	r := send("old-endpoint-two/model")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"second"`) {
		t.Fatalf("old route: %d %s", r.Code, r.Body.String())
	}
	q, _, ok := provider.Resolve("old-endpoint-two/model")
	if !ok {
		t.Fatal("alias disappeared")
	}
	cs, _, _, _ := perKeyEvery(q, "model", provider.Chat)
	if len(cs) != 1 || cs[0].rest != "endpoint-pool#"+two {
		t.Fatalf("alias did not preserve account cooldown: %+v", len(cs))
	}
	g := provider.Group{ID: "endpoint-order", Name: "Endpoint order", Members: []string{"endpoint-pool/model#" + one, "endpoint-middle/model", "endpoint-pool/model#" + two}, Routing: "order", Affinity: "off"}
	if err := provider.SaveGroup(g); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	seen = nil
	refuseFirst = true
	mu.Unlock()
	r = send("group/endpoint-order")
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"middle"`) {
		t.Fatalf("group: %d %s", r.Code, r.Body.String())
	}
	mu.Lock()
	calls := strings.Join(seen, ",")
	mu.Unlock()
	if calls != "first,middle" {
		t.Fatalf("group order changed: %s", calls)
	}
	if _, ok := RestOf("endpoint-pool#" + one); !ok {
		t.Fatal("first account did not rest")
	}
	if _, ok := RestOf("endpoint-pool#" + two); ok {
		t.Fatal("first account rested second account")
	}
	if err := provider.SetKeyOn(p.ID, two, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	seen = nil
	mu.Unlock()
	r = send("old-endpoint-two/model")
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if r.Code == 200 || n != 0 {
		t.Fatalf("disabled alias reached another key: status=%d requests=%d", r.Code, n)
	}
}
