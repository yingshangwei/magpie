package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

func TestPeerSyncRefusesAStaleGUIPreview(t *testing.T) {
	fresh(t)
	p := provider.Provider{ID: "api", Name: "API", Chat: "https://api.invalid/v1", Key: "test-only-key", Models: []string{"m1"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{SyncSend: []peering.Selection{{Provider: p.ID, Models: p.Models}}}, peering.Peer{SyncReceive: []string{p.ID}, ReceiveKeys: true})
	post := func(revision string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"peer": "office", "provider": p.ID, "revision": revision})
		r := httptest.NewRequest("POST", "http://127.0.0.1/v1/peers/sync", strings.NewReader(string(b)))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("User-Agent", "magpie/peer-gui")
		w := httptest.NewRecorder()
		lanGuard(pair.a.Handler()).ServeHTTP(w, r)
		return w
	}
	if w := post("outdated-preview"); w.Code != http.StatusConflict {
		t.Fatalf("stale preview was allowed to send: %d %s", w.Code, w.Body)
	}
	if _, err := provider.Find("sync-office-api"); err == nil {
		t.Fatal("stale sync created a provider on the receiver")
	}
	if w := post(peering.Revision(pair.aPolicy)); w.Code != http.StatusOK {
		t.Fatalf("current preview failed: %d %s", w.Code, w.Body)
	}
	copy, err := provider.Find("sync-office-api")
	if err != nil || copy.Key != "" || !copy.Off {
		t.Fatal("metadata-only preview transferred credentials or enabled the copy")
	}
	// Existing CLI callers omit a revision and retain their explicit
	// command's behavior; the GUI always supplies the policy it displayed.
	if w := post(""); w.Code != http.StatusOK {
		t.Fatalf("CLI sync changed: %d %s", w.Code, w.Body)
	}
}
