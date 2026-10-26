package provider

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func TestPeerRefreshFreezesLegacySelectionBeforePublishing(t *testing.T) {
	peerSyncHome(t)
	p := normalize(Provider{ID: "peer-home", Peer: "home", Name: "My name", Preset: RemoteMagpiePreset, Chat: "http://peer.invalid/v1"})
	// The old CLI stored an implicit all selection. Bypass Save only to
	// recreate that on-disk legacy state, before a new remote model appears.
	if err := storeInitial(file{Providers: []Provider{p}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "fake/m1"}, {ID: "fake/m2"}}); err != nil {
		t.Fatal(err)
	}
	old := catalog.Changed
	t.Cleanup(func() { catalog.Changed = old })
	checks := 0
	catalog.Changed = func() {
		checks++
		got := peerSyncStored(t, p.ID)
		if got.Name != "My name" || !slices.Equal(got.Models, []string{"fake/m1", "fake/m2"}) {
			t.Error("refresh published a catalog before freezing the old selection")
		}
		for _, m := range got.Exposed() {
			if m.ID == "fake/m3" {
				t.Error("new remote model was enabled during refresh")
			}
		}
	}
	if err := RefreshPeerCatalog("home", "Remote name", []catalog.Model{{ID: "fake/m1"}, {ID: "fake/m2"}, {ID: "fake/m3"}}); err != nil {
		t.Fatal(err)
	}
	if checks < 2 {
		t.Fatal("selection commit and catalog publication were not both observed")
	}
	// A later ordinary edit must not turn that frozen selection into all.
	p = peerSyncStored(t, p.ID)
	p.Off = true
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
}

func TestPeerRefreshKeepsPicksAfterSelectNone(t *testing.T) {
	peerSyncHome(t)
	p := Provider{ID: "peer-home", Peer: "home", Preset: RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}, NoModels: true}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if err := RefreshPeerCatalog("home", "Home", []catalog.Model{{ID: "fake/m1"}, {ID: "fake/m2"}}); err != nil {
		t.Fatal(err)
	}
	got := peerSyncStored(t, p.ID)
	if got.NoModels || !slices.Equal(got.Models, p.Models) {
		t.Fatal("selecting a model after Select none was lost")
	}
}

func TestPeerRefreshRefusesRenamedOwner(t *testing.T) {
	peerSyncHome(t)
	// Recreate the invalid record an older GUI's accepted rename left.
	p := normalize(Provider{ID: "renamed", Peer: "home", Was: []string{"peer-home"}, Preset: RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}})
	if err := storeInitial(file{Providers: []Provider{p}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := RefreshPeerCatalog("home", "Home", []catalog.Model{{ID: "fake/m1"}}); err == nil {
		t.Fatal("refresh accepted a provider's former id")
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("refresh created another provider beside the renamed one")
	}
}

func TestPeerOwnedIDsCannotBeRenamed(t *testing.T) {
	for _, kind := range []string{"tunnel", "synced-api"} {
		t.Run(kind, func(t *testing.T) {
			peerSyncHome(t)
			id := "peer-home"
			if kind == "synced-api" {
				var err error
				id, err = ImportPeerConfig("home", peerSyncConfig(), true)
				if err != nil {
					t.Fatal(err)
				}
			} else if err := Save(Provider{ID: id, Peer: "home", Name: "Home", Preset: RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(Path())
			if err != nil {
				t.Fatal(err)
			}
			if err := Rename(id, "renamed"); err == nil {
				t.Fatal("renamed a peer-owned provider")
			}
			after, err := os.ReadFile(Path())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected rename changed the provider file")
			}
			if err := Rename(id, id); err != nil {
				t.Fatalf("keeping the same ID was refused: %v", err)
			}
		})
	}
}

func TestPeerProviderStoresWithoutKeysAndKeepsExplicitModels(t *testing.T) {
	peerSyncHome(t)
	p := Provider{ID: "peer-home", Peer: "home", Name: "Home", Preset: RemoteMagpiePreset,
		Chat: "http://peer.invalid/v1", Models: []string{"claude/allowed"}, PickedFrom: []string{"claude/allowed"}}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "claude/allowed"}, {ID: "claude/new"}}); err != nil {
		t.Fatal(err)
	}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Find(p.ID)
	if err != nil || got.Key != "" || !got.Ready() {
		t.Fatalf("keyless peer cannot serve: provider=%+v err=%v", got, err)
	}
	if models := got.Exposed(); len(models) != 1 || models[0].ID != "claude/allowed" {
		t.Fatalf("peer model scope expanded: %+v", models)
	}
	for _, tc := range []struct {
		name string
		edit func(*Provider)
	}{
		{"different owner", func(p *Provider) { p.ID = "local" }},
		{"external URL", func(p *Provider) { p.Chat = "https://api.example.test/v1" }},
		{"API key", func(p *Provider) { p.Key = "secret" }},
		{"catalog URL", func(p *Provider) { p.ModelsURL = "http://127.0.0.1:9999/models" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := *got
			tc.edit(&bad)
			if err := Save(bad); err == nil {
				t.Fatal("invalid peer provider saved")
			}
		})
	}
}

func TestPeerSyncRejectsWildcardsAndOversizedModelNames(t *testing.T) {
	peerSyncHome(t)
	for _, model := range []string{"*", "claude-?", strings.Repeat("m", 513)} {
		cfg := peerSyncConfig()
		cfg.Models = []string{model}
		if _, err := ImportPeerConfig("home-link", cfg, true); err == nil {
			t.Fatalf("imported invalid model selection of length %d", len(model))
		}
	}
	if ps, err := Stored(); err != nil || len(ps) != 0 {
		t.Fatalf("invalid selection changed stored providers: %d, %v", len(ps), err)
	}
	// Pairing ids must also make valid generated provider ids.
	if _, err := ImportPeerConfig("home--link", peerSyncConfig(), true); err == nil {
		t.Fatal("accepted a link id incompatible with provider ids")
	}
}
