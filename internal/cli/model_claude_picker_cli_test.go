package cli

import (
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/settings"
)

func TestModelClaudePickerCmd(t *testing.T) {
	groupsHome(t)
	catalog.Changed = nil
	for _, mode := range []string{"simple", "default"} {
		if err := modelCmd([]string{"claude-picker", mode}); err != nil {
			t.Fatal(err)
		}
		if settings.Load().ClaudeMinimalModelPicker != (mode == "simple") {
			t.Fatalf("mode %q was not saved", mode)
		}
		if err := modelCmd([]string{"claude-picker"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"typo"}, {"simple", "extra"}, {"--help"}} {
		if err := modelCmd(append([]string{"claude-picker"}, args...)); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
		if settings.Load().ClaudeMinimalModelPicker {
			t.Fatalf("invalid arguments changed the setting: %v", args)
		}
	}
}
