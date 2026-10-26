package cli

import (
	"bytes"
	"os"
	"slices"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/provider"
)

func peerCLIProvider(t *testing.T) provider.Provider {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	provider.Changed()
	p := provider.Provider{ID: "peer-office", Peer: "office", Name: "Office", Preset: provider.RemoteMagpiePreset, Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "fake/m1"}, {ID: "fake/m2"}}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPeerCLIAllKeepsKnownSelection(t *testing.T) {
	for _, all := range []string{"all", "-"} {
		t.Run(all, func(t *testing.T) {
			p := peerCLIProvider(t)
			if _, err := printed(t, func() error { return providerCmd([]string{"provider", "models", p.ID, all}) }); err != nil {
				t.Fatal(err)
			}
			got := mustFind(t, p.ID)
			if got.NoModels || !slices.Equal(got.Models, []string{"fake/m1", "fake/m2"}) {
				t.Fatalf("all did not save the known selection: models=%v none=%v", got.Models, got.NoModels)
			}
			if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "fake/m1"}, {ID: "fake/m2"}, {ID: "fake/m3"}}); err != nil {
				t.Fatal(err)
			}
			if got := mustFind(t, p.ID).Exposed(); len(got) != 2 {
				t.Fatalf("a newly listed model was automatically enabled: %v", got)
			}
		})
	}
}

func TestPeerCLIAllRefusesUnknownCatalog(t *testing.T) {
	p := peerCLIProvider(t)
	if err := os.Remove(catalog.LivePath(p.ID)); err != nil {
		t.Fatal(err)
	}
	provider.Changed()
	before, err := os.ReadFile(provider.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := printed(t, func() error { return providerCmd([]string{"provider", "models", p.ID, "all"}) }); err == nil {
		t.Fatal("all accepted an unreadable catalog as a selection")
	}
	after, err := os.ReadFile(provider.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed all changed the saved provider")
	}
}

func TestPeerCLIRejectsIDChangeBeforeSavingOtherFields(t *testing.T) {
	p := peerCLIProvider(t)
	before, err := os.ReadFile(provider.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := providerCmd([]string{"provider", "set", p.ID, "id=renamed", "name=Must not be saved"}); err == nil {
		t.Fatal("CLI accepted changing a peer-owned ID")
	}
	after, err := os.ReadFile(provider.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected rename changed the saved provider")
	}
	if _, err := printed(t, func() error { return providerCmd([]string{"provider", "set", p.ID, "name=Edited name"}) }); err != nil {
		t.Fatalf("display name edit was refused: %v", err)
	}
	if got := mustFind(t, p.ID); got.Name != "Edited name" || !got.Ready() {
		t.Fatalf("provider is not editable and ready: name=%q ready=%v", got.Name, got.Ready())
	}
}
