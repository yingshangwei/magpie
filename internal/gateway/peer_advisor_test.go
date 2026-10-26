package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

func TestPeerAdvisorCannotUseAnUnsharedNativeModel(t *testing.T) {
	fresh(t)
	h := newScriptHarness(t, "native advisor answer", "")
	if err := os.MkdirAll(h.config, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := mustJSON(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "peer-advisor-test-access", "refreshToken": "peer-advisor-test-refresh",
		"expiresAt": time.Now().Add(24 * time.Hour).UnixMilli(), "subscriptionType": "max",
	}})
	if err := os.WriteFile(filepath.Join(h.config, ".credentials.json"), credentials, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude.json"), mustJSON(map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "peer-advisor@example.test", "accountUuid": "peer-advisor-test-account"},
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	provider.ForgetAccounts()
	t.Cleanup(provider.ForgetAccounts)
	if p, err := provider.Find("claude"); err != nil || p.Account == nil {
		t.Fatalf("native Claude fixture unavailable: %v", err)
	}
	if err := provider.Save(provider.Provider{ID: "claude", Models: []string{"claude-sonnet-5", "claude-opus-5-5"}}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "claude", Models: []string{"claude-sonnet-5"}}}})
	body := advisorTurn("claude/claude-sonnet-5", "claude-opus-5-5")
	status, answer := peerTestRoundTrip(t, pair.a.peerHub(), http.MethodPost, "/v1/messages", body, true)
	started := false
	for _, log := range h.runs() {
		started = started || strings.Contains(log, "--advisor claude-opus-5-5")
	}
	if status != http.StatusForbidden || started {
		t.Fatalf("unshared advisor reached B's native Claude bridge: status=%d advisorStarted=%v", status, started)
	}
	if !strings.Contains(answer, "advisor") {
		t.Fatalf("peer refusal did not identify the advisor selection: %s", answer)
	}
	// A local caller keeps the native advisor capability and its existing
	// model resolution; only the peer's grant restricts the nested model.
	status, answer = peerTestPost(t, pair.bHTTP.URL+"/v1/messages", body)
	if status != http.StatusOK || !strings.Contains(answer, "native advisor answer") {
		t.Fatalf("local native advisor behavior changed: %d %s", status, answer)
	}
	started = false
	for _, log := range h.runs() {
		started = started || strings.Contains(log, "--advisor claude-opus-5-5")
	}
	if !started {
		t.Fatal("the local native request did not pass its advisor to Claude Code")
	}
	status, answer = peerTestRoundTrip(t, pair.a.peerHub(), http.MethodPost, "/v1/messages", advisorTurn("claude/claude-sonnet-5", "claude/claude-sonnet-5[1m]"), true)
	if status != http.StatusOK || !strings.Contains(answer, "native advisor answer") {
		t.Fatalf("explicitly shared native advisor was refused: %d %s", status, answer)
	}
	started = false
	for _, log := range h.runs() {
		started = started || strings.Contains(log, "--advisor claude-sonnet-5")
	}
	if !started {
		t.Fatal("the shared advisor did not reach B's native Claude bridge")
	}
}

