package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/wslrun"
)

// privacyInput is an inherited environment naming a repository, OTel
// export and custom headers, with the bridge's own settings overridden in
// other cases and values.
var privacyInput = []string{
	"PATH=/bin", "HTTPS_PROXY=http://127.0.0.1:12345", "NODE_EXTRA_CA_CERTS=/test/ca.pem",
	"CLAUDE_CONFIG_DIR=/test/account", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/test/storage",
	"GIT_DIR=/test/project/.git", "git_work_tree=/test/project", "GIT_COMMON_DIR=/test/common",
	"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.url", "GIT_CONFIG_VALUE_0=https://git.example.test/team/project",
	"GIT_CONFIG_PARAMETERS='remote.origin.url=https://git.example.test/team/project'",
	"GIT_CONFIG_GLOBAL=/test/global", "GIT_CONFIG_SYSTEM=/test/system", "GIT_CONFIG_NOSYSTEM=0",
	"ANTHROPIC_CUSTOM_HEADERS=X-Project: private-project", "anthropic_custom_headers=X-Owner: private-owner",
	"OTEL_RESOURCE_ATTRIBUTES=service.name=private-project", "otel_exporter_otlp_endpoint=https://collector.example.test",
	"OTEL_LOG_RAW_API_BODIES=1", "CLAUDE_CODE_ENABLE_TELEMETRY=1", "CLAUDE_CODE_ENABLE_FEEDBACK_SURVEY_FOR_OTEL=1",
	"ENABLE_CLAUDEAI_MCP_SERVERS=1", "DISABLE_AUTO_COMPACT=0", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=0",
	"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS=0", "claude_code_disable_nonessential_traffic=false",
	"DISABLE_ERROR_REPORTING=0", "DISABLE_AUTOUPDATER=0", "disable_telemetry=0", "DO_NOT_TRACK=0",
	"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=0",
}

// privacyEnvHolds checks got has each of want once, as set, never an
// inherited Git, OTel or custom-header setting, and the inputs a run needs
// (the first five of privacyInput).
func privacyEnvHolds(t *testing.T, got []string, want map[string]string) {
	t.Helper()
	seen := map[string]int{}
	for _, e := range got {
		k, v, _ := strings.Cut(e, "=")
		upper := strings.ToUpper(k)
		if expected, ok := want[upper]; ok {
			seen[upper]++
			if k != upper || v != expected {
				t.Errorf("privacy setting %s has an inherited override", e)
			}
			continue
		}
		if strings.HasPrefix(upper, "GIT_") || strings.HasPrefix(upper, "OTEL_") || upper == "ANTHROPIC_CUSTOM_HEADERS" ||
			upper == "CLAUDE_CODE_ENABLE_TELEMETRY" || upper == "CLAUDE_CODE_ENABLE_FEEDBACK_SURVEY_FOR_OTEL" {
			t.Errorf("bridge inherited %s", e)
		}
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("privacy setting %s occurs %d times, want once", k, seen[k])
		}
	}
	for _, keep := range privacyInput[:5] {
		if !slices.Contains(got, keep) {
			t.Errorf("required input lost: %s", keep)
		}
	}
}

// privacySettings are what every bridge run is given, one-off or a turn's:
// in this fork telemetry, tracking and nonessential traffic are off for a
// turn's run too (forkBridgeTelemetryOff).
func privacySettings(ttl string) map[string]string {
	return map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"ENABLE_CLAUDEAI_MCP_SERVERS": "0", "DISABLE_AUTO_COMPACT": "1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
		"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS": "1", "CLAUDE_CODE_PROMPT_CACHE_TTL": ttl,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_TELEMETRY": "1", "DO_NOT_TRACK": "1",
		"DISABLE_ERROR_REPORTING": "1", "DISABLE_AUTOUPDATER": "1",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
	}
}

// The one-off runs (the model test, the warm-up, /usage) are quiet.
func TestClaudePrivacyEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	privacyEnvHolds(t, cleanClaudeEnv(privacyInput), privacySettings(claudeRunTTL(privacyInput)))
}

