package agent

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// A relayed Claude model must name the relay even though its label looks
// like a native model. The source survives sync without exposing accounts.
func TestClaudeMinimalPickerSourceDescription(t *testing.T) {
	home := syncHome(t)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Super Relay",
		Key: "fixture", Chat: "http://127.0.0.1:1/v1", Models: []string{"claude-sonnet-4-6"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, path, `{"theme":"dark"}`)
	if err := edit.SetJSON(settings.Path(), edit.KV{Path: "claudeMinimalModelPicker", Value: true}); err != nil {
		t.Fatal(err)
	}
	a := claude(home)
	if err := a.Apply("model", "relay/claude-sonnet-4-6"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.Sync(); err != nil {
			t.Fatal(err)
		}
		var picker struct {
			Options []struct{ Model, Description string }
		}
		raw, _ := edit.GetJSON(path, "modelPicker")
		if err := json.Unmarshal([]byte(raw), &picker); err != nil {
			t.Fatal(err)
		}
		if len(picker.Options) != 1 || picker.Options[0].Description != "Source: Super Relay" {
			t.Fatalf("relayed Claude source missing from the generated picker: %+v", picker.Options)
		}
		if !claudeSameModel(picker.Options[0].Model, "relay/claude-sonnet-4-6") {
			t.Fatalf("source description changed the route: %+v", picker.Options)
		}
	}
}

func TestClaudePickerDescriptionUsesServingSource(t *testing.T) {
	for _, c := range []struct{ ref, source, want string }{
		{"claude/claude-opus-5-5", "Claude Code", claudePickerBlankDescription},
		{"codex/gpt-6.1-sol", "Codex", claudePickerBlankDescription},
		{"antigravity/gemini-4-argon", "Antigravity", claudePickerBlankDescription},
		{"gemini/gemini-3.8-flash", "Gemini CLI", claudePickerBlankDescription},
		{"antigravity/claude-sonnet-4-6", "Antigravity", "Source: Antigravity"},
		{"antigravity/gpt-oss-120b", "Antigravity", "Source: Antigravity"},
		{"relay/claude-sonnet-4-6", "Super Relay", "Source: Super Relay"},
		{"traex/gemini-3.1-pro", "TraeX", "Source: TraeX"},
		{"traex/gpt-5.2", "TraeX", "Source: TraeX"},
		{"llmbox/deepseek-v4.1-flash", "LLMBox", "Source: LLMBox"},
		{"group/auto-claude", RoutingGroups, "Source: Routing groups"},
		{"custom/claude-opus-5-5", "", "Source: custom"},
	} {
		t.Run(c.ref, func(t *testing.T) {
			o := Option{Ref: c.ref, Group: c.source, Label: "A renamed model", Note: "account@example.com · via magpie"}
			if got := claudePickerDescription(o); got != c.want {
				t.Errorf("%s description = %q, want %q", c.ref, got, c.want)
			}
		})
	}
}