func TestPeerAdvisorReferencesStayWithinTheSelectedProvider(t *testing.T) {
	fresh(t)
	var calls atomic.Int32
	got := make(chan string, 32)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls.Add(1)
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			if strings.HasPrefix(tool.Get("type").String(), "advisor_") {
				got <- tool.Get("model").String()
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, peerTestSSE(false))
	}))
	t.Cleanup(up.Close)
	for _, p := range []provider.Provider{
		{ID: "fake", Key: "k", Anthropic: up.URL, Models: []string{"claude-sonnet-5", "claude-haiku-4-5", "claude-opus-5-5"}},
		{ID: "other", Key: "k", Anthropic: up.URL, Models: []string{"claude-haiku-4-5"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{
		{Provider: "fake", Models: []string{"claude-sonnet-5", "claude-haiku-4-5"}},
		{Provider: "other", Models: []string{"claude-haiku-4-5"}},
	}})
	for _, tc := range []struct {
		advisor   string
		allowed   bool
		duplicate bool
	}{
		{"claude-opus-5-5", false, false},
		{"fake/claude-opus-5-5[1m]", false, false},
		{"other/claude-haiku-4-5", false, false},
		{"claude-future-unselected", false, false},
		{"fake/claude-opus-5-5", false, true},
		{"claude-haiku-4-5", true, false},
		{"fake/claude-haiku-4-5", true, false},
		{" fake/claude-haiku-4-5[1m] ", true, false},
	} {
		name := tc.advisor
		if tc.duplicate {
			name = "duplicate tools"
		}
		t.Run(name, func(t *testing.T) {
			before := calls.Load()
			body := advisorTurn("fake/claude-sonnet-5", tc.advisor)
			if tc.duplicate {
				body = strings.TrimSuffix(strings.TrimSpace(body), "}") + `,"tools":[{"type":"advisor_20260301","name":"advisor","model":"fake/claude-haiku-4-5"}]}`
			}
			status, answer := peerTestRoundTrip(t, pair.a.peerHub(), http.MethodPost, "/v1/messages", body, true)
			if !tc.allowed {
				if calls.Load() != before || status != http.StatusForbidden {
					t.Fatalf("advisor %q escaped the peer's provider/model selection: status=%d vendorCalls=%d", tc.advisor, status, calls.Load()-before)
				}
				return
			}
			if status != http.StatusOK || calls.Load() != before+1 {
				t.Fatalf("shared advisor %q was not forwarded: %d %s", tc.advisor, status, answer)
			}
			select {
			case model := <-got:
				if model != "claude-haiku-4-5" {
					t.Fatalf("shared advisor reached the wrong upstream model: %q", model)
				}
			default:
				t.Fatal("shared advisor was silently removed")
			}
		})
	}
}

func TestPeerAdvisorCannotGainAccessThroughAnAllowedFallback(t *testing.T) {
	fresh(t)
	var primaryCalls, spareCalls, spareAdvisors atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	t.Cleanup(primary.Close)
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		spareCalls.Add(1)
		for _, tool := range gjson.GetBytes(body, "tools").Array() {
			if strings.HasPrefix(tool.Get("type").String(), "advisor_") && tool.Get("model").String() == "claude-opus-5-5" {
				spareAdvisors.Add(1)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, peerTestSSE(false))
	}))
	t.Cleanup(spare.Close)
	// Put the spare first in local model resolution. Its Opus exists on B,
	// but this peer can only use that provider's Sonnet.
	for _, p := range []provider.Provider{
		{ID: "a-spare", Key: "k", Anthropic: spare.URL, Models: []string{"claude-sonnet-5", "claude-opus-5-5"}},
		{ID: "z-primary", Key: "k", Anthropic: primary.URL, Models: []string{"claude-sonnet-5", "claude-opus-5-5"}, Fallback: []string{"a-spare/claude-sonnet-5"}},
	} {
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{
		{Provider: "z-primary", Models: []string{"claude-sonnet-5", "claude-opus-5-5"}},
		{Provider: "a-spare", Models: []string{"claude-sonnet-5"}},
	}})
	body := strings.Replace(advisorTurn("z-primary/claude-sonnet-5", "claude-opus-5-5"), `"stream":true`, `"stream":false`, 1)
	status, answer := peerTestRoundTrip(t, pair.a.peerHub(), http.MethodPost, "/v1/messages", body, true)
	if primaryCalls.Load() == 0 {
		t.Fatalf("the explicitly shared primary was not tried: %d %s", status, answer)
	}
	if spareCalls.Load() != 0 || status != http.StatusForbidden {
		t.Fatalf("fallback gained an unshared advisor: status=%d spareCalls=%d opusAdvisors=%d", status, spareCalls.Load(), spareAdvisors.Load())
	}
	status, answer = peerTestRoundTrip(t, pair.a.peerHub(), http.MethodPost, "/v1/messages", `{"model":"z-primary/claude-sonnet-5","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"no advisor"}]}`, true)
	if status != http.StatusOK || spareCalls.Load() != 1 || spareAdvisors.Load() != 0 {
		t.Fatalf("the shared fallback without an advisor was blocked: %d %s", status, answer)
	}
}
