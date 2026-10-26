package gui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

func peerGUIRequest(h http.Handler, method, path string, value any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(value)
	r := httptest.NewRequest(method, "http://127.0.0.1:3430/api/peers"+path, bytes.NewReader(b))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:3430")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPeerGUIEditorKeepsSecretsAndOtherDevices(t *testing.T) {
	sandboxHome(t)
	h := Handler(nil, nil)
	draft := peerGUIRequest(h, "POST", "/draft", struct{}{})
	var code struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if draft.Code != http.StatusOK || json.Unmarshal(draft.Body.Bytes(), &code) != nil || len(code.Token) != 64 || !peering.ValidID(code.ID) {
		t.Fatalf("create pairing: %d %s", draft.Code, draft.Body)
	}
	if _, err := os.Stat(peering.Path()); !os.IsNotExist(err) {
		t.Fatal("opening or cancelling a draft wrote configuration")
	}
	p := peering.Peer{ID: code.ID, Name: "Office", Token: code.Token}
	save := func(p peering.Peer, revision string) *httptest.ResponseRecorder {
		return peerGUIRequest(h, "POST", "/save", map[string]any{"peer": p, "revision": revision})
	}
	if w := save(p, ""); w.Code != http.StatusOK {
		t.Fatalf("save new device: %d %s", w.Code, w.Body)
	}
	c, err := peering.Load(peering.Path())
	if err != nil || len(c.Peers) != 1 || !reflect.DeepEqual(c.Peers[0], p) {
		t.Fatal("creating a pairing added permissions or failed to persist")
	}
	read := peerGUIRequest(h, "GET", "", nil)
	var view struct {
		Peers []peerView `json:"peers"`
	}
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &view) != nil || len(view.Peers) != 1 || strings.Contains(read.Body.String(), code.Token) || view.Peers[0].Token != "" {
		t.Fatal("device list failed or disclosed a pairing token")
	}
	revision := view.Peers[0].Revision
	// A second window adds another link after this editor loaded.
	other := peering.Peer{ID: "home", Token: strings.Repeat("x", 64)}
	if w := save(other, ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	p.Name, p.Token, p.Import = "Office renamed", "", true
	if w := save(p, revision); w.Code != 200 {
		t.Fatalf("save existing device: %d %s", w.Code, w.Body)
	}
	c, err = peering.Load(peering.Path())
	if err != nil || len(c.Peers) != 2 || c.Peers[0].Token != code.Token || c.Peers[0].Name != p.Name || !reflect.DeepEqual(c.Peers[1], other) {
		t.Fatal("editing one device dropped its secret or the other device")
	}
	before, _ := os.ReadFile(peering.Path())
	if w := save(p, revision); w.Code != http.StatusBadRequest {
		t.Fatal("stale editor overwrote the device")
	}
	if w := save(p, ""); w.Code != http.StatusBadRequest {
		t.Fatal("joining a duplicate pairing replaced its existing permissions")
	}
	after, _ := os.ReadFile(peering.Path())
	if !bytes.Equal(before, after) {
		t.Fatal("rejected saves changed the file")
	}
	current := peering.Revision(c.Peers[0])
	request := map[string]string{"id": p.ID, "revision": current}
	reveal := peerGUIRequest(h, "POST", "/pairing", request)
	if reveal.Code != 200 || !strings.Contains(reveal.Body.String(), code.Token) || reveal.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("explicit pairing code did not return the private, uncached code")
	}
	if w := peerGUIRequest(h, "POST", "/remove", request); w.Code != 200 {
		t.Fatal(w.Body)
	}
	c, _ = peering.Load(peering.Path())
	if len(c.Peers) != 1 || !reflect.DeepEqual(c.Peers[0], other) {
		t.Fatal("removing one link changed another")
	}
}

func TestPeerGUIRefusesUnreadableOrInvalidPolicy(t *testing.T) {
	sandboxHome(t)
	h := Handler(nil, nil)
	p := peering.Peer{ID: "office", Token: strings.Repeat("x", 64)}
	for _, url := range []string{"ws://192.0.2.1", "wss://user:pass@example.com", "wss://example.com/?token=bad"} {
		p.URL = url
		if w := peerGUIRequest(h, "POST", "/save", map[string]any{"peer": p}); w.Code != http.StatusBadRequest {
			t.Fatalf("unsafe address accepted: %s", url)
		}
	}
	p.URL = ""
	if err := peering.Save(peering.Config{Version: 1, Peers: []peering.Peer{p}}); err != nil {
		t.Fatal(err)
	}
	broken := []byte("{broken")
	if err := os.WriteFile(peering.Path(), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "/save", "/remove", "/pairing"} {
		method := "POST"
		var value any = map[string]string{"id": p.ID, "revision": peering.Revision(p)}
		if path == "" {
			method, value = "GET", nil
		} else if path == "/save" {
			value = map[string]any{"peer": p}
		}
		if w := peerGUIRequest(h, method, path, value); w.Code != http.StatusBadRequest {
			t.Fatalf("unreadable policy accepted by %s: %d", path, w.Code)
		}
	}
	if after, _ := os.ReadFile(peering.Path()); !bytes.Equal(broken, after) {
		t.Fatal("unreadable policy was overwritten")
	}
}