// A turn's run is as quiet as a one-off run in this fork
// (forkBridgeTelemetryOff), whatever the user set: Git, OTel and custom
// headers stay out, and telemetry, tracking and nonessential traffic off.
func TestClaudePrivacyRunEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, k := range claudeQuietVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	privacyEnvHolds(t, claudeRunEnv(privacyInput, "5m"), privacySettings("5m"))
	on := append(slices.Clone(privacyInput), "DISABLE_TELEMETRY=0", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=0")
	privacyEnvHolds(t, claudeRunEnv(on, "5m"), privacySettings("5m"))
}

// Git reads environment overrides, includes in global/system config and
// the user's name and email in their global config whatever folder it runs
// in. A turn's run works in the caller's own repository (clientWorkDir),
// which stays readable; nothing else of the user's Git does.
func TestClaudePrivacyGitSources(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	home := t.TempDir()
	project := filepath.Join(root, "project")
	cmd := proc.Command("git", "init", "-q", project)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	origin := "https://git.example.test/team/private-project.git"
	email := "owner@example.test"
	config := "[remote \"origin\"]\n\turl = " + origin + "\n"
	included := filepath.Join(root, "included")
	global := filepath.Join(root, "global")
	for file, contents := range map[string]string{
		included: config, global: "[include]\n\tpath = " + filepath.ToSlash(included) + "\n",
		filepath.Join(home, ".gitconfig"):        "[user]\n\temail = " + email + "\n",
		filepath.Join(project, ".git", "config"): "[core]\n\trepositoryformatversion = 0\n\tbare = false\n" + config,
	} {
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runs := map[string]func([]string) []string{
		"one-off": cleanClaudeEnv,
		"turn":    func(env []string) []string { return claudeRunEnv(env, "1h") },
	}
	for name, c := range map[string]struct {
		dirty    []string
		key, val string
	}{
		"repository environment": {[]string{"GIT_DIR=" + filepath.Join(project, ".git"), "GIT_WORK_TREE=" + project}, "remote.origin.url", origin},
		"global include":         {[]string{"GIT_CONFIG_GLOBAL=" + global}, "remote.origin.url", origin},
		"system config":          {[]string{"GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM=" + included}, "remote.origin.url", origin},
		"config injection":       {[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.url", "GIT_CONFIG_VALUE_0=" + origin}, "remote.origin.url", origin},
		"home config":            {[]string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}, "user.email", email},
	} {
		for run, envOf := range runs {
			t.Run(name+"/"+run, func(t *testing.T) {
				env := append(os.Environ(), c.dirty...)
				read := func(env []string) ([]byte, error) {
					cmd := proc.Command("git", "config", "--get", c.key)
					cmd.Dir, cmd.Env = work, env
					return cmd.CombinedOutput()
				}
				if out, err := read(env); err != nil || strings.TrimSpace(string(out)) != c.val {
					t.Fatalf("fixture didn't expose its %s: %v: %s", c.key, err, out)
				}
				if out, err := read(envOf(env)); err == nil || strings.TrimSpace(string(out)) != "" {
					t.Errorf("bridge could still read the fixture's %s through %s", c.key, name)
				}
			})
		}
	}
}

func TestClaudePrivacyWSLEnvironment(t *testing.T) {
	cli := claudeCLI{wsl: &wslrun.Tool{}}
	env := cli.env(cleanClaudeEnv([]string{"GIT_DIR=/test/project/.git"}), "")
	var pass string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "WSLENV="); ok {
			pass = v
		}
	}
	for _, name := range []string{"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "CLAUDE_CODE_DISABLE_AUTO_MEMORY",
		"CLAUDE_CODE_PROMPT_CACHE_TTL", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL"} {
		if !slices.Contains(strings.Split(pass, ":"), name) {
			t.Errorf("WSL run is missing %s", name)
		}
	}
	if !slices.Contains(env, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Error("WSL must use its Linux null device, not a Windows path")
	}
}

