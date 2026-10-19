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

func TestClaudePrivacyEnvironment(t *testing.T) {
	input := []string{
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
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=0", "claude_code_disable_nonessential_traffic=false",
		"DISABLE_TELEMETRY=0", "DISABLE_ERROR_REPORTING=0", "DISABLE_AUTOUPDATER=0",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=0",
	}
	got := cleanClaudeEnv(input)
	want := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CEILING_DIRECTORIES": os.TempDir(),
		"ENABLE_CLAUDEAI_MCP_SERVERS": "0", "DISABLE_AUTO_COMPACT": "1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_PROMPT_CACHE_TTL": "1h",
		"DISABLE_TELEMETRY": "1", "DISABLE_ERROR_REPORTING": "1", "DISABLE_AUTOUPDATER": "1",
		"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
	}
	seen := map[string]int{}
	for _, e := range got {
		k, v, _ := strings.Cut(e, "=")
		upper := strings.ToUpper(k)
		if expected, ok := want[upper]; ok {
			seen[upper]++
			if k != upper || v != expected {
				t.Errorf("privacy setting %s has an inherited override", k)
			}
			continue
		}
		if strings.HasPrefix(upper, "GIT_") || strings.HasPrefix(upper, "OTEL_") || upper == "ANTHROPIC_CUSTOM_HEADERS" ||
			upper == "CLAUDE_CODE_ENABLE_TELEMETRY" || upper == "CLAUDE_CODE_ENABLE_FEEDBACK_SURVEY_FOR_OTEL" {
			t.Errorf("bridge inherited %s", k)
		}
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("privacy setting %s occurs %d times, want once", k, seen[k])
		}
	}
	for _, keep := range input[:5] {
		if !slices.Contains(got, keep) {
			t.Errorf("required input lost: %s", keep)
		}
	}
}

// Git reads both environment overrides and includes in global/system config
// even when the bridge's current directory is outside a repository.
func TestClaudePrivacyGitSources(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	project := filepath.Join(root, "project")
	cmd := proc.Command("git", "init", "-q", project)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	origin := "https://git.example.test/team/private-project.git"
	config := "[remote \"origin\"]\n\turl = " + origin + "\n"
	included := filepath.Join(root, "included")
	global := filepath.Join(root, "global")
	for file, contents := range map[string]string{
		included: config, global: "[include]\n\tpath = " + filepath.ToSlash(included) + "\n",
		filepath.Join(project, ".git", "config"): "[core]\n\trepositoryformatversion = 0\n\tbare = false\n" + config,
	} {
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, dirty := range map[string][]string{
		"repository environment": {"GIT_DIR=" + filepath.Join(project, ".git"), "GIT_WORK_TREE=" + project},
		"global include":         {"GIT_CONFIG_GLOBAL=" + global},
		"system config":          {"GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM=" + included},
		"config injection":       {"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=remote.origin.url", "GIT_CONFIG_VALUE_0=" + origin},
	} {
		t.Run(name, func(t *testing.T) {
			env := append(os.Environ(), dirty...)
			read := func(env []string) ([]byte, error) {
				cmd := proc.Command("git", "config", "--get", "remote.origin.url")
				cmd.Dir, cmd.Env = work, env
				return cmd.CombinedOutput()
			}
			if out, err := read(env); err != nil || strings.TrimSpace(string(out)) != origin {
				t.Fatalf("fixture didn't expose its origin: %v: %s", err, out)
			}
			if out, err := read(cleanClaudeEnv(env)); err == nil || strings.TrimSpace(string(out)) != "" {
				t.Errorf("bridge could still read the fixture origin through %s", name)
			}
		})
	}
	t.Run("temporary folder in a repository", func(t *testing.T) {
		// All bridge and auxiliary command working directories are children
		// of TempDir. A custom TMPDIR must not make its parent Git visible.
		for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
			t.Setenv(k, project)
		}
		work := filepath.Join(project, "bridge")
		if err := os.Mkdir(work, 0o700); err != nil {
			t.Fatal(err)
		}
		read := func(env []string) ([]byte, error) {
			cmd := proc.Command("git", "config", "--get", "remote.origin.url")
			cmd.Dir, cmd.Env = work, env
			return cmd.CombinedOutput()
		}
		if out, err := read(os.Environ()); err != nil || strings.TrimSpace(string(out)) != origin {
			t.Fatalf("fixture didn't expose its parent origin: %v: %s", err, out)
		}
		if out, err := read(cleanClaudeEnv(os.Environ())); err == nil || strings.TrimSpace(string(out)) != "" {
			t.Error("bridge could still read its temporary folder's parent origin")
		}
	})
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
		"CLAUDE_CODE_PROMPT_CACHE_TTL", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL", "GIT_CEILING_DIRECTORIES/p"} {
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
