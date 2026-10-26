package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

func TestPeerGUIRejectsIDChangeBeforeSavingOtherFields(t *testing.T) {
	for _, kind := range []string{"tunnel", "synced-api"} {
		t.Run(kind, func(t *testing.T) {
			sandboxHome(t)
			provider.Changed()
			id := "peer-office"
			if kind == "synced-api" {
				var err error
				id, err = provider.ImportPeerConfig("office", provider.PeerProviderConfig{SourceID: "relay", Name: "API", Chat: "http://127.0.0.1:1/v1", Key: "test-only", Models: []string{"m1"}}, true)
				if err != nil {
					t.Fatal(err)
				}
			} else if err := provider.Save(provider.Provider{ID: id, Name: "Office", Peer: "office", Preset: provider.RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}}); err != nil {
				t.Fatal(err)
			}
			p, err := provider.Find(id)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(provider.Path())
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			save := func(to, name string) *httptest.ResponseRecorder {
				body, err := json.Marshal(map[string]any{"id": to, "from": p.ID, "name": name, "preset": p.Preset, "key": "", "chat": p.Chat, "responses": p.Responses, "anthropic": p.Anthropic, "models": p.Models, "headers": map[string]string{}})
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/save", bytes.NewReader(body)))
				return w
			}
			if w := save("renamed", "Must not be saved"); w.Code != http.StatusBadRequest {
				t.Fatalf("GUI accepted changing a peer-owned ID: %d", w.Code)
			}
			after, err := os.ReadFile(provider.Path())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected rename changed the saved provider")
			}
			if w := save(p.ID, "Edited name"); w.Code != http.StatusOK {
				t.Fatalf("display name edit was refused: %d %s", w.Code, w.Body.String())
			}
			if got, err := provider.Find(p.ID); err != nil || !got.Ready() || got.Name != "Edited name" {
				t.Fatalf("provider is no longer editable and ready: %v", err)
			}
		})
	}
}

func TestPeerGUIEditKeepsOwnership(t *testing.T) {
	for _, kind := range []string{"tunnel", "synced-api"} {
		t.Run(kind, func(t *testing.T) {
			sandboxHome(t)
			provider.Changed()
			p := provider.Provider{ID: "peer-office", Name: "Office (WS)", Peer: "office", Preset: provider.RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}}
			cfg := provider.PeerProviderConfig{SourceID: "relay", Name: "API", Chat: "http://127.0.0.1:1/v1", Key: "test-only", Models: []string{"m1"}}
			if kind == "synced-api" {
				id, err := provider.ImportPeerConfig("office", cfg, true)
				if err != nil {
					t.Fatal(err)
				}
				got, err := provider.Find(id)
				if err != nil {
					t.Fatal(err)
				}
				p = *got
			} else if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: p.Models[0]}}); err != nil {
				t.Fatal(err)
			}
			// Match the existing editor's save DTO: ownership is not a form field.
			body, err := json.Marshal(map[string]any{"id": p.ID, "from": p.ID, "name": "Edited name", "preset": p.Preset, "key": "", "chat": p.Chat, "responses": p.Responses, "anthropic": p.Anthropic, "models": p.Models, "headers": map[string]string{}})
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/save", strings.NewReader(string(body))))
			if w.Code != http.StatusOK {
				t.Fatalf("ordinary editor save rejected: %d %s", w.Code, w.Body.String())
			}
			after, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Name != "Edited name" || after.Peer != p.Peer || after.PeerSource != p.PeerSource || after.Key != p.Key {
				t.Fatalf("editor did not preserve ownership and key: name=%q peer=%q source=%q", after.Name, after.Peer, after.PeerSource)
			}
			if kind == "synced-api" {
				cfg.Name, cfg.Key, cfg.Models = "Updated copy", "updated-test-only", []string{"m2"}
				id, err := provider.ImportPeerConfig("office", cfg, true)
				if err != nil {
					t.Fatalf("sync after GUI edit: %v", err)
				}
				updated, err := provider.Find(id)
				if err != nil {
					t.Fatal(err)
				}
				if updated.PeerSource != p.PeerSource || updated.Name != cfg.Name || updated.Key != cfg.Key || !slices.Equal(updated.Models, cfg.Models) {
					t.Fatal("sync after GUI edit did not update the owned copy")
				}
			}
		})
	}
}
