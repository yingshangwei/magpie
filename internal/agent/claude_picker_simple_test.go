package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestClaudeMinimalPicker(t *testing.T) {
	home := syncHome(t)
	for _, id := range []string{"source-a", "source-b"} {
		p := provider.Provider{ID: id, Name: id + " CLI", Key: "fixture",
			Chat: "http://127.0.0.1:1/v1", Models: []string{"alpha", "beta"}}
		if id == "source-a" {
			p.Models = append(p.Models, "alpha-alias", "only")
		}
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
		for _, m := range p.Models {
			name := map[string]string{"alpha": "Alpha", "beta": "Beta", "alpha-alias": "Alpha", "only": "Only"}[m]
			if err := provider.SetModelName(id+"/"+m, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := provider.SaveGroup(provider.Group{ID: "choice", Name: "Alpha",
		Members: []string{"source-a/alpha", "source-b/alpha"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{"theme":"dark"}`)
	a := claude(home)
	if err := a.Apply("model", "source-a/alpha"); err != nil {
		t.Fatal(err)
	}
	type row struct{ Model, Label, Description string }
	read := func() (map[string]row, bool) {
		t.Helper()
		var p struct {
			Options               []row
			ReplaceBuiltInOptions bool
		}
		raw, _ := edit.GetJSON(path, "modelPicker")
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		rows := map[string]row{}
		for _, r := range p.Options {
			rows[r.Model] = r
		}
		return rows, p.ReplaceBuiltInOptions
	}
	before, _ := read()
	beforeModel, _ := edit.GetJSON(path, "model")
	if err := edit.SetJSON(settings.Path(), edit.KV{Path: "claudeMinimalModelPicker", Value: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	got, replaces := read()
	if !replaces {
		t.Fatal("simple picker still adds built-in and discovered rows")
	}
	if len(got) != len(before) {
		t.Fatalf("model routes changed: %d before, %d after", len(before), len(got))
	}
	for id := range before {
		r, ok := got[id]
		if !ok || r.Model != id || r.Description != claudePickerBlankDescription ||
			strings.Contains(r.Label, "CLI") || strings.Contains(r.Label, "magpie") {
			t.Errorf("route or minimal display for %s: %+v", id, r)
		}
	}
	suffix := func(id, name string) string {
		t.Helper()
		s := strings.TrimPrefix(got[id].Label, name+" ")
		if !strings.HasPrefix(s, "v") {
			t.Fatalf("%s lacks a source marker: %q", id, got[id].Label)
		}
		return s
	}
	sourceA, sourceB := suffix("source-a/alpha", "Alpha"), suffix("source-b/alpha", "Alpha")
	if sourceA == sourceB || suffix("source-a/beta", "Beta") != sourceA ||
		suffix("source-a/alpha-alias", "Alpha") != sourceA || suffix("source-b/beta", "Beta") != sourceB {
		t.Fatalf("markers are not tied to sources across models: %+v", got)
	}
	if group := suffix("group/choice", "Alpha"); group == sourceA || group == sourceB {
		t.Fatal("a routing group claims to be one of its providers")
	}
	if got["source-a/only"].Label != "Only" {
		t.Fatalf("a unique name gained a marker: %+v", got["source-a/only"])
	}
	if model, _ := edit.GetJSON(path, "model"); model != beforeModel {
		t.Fatalf("selected model changed from %q to %q", beforeModel, model)
	}
	if theme, _ := edit.GetJSON(path, "theme"); theme != "dark" {
		t.Fatal("unrelated user setting changed")
	}
	// A refresh rewrites magpie's clean picker too, even though the visible
	// descriptions no longer contain the old ownership fingerprint.
	if err := provider.Save(provider.Provider{ID: "source-a", Name: "Renamed",
		Key: "fixture", Chat: "http://127.0.0.1:1/v1",
		Models: []string{"only", "beta", "alpha-alias", "alpha", "new"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	after, _ := read()
	for id, r := range got {
		if after[id] != r {
			t.Errorf("refresh or reordering changed %s: %+v -> %+v", id, r, after[id])
		}
	}
	if _, ok := after["source-a/new"]; !ok {
		t.Fatal("clean picker stopped syncing new models")
	}
	if err := edit.SetJSON(settings.Path(), edit.KV{Path: "claudeMinimalModelPicker", Value: false}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	normal, replaces := read()
	if replaces || !strings.Contains(normal["source-a/alpha"].Description, "via magpie") {
		t.Fatal("default mode was not restored")
	}
}

func TestClaudeMinimalPickerOneSource(t *testing.T) {
	home := syncHome(t)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "CLI",
		Key: "fixture", Chat: "http://127.0.0.1:1/v1", Models: []string{"one", "alias"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "alias"} {
		if err := provider.SetModelName("relay/"+id, "Same model"); err != nil {
			t.Fatal(err)
		}
	}
	if err := edit.SetJSON(settings.Path(), edit.KV{Path: "claudeMinimalModelPicker", Value: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{}`)
	if err := claude(home).Apply("model", "relay/one"); err != nil {
		t.Fatal(err)
	}
	var picker struct {
		Options []struct{ Model, Label string }
	}
	raw, _ := edit.GetJSON(path, "modelPicker")
	if err := json.Unmarshal([]byte(raw), &picker); err != nil {
		t.Fatal(err)
	}
	for _, r := range picker.Options {
		if strings.HasPrefix(r.Model, "relay/") && r.Label != "Same model" {
			t.Fatalf("aliases from one source gained row numbers: %+v", r)
		}
	}
}

func TestClaudePickerSourceNumbers(t *testing.T) {
	syncHome(t)
	first, err := claudePickerSourceNumbers([]string{"b", "a", "a"})
	if err != nil || first["a"] == 0 || first["a"] == first["b"] {
		t.Fatalf("initial sources: %v, %v", first, err)
	}
	again, err := claudePickerSourceNumbers([]string{"a", "b"})
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("reordering renumbered sources: %v, %v", again, err)
	}
	// A missing source stays reserved; a new one sorts before it but gets
	// a later number, and the missing one keeps its number when it returns.
	next, err := claudePickerSourceNumbers([]string{"0-new", "a"})
	if err != nil || next["a"] != first["a"] || next["b"] != first["b"] ||
		next["0-new"] <= max(first["a"], first["b"]) {
		t.Fatalf("adding/hiding a source reused a number: %v, %v", next, err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"c", "d", "e"} {
		wg.Go(func() {
			if _, err := claudePickerSourceNumbers([]string{id}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	all, err := claudePickerSourceNumbers([]string{"b", "a", "0-new", "c", "d", "e"})
	if err != nil {
		t.Fatal(err)
	}
	values := []int{}
	for _, n := range all {
		values = append(values, n)
	}
	slices.Sort(values)
	if len(slices.Compact(values)) != len(all) {
		t.Fatalf("concurrent syncs reused numbers: %v", all)
	}
	for _, bad := range []string{`{`, `null`, `[]`, `{"a":0}`, `{"a":1,"b":1}`} {
		if err := os.WriteFile(claudePickerSourcesPath(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := claudePickerSourceNumbers([]string{"fresh"}); err == nil {
			t.Errorf("silently replaced invalid source registry %q", bad)
		}
		if b, _ := os.ReadFile(claudePickerSourcesPath()); string(b) != bad {
			t.Errorf("invalid source registry overwritten: %s", b)
		}
	}
	if err := os.Remove(claudePickerSourcesPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(claudePickerSourcesPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := claudePickerSourceNumbers([]string{"fresh"}); err == nil {
		t.Fatal("unreadable source registry treated as empty")
	}
}