func TestPeerGUIControlsStayLocal(t *testing.T) {
	sandboxHome(t)
	h := Handler(nil, nil)
	for _, path := range []string{"", "/draft", "/save", "/pairing", "/remove", "/status", "/refresh", "/sync"} {
		method := "POST"
		if path == "" || path == "/status" {
			method = "GET"
		}
		for _, attack := range []string{"remote", "forwarded", "rebound", "origin", "null", "frame", "cross-site"} {
			r := httptest.NewRequest(method, "http://localhost:3430/api/peers"+path, strings.NewReader("{}"))
			r.RemoteAddr = "127.0.0.1:1234"
			r.Header.Set("Content-Type", "application/json")
			switch attack {
			case "remote":
				r.RemoteAddr = "203.0.113.1:1234"
			case "forwarded":
				r.Header.Set("X-Forwarded-For", "203.0.113.1")
			case "rebound":
				r.Host = "attacker.example"
			case "origin":
				r.Header.Set("Origin", "http://localhost:9999")
			case "null":
				r.Header.Set("Origin", "null")
			case "frame":
				r.Header.Set("Sec-Fetch-Dest", "iframe")
			case "cross-site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s reached %s: %d", attack, path, w.Code)
			}
		}
	}
	// Wails removes the URL scheme and supplies TEST-NET until ownWindow
	// marks the native transport. The HTTP callers above cannot forge it.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://wails.localhost/api/peers", nil)
	if ownWindow(h).ServeHTTP(w, r); w.Code != 200 {
		t.Fatalf("native window refused: %d %s", w.Code, w.Body)
	}
}

func TestPeerGUIChoicesRespectSharingAndCopyBoundaries(t *testing.T) {
	sandboxHome(t)
	p := provider.Provider{ID: "api", Name: "Local API", Chat: "https://api.invalid/v1", Key: "private-test-key", Models: []string{"m1"}}
	remote := provider.Provider{ID: "peer-office", Name: "Remote", Peer: "office", Preset: provider.RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"secret/m2"}}
	for _, p := range []provider.Provider{p, remote} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
		if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: p.Models[0]}}); err != nil {
			t.Fatal(err)
		}
	}
	w := peerGUIRequest(Handler(nil, nil), "GET", "", nil)
	var v struct {
		Providers []peerChoice `json:"providers"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("provider choices: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), p.Key) || strings.Contains(w.Body.String(), "secret/m2") {
		t.Fatal("choices expose credentials or allow re-exporting remote models")
	}
	found := false
	for _, choice := range v.Providers {
		if choice.ID == p.ID {
			found = choice.CanSync && reflect.DeepEqual(choice.Models, p.Models)
		}
	}
	if !found {
		t.Fatal("local API models cannot be selected for copying")
	}
}

func TestPeerGUIUsesTheRunningGateway(t *testing.T) {
	sandboxHome(t)
	type received struct{ path, body string }
	arrived := make(chan received, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("GUI forwarded browser credentials or headers")
		}
		b, _ := io.ReadAll(r.Body)
		arrived <- received{r.URL.Path, string(b)}
		writeJSON(w, map[string]string{"id": "sync-office-api"})
	}))
	defer server.Close()
	t.Setenv("MAGPIE_ADDR", strings.TrimPrefix(server.URL, "http://"))
	h := Handler(nil, nil)
	w := peerGUIRequest(h, "POST", "/sync", map[string]string{"peer": "office", "provider": "api", "revision": "test-revision"})
	if w.Code != 200 {
		t.Fatalf("GUI did not reach the actual daemon: %d %s", w.Code, w.Body)
	}
	got := <-arrived
	if got.path != "/v1/peers/sync" || got.body != `{"peer":"office","provider":"api","revision":"test-revision"}` || !strings.Contains(w.Body.String(), "sync-office-api") {
		t.Fatalf("GUI changed the sync request or response: %+v %s", got, w.Body)
	}
}
