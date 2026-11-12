package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A deliberate fork deviation (forkBridgeTelemetryOff, the user's choice on
// 2026-11-12): upstream's TestClaudeRunReadsItsFlags holds a turn's Claude
// Code to a plain claude's network, so it reads the account's feature flags,
// Message Threads among them (#5963). This fork keeps a turn's run as quiet
// as the one-off runs instead: CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC,
// DISABLE_TELEMETRY and DO_NOT_TRACK are set whatever the user set, in
// magpie's environment or in their Claude Code's settings.json, and every
// turn sends the whole history. Nothing forces threads
// (CLAUDE_CODE_TETHER_LIVE is never set); updates and error reports stay off.
func TestClaudeRunStaysQuiet(t *testing.T) {
	const quiet = "traffic 1 telemetry 1 track 1 tether unset updater 1 errors 1"
	for _, c := range []struct {
		name, settings string
		env            map[string]string
		want           string
	}{
		{name: "nothing set", want: quiet},
		{name: "settings telemetry off", settings: `{"env":{"DISABLE_TELEMETRY":"1"}}`, want: quiet},
		{name: "settings nonessential off", settings: `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"true"}}`, want: quiet},
		{name: "settings telemetry on", settings: `{"env":{"DISABLE_TELEMETRY":"0","DO_NOT_TRACK":"0"}}`, want: quiet},
		{name: "magpie's env", env: map[string]string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DO_NOT_TRACK": "1"}, want: quiet},
		{name: "magpie's env telemetry on", env: map[string]string{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "0", "DISABLE_TELEMETRY": "0", "DO_NOT_TRACK": "0"}, want: quiet},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range append(claudeQuietVars, "CLAUDE_CODE_TETHER_LIVE", "DISABLE_AUTOUPDATER", "DISABLE_ERROR_REPORTING") {
				t.Setenv(k, "")
				os.Unsetenv(k)
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			h := newScriptHarness(t, "ok", `echo "traffic ${CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC-unset} telemetry ${DISABLE_TELEMETRY-unset} track ${DO_NOT_TRACK-unset} tether ${CLAUDE_CODE_TETHER_LIVE-unset} updater ${DISABLE_AUTOUPDATER-unset} errors ${DISABLE_ERROR_REPORTING-unset}" >> $log`)
			if c.settings != "" {
				if err := os.MkdirAll(h.config, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(h.config, "settings.json"), []byte(c.settings), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h.ask("rules", "hello", "next")
			runs := h.runs()
			if len(runs) != 1 {
				t.Fatalf("runs: %d", len(runs))
			}
			for _, log := range runs {
				if !strings.Contains(log, c.want) {
					t.Fatalf("run's environment, want %q:\n%s", c.want, log)
				}
			}
		})
	}
}
