package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

func TestPeerRefreshWithUnreadableCatalog(t *testing.T) {
	for _, mode := range []string{"missing", "invalid-json"} {
		for _, selection := range []string{"explicit", "none", "legacy-implicit"} {
			t.Run(mode+"/"+selection, func(t *testing.T) {
				fresh(t)
				source := provider.Provider{ID: "fake", Key: "test-only", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1", "m2"}}
				if err := provider.Save(source); err != nil {
					t.Fatal(err)
				}
				pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "fake", Models: []string{"m1", "m2", "m3"}}}})
				p, err := provider.Find("peer-office")
				if err != nil {
					t.Fatal(err)
				}
				p.Name, p.Models = "My name", []string{"fake/m1"}
				if selection == "none" {
					p.Models, p.NoModels = nil, true
				}
				if err := provider.Save(*p); err != nil {
					t.Fatal(err)
				}
				if selection == "legacy-implicit" {
					// An older version's `models all` persisted implicit picks.
					data, err := os.ReadFile(provider.Path())
					if err != nil {
						t.Fatal(err)
					}
					var saved map[string]any
					if err := json.Unmarshal(data, &saved); err != nil {
						t.Fatal(err)
					}
					for _, item := range saved["providers"].([]any) {
						entry := item.(map[string]any)
						if entry["id"] == p.ID {
							delete(entry, "models")
							delete(entry, "noModels")
							delete(entry, "pickedFrom")
						}
					}
					data, err = json.Marshal(saved)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(provider.Path(), data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				cachePath := catalog.LivePath(p.ID)
				if mode == "missing" {
					err = os.Remove(cachePath)
				} else {
					err = os.WriteFile(cachePath, []byte("{broken"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
				provider.Changed()
				source.Models = []string{"m1", "m2", "m3"}
				if err := provider.Save(source); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(provider.Path())
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err = pair.a.refreshPeer(ctx, pair.aPolicy)
				if selection == "legacy-implicit" {
					if err == nil {
						t.Fatal("refresh accepted an unknown historical selection")
					}
					after, readErr := os.ReadFile(provider.Path())
					if readErr != nil || !bytes.Equal(before, after) {
						t.Fatal("unknown historical selection changed the provider file")
					}
					if _, _, ok := catalog.Live(p.ID); ok {
						t.Fatal("unknown selection was expanded by publishing the new catalog")
					}
					// Selecting exact models recovers without enabling the new m3.
					p, err = provider.Find(p.ID)
					if err != nil {
						t.Fatal(err)
					}
					p.Models, p.NoModels = []string{"fake/m1"}, false
					if err := provider.Save(*p); err != nil {
						t.Fatal(err)
					}
					err = pair.a.refreshPeer(ctx, pair.aPolicy)
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := provider.Find(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"fake/m1"}
				if selection == "none" {
					want = nil
				}
				if got.Name != "My name" || !slices.Equal(got.Models, want) || got.NoModels != (len(want) == 0) {
					t.Fatalf("refresh changed local choices: name=%q models=%v none=%v", got.Name, got.Models, got.NoModels)
				}
				if available := got.Available(); len(available) != 3 {
					t.Fatalf("catalog was not restored: %v", available)
				}
			})
		}
	}
}

func TestPeerRefreshKeepsConcurrentUserSave(t *testing.T) {
	fresh(t)
	// Inject the user's completed save after refresh has read its provider
	// and before refresh saves it. SaveLive calls this hook synchronously.
	oldChanged := catalog.Changed
	var armed atomic.Bool
	edited := make(chan error, 1)
	catalog.Changed = func() {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		p, err := provider.Find("peer-office")
		if err == nil {
			p.Name, p.Models, p.Off = "User edit completed", []string{"fake/m1"}, true
			err = provider.Save(*p)
			if err == nil {
				got, e := provider.Find(p.ID)
				if e != nil {
					err = e
				} else if got.Name != p.Name || !slices.Equal(got.Models, p.Models) {
					err = fmt.Errorf("user save was not persisted")
				}
			}
		}
		edited <- err
	}
	t.Cleanup(func() { catalog.Changed = oldChanged })
	if err := provider.Save(provider.Provider{ID: "fake", Key: "test-only", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "fake", Models: []string{"m1", "m2"}}}})
	armed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pair.a.refreshPeer(ctx, pair.aPolicy); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-edited:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("interleaved user save did not run")
	}
	got, err := provider.Find("peer-office")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "User edit completed" || !got.Off || !slices.Equal(got.Models, []string{"fake/m1"}) {
		t.Fatalf("refresh overwrote a completed user save: name=%q models=%v", got.Name, got.Models)
	}
}
