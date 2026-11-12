package agent

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// A synthetic upstream refresh changes only a status suffix in one name.
// The model ids and the serving providers stay the same throughout.
func TestClaudePickerStatusRefreshKeepsNamesAndSourceMarkers(t *testing.T) {
	syncHome(t)
	if err := edit.SetJSON(settings.Path(), edit.KV{Path: "claudeMinimalModelPicker", Value: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-a", "source-b"} {
		if err := catalog.SaveLive(id, "", []catalog.Model{{ID: "fixture-flash", Name: "Fixture Flash"}}); err != nil {
			t.Fatal(err)
		}
		if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "fixture", Chat: "http://127.0.0.1:1/v1", Models: []string{"fixture-flash"}}); err != nil {
			t.Fatal(err)
		}
	}
	before, minimal, err := claudePickerLabels()
	if err != nil || !minimal {
		t.Fatalf("initial picker: minimal=%t, err=%v", minimal, err)
	}
	for _, id := range []string{"source-a/fixture-flash", "source-b/fixture-flash"} {
		if !strings.HasPrefix(before[id], "Fixture Flash v") {
			t.Fatalf("fixture must start with disambiguated source markers: %s=%q", id, before[id])
		}
	}
	if err := catalog.SaveLive("source-a", "", []catalog.Model{{ID: "fixture-flash", Name: "Fixture Flash ⚠️ 93%"}}); err != nil {
		t.Fatal(err)
	}
	catalog.Touched()
	after, _, err := claudePickerLabels()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"source-a/fixture-flash", "source-b/fixture-flash"} {
		if after[id] != before[id] {
			t.Errorf("status refresh changed a stable display name or source marker: %s: %q -> %q", id, before[id], after[id])
		}
	}
}

func TestClaudePickerNamesKeepSourceMarkersAcrossSpellings(t *testing.T) {
	syncHome(t)
	if err := edit.SetJSON(settings.Path(), edit.KV{Path: "claudeMinimalModelPicker", Value: true}); err != nil {
		t.Fatal(err)
	}
	for id, name := range map[string]string{"a": "DeepSeek V4.1 Flash", "b": "DeepSeek-V4.1-Flash", "c": "deepseek_v4.1_flash"} {
		if err := catalog.SaveLive(id, "", []catalog.Model{{ID: "deepseek-v4.1-flash", Name: name}}); err != nil {
			t.Fatal(err)
		}
		if err := provider.Save(provider.Provider{ID: id, Name: id, Key: "fixture", Chat: "http://127.0.0.1:1/v1", Models: []string{"deepseek-v4.1-flash"}}); err != nil {
			t.Fatal(err)
		}
	}
	labels, _, err := claudePickerLabels()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if !strings.Contains(labels[id+"/deepseek-v4.1-flash"], " v") {
			t.Errorf("source %s lost its marker: %v", id, labels)
		}
	}
}

func TestClaudePickerNameKeepsUserNames(t *testing.T) {
	for _, e := range []provider.Entry{
		{Name: "My model ⚠️ 93%", Default: "upstream"},
		{Name: "My group ⚠️ 93%", Group: "chosen", Named: true},
		{Name: "Model 93%"},
		{Name: "Model ⚠️ experimental"},
	} {
		if got := claudePickerName(e); got != e.Name {
			t.Errorf("user or non-status name changed: %q -> %q", e.Name, got)
		}
	}
}

func TestClaudePickerFoundGroupKeepsItsCatalogName(t *testing.T) {
	syncHome(t)
	writeFile(t, catalog.CachePath(), `{"deepseek":{"models":{"deepseek-v4.1-flash":{"id":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash"}}}}`)
	catalog.Reset()
	t.Cleanup(catalog.Reset)
	for _, name := range []string{"DeepSeek V4.1 Flash", "deepseek-v4.1-flash ⚠️ 93%", "DeepSeek-V4.1-Flash"} {
		e := provider.Entry{Model: "deepseek-v4.1-flash", Name: name, Group: "auto-deepseek-v4-1-flash"}
		if got := claudePickerName(e); got != "DeepSeek V4.1 Flash" {
			t.Errorf("a found group's member changed its display name: %q -> %q", name, got)
		}
	}
}