// Use the real handler, routing, signing and retry paths with a transport
// that serves fixture bytes in memory: no listener or vendor is involved.
func TestClaudePrivacyRelayHeaders(t *testing.T) {
	noRests()
	t.Cleanup(noRests)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := provider.Provider{ID: "privacy", Name: "Privacy", Key: "fixture-key", Models: []string{"m1"}, Anthropic: "http://127.0.0.1:1",
		Headers: map[string]string{"x-ClAuDe-CoDe-WiGgLy-DoVe": `jy="git.example.test/team/configured"`, "X-Fixture": "kept"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	s := New()
	calls := 0
	s.client = &http.Client{Transport: countTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		for k := range req.Header {
			if strings.EqualFold(k, "x-claude-code-wiggly-dove") {
				t.Errorf("request %d forwarded repository metadata", calls)
			}
		}
		for k, want := range map[string]string{"X-Fixture": "kept", "X-Claude-Code-Session-Id": "fixture-session", "Anthropic-Version": "2023-06-01"} {
			if req.Header.Get(k) != want {
				t.Errorf("request %d lost required header %s", calls, k)
			}
		}
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"type":"authentication_error","message":"x-api-key and Authorization cannot both be present"}}`))
		} else {
			w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`))
		}
		return w.Result(), nil
	})}
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"privacy/m1","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer "+Token)
	req.Header.Set("User-Agent", "claude-cli/2.1.296 (external, cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "fixture-session")
	req.Header["x-claude-code-wiggly-dove"] = []string{`jy="git.example.test/team/client"`, `jy="git.example.test/team/second"`}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || calls != 2 {
		t.Fatalf("retry result: status %d, calls %d, body %s", w.Code, calls, w.Body.String())
	}
}

func TestClaudePrivacyCopiedHeaders(t *testing.T) {
	dst := http.Header{}
	copyHeaders(dst, http.Header{
		"X-ClAuDe-CoDe-WiGgLy-DoVe": {"private"},
		"X-Claude-Code-Session-Id":  {"session"}, "Authorization": {"Bearer fixture"},
	})
	for k := range dst {
		if strings.EqualFold(k, "x-claude-code-wiggly-dove") {
			t.Error("backend passthrough copied repository metadata")
		}
	}
	if dst.Get("X-Claude-Code-Session-Id") != "session" || dst.Get("Authorization") != "Bearer fixture" {
		t.Error("backend passthrough lost unrelated headers")
	}
}

// An explicit account proxy's failure stays a failure. The fake dialer
// records transport decisions without opening a socket or resolving DNS.
func TestClaudePrivacyProxyFailure(t *testing.T) {
	calls := 0
	base := &http.Transport{Proxy: netproxy.Func, DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		calls++
		if addr != "127.0.0.1:12345" {
			t.Errorf("proxy failure attempted a direct dial to %s", addr)
		}
		return nil, errors.New("fixture proxy unavailable")
	}}
	client := &http.Client{Transport: netproxy.Dispatch(base)}
	for _, target := range []string{"http://model.example.test", "https://model.example.test"} {
		req, _ := http.NewRequestWithContext(netproxy.With(context.Background(), "http://127.0.0.1:12345"), http.MethodGet, target, nil)
		if _, err := client.Do(req); err == nil {
			t.Error("unavailable proxy did not fail the request")
		}
	}
	if calls != 2 {
		t.Errorf("got %d dial attempts, want one per failed request", calls)
	}
}

func BenchmarkClaudePrivacyEnvironment(b *testing.B) {
	env := []string{"PATH=/bin", "HOME=/fixture", "CLAUDE_CONFIG_DIR=/fixture/account", "HTTPS_PROXY=http://127.0.0.1:12345",
		"GIT_DIR=/fixture/.git", "GIT_CONFIG_COUNT=1", "OTEL_RESOURCE_ATTRIBUTES=service.name=fixture", "ANTHROPIC_CUSTOM_HEADERS=X-Project: fixture"}
	b.ReportAllocs()
	for b.Loop() {
		cleanClaudeEnv(env)
	}
}

func BenchmarkClaudePrivacySigning(b *testing.B) {
	p := provider.Provider{Key: "fixture"}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1", nil)
	b.ReportAllocs()
	for b.Loop() {
		req.Header.Set("X-Claude-Code-Wiggly-Dove", "fixture")
		if err := p.Sign(context.Background(), req, provider.Anthropic, nil); err != nil {
			b.Fatal(err)
		}
	}
}
