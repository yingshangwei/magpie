package provider

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The account-scoped report returned by /usage-session/report on 2026-10-24,
// with the key and account anonymized and dates moved with the test clock.
func asterGateReport(now time.Time) string {
	return fmt.Sprintf(`{"service":"astergate","protocol":"astergate-self-usage","protocol_version":1,
	"account_scope":"accounts","account_quota_shared":true,"accounts":[{
	"account_id":"account-a","provider":"claude","plan":"max","state":"available","cached":true,"current":false,
	"windows":[
	{"account_id":"account-a","kind":"five_hour","scope":"account","used_ratio":0.2,"resets_at":%q,"checked_at":%q},
	{"account_id":"account-a","kind":"weekly","scope":"account","used_ratio":0.1,"resets_at":%q,"checked_at":%q},
	{"account_id":"account-a","kind":"weekly_fable","scope":"model","scoped_model":"Fable","applies_to_models":["claude-fable-5","claude-fable-5-1"],"used_ratio":1,"resets_at":%q,"checked_at":%q}]}]}`,
		now.Add(time.Hour).Format(time.RFC3339Nano), now.Add(-11*time.Minute).Format(time.RFC3339Nano),
		now.Add(6*24*time.Hour).Format(time.RFC3339Nano), now.Add(-10*time.Minute).Format(time.RFC3339Nano),
		now.Add(6*24*time.Hour).Format(time.RFC3339Nano), now.Add(-9*time.Minute).Format(time.RFC3339Nano))
}

type asterGateFake struct {
	logins, reports, refreshes, logouts     atomic.Int32
	cooldown, limited, redirect, down, idle atomic.Bool
	now                                     time.Time
}

func asterGateServer(t *testing.T, origin string) *asterGateFake {
	return asterGateServerAt(t, origin, "sk-aster-test")
}

func asterGateServerAt(t *testing.T, base, apiKey string) *asterGateFake {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	origin, prefix := u.Scheme+"://"+u.Host, u.Path
	f := &asterGateFake{now: time.Now().UTC().Truncate(time.Second)}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Host") != strings.TrimPrefix(origin, "https://") || r.Header.Get("Origin") != origin ||
			r.Header.Get("x-usage-client") != "1" || r.Header.Get("x-usage-origin") != origin ||
			r.Header.Get("Referer") != base+"/key-usage" || r.Header.Get("Authorization") != "" ||
			strings.Contains(string(b), apiKey) || strings.Contains(r.URL.String(), apiKey) {
			t.Error("usage request has the wrong origin or exposed its key")
			w.WriteHeader(400)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix+"/usage-session/") {
			t.Errorf("usage request lost its configured path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		request := r.Method + " " + strings.TrimPrefix(r.URL.Path, prefix)
		switch request {
		case "POST /usage-session/challenge":
			json.NewEncoder(w).Encode(map[string]string{"challenge": "test-challenge", "public_key": enc.EncodeToString(der)})
		case "POST /usage-session/login":
			var login struct{ Challenge, JWE string }
			if err := json.Unmarshal(b, &login); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			payload, err := openAsterGateJWE(key, login.JWE)
			if err != nil || payload["key"] != apiKey || payload["challenge"] != login.Challenge || payload["origin"] != origin {
				t.Errorf("usage login cannot be decrypted or has wrong claims: %v", err)
				w.WriteHeader(401)
				return
			}
			f.logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "usage", Value: "test-session", Path: prefix + "/usage-session", Secure: true, HttpOnly: true})
			w.WriteHeader(204)
		default:
			cookie, err := r.Cookie("usage")
			if err != nil || cookie.Value != "test-session" {
				t.Error("usage session cookie is missing")
				w.WriteHeader(401)
				return
			}
			switch request {
			case "GET /usage-session/report":
				f.reports.Add(1)
				if f.redirect.Load() {
					http.Redirect(w, r, "https://another.invalid/stolen", http.StatusTemporaryRedirect)
					return
				}
				if f.down.Load() {
					w.WriteHeader(503)
					return
				}
				if r.URL.Query().Get("timezone") != "UTC" {
					t.Error("usage report has no valid timezone")
				}
				report := asterGateReport(f.now)
				if f.idle.Load() {
					// The beta report on 2026-11-02 omitted resets_at for
					// its unused five-hour window after the previous one ended.
					report = strings.Replace(report, `"used_ratio":0.2,`, `"used_ratio":0,`, 1)
					reset := fmt.Sprintf(`"resets_at":%q,`, f.now.Add(time.Hour).Format(time.RFC3339Nano))
					report = strings.Replace(report, reset, "", 1)
				} else if f.refreshes.Load() > 0 {
					report = strings.Replace(report, `"used_ratio":0.2`, `"used_ratio":0.4`, 1)
				}
				io.WriteString(w, report)
			case "GET /usage-session/refresh-status":
				next := "null"
				if f.cooldown.Load() {
					next = fmt.Sprintf("%q", f.now.Add(5*time.Minute).Format(time.RFC3339Nano))
				}
				fmt.Fprintf(w, `{"accounts":[{"account_id":"account-a","supported":true,"next_allowed_at":%s}]}`, next)
			case "POST /usage-session/refresh":
				var request map[string]string
				if json.Unmarshal(b, &request) != nil || request["account_id"] != "account-a" {
					t.Error("refresh targets an account outside its report")
				}
				f.refreshes.Add(1)
				if f.limited.Load() {
					w.WriteHeader(429)
					return
				}
				io.WriteString(w, `{"status":"refreshed"}`)
			case "POST /usage-session/logout":
				f.logouts.Add(1)
				w.WriteHeader(204)
			default:
				t.Errorf("unexpected usage request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(404)
			}
		}
	}))
	t.Cleanup(srv.Close)
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = rewrite{srv}
	t.Cleanup(func() { http.DefaultClient.Transport = old })
	return f
}

func openAsterGateJWE(key *rsa.PrivateKey, token string) (map[string]string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 5 {
		return nil, fmt.Errorf("expected compact JWE")
	}
	decoded := make([][]byte, 5)
	for i, part := range parts {
		b, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return nil, err
		}
		decoded[i] = b
	}
	if string(decoded[0]) != `{"alg":"RSA-OAEP-256","enc":"A256GCM"}` {
		return nil, fmt.Errorf("unsupported JWE algorithms")
	}
	cek, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, decoded[1], nil)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	b, err := gcm.Open(nil, decoded[2], append(decoded[3], decoded[4]...), []byte(parts[0]))
	if err != nil {
		return nil, err
	}
	var payload map[string]string
	err = json.Unmarshal(b, &payload)
	return payload, err
}

func TestAsterGateQuotaCard(t *testing.T) {
	for _, c := range []struct{ name, base, key string }{
		{"main", "https://astergate.cc", "sk-aster-test"},
		{"beta", "https://beta.astergate.cc", "sk-aster-test"},
		{"self-hosted IP path", "https://192.0.2.10/astergate", "agk_self-hosted-test"},
		{"self-hosted domain", "https://gateway.example.test", "agk_self-hosted-test"},
		{"self-hosted port and path", "https://gateway.example.test:8443/team/astergate", "agk_self-hosted-test"},
	} {
		t.Run(c.name, func(t *testing.T) { testAsterGateQuotaCard(t, c.base, c.key) })
	}
}

func testAsterGateQuotaCard(t *testing.T, base, key string) {
	keyLimitsHome(t)
	f := asterGateServerAt(t, base, key)
	p := Provider{ID: "astergate-test", Name: "AsterGate", Anthropic: base + "/v1", Key: key}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	qs := PlanQuotas(context.Background())
	if len(qs) != 1 || qs[0].Provider != p.ID || len(qs[0].Windows) != 3 || qs[0].Plan != "max" || qs[0].Balance != "" {
		t.Fatalf("AsterGate's shared allowance card is missing: %+v", qs)
	}
	q := qs[0]
	if q.ReadAt == nil || !q.ReadAt.Equal(f.now.Add(-11*time.Minute)) {
		t.Errorf("cached vendor reading was dated as fetched: %v", q.ReadAt)
	}
	a, ok := KeyAllowance(p)
	if !ok || a.Full("claude-fable-5-1", 100, f.now).IsZero() || !a.Full("claude-opus-5-5", 100, f.now).IsZero() {
		t.Fatalf("Fable's week must stop Fable only: %v %+v", ok, a)
	}
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		if used, _ := a.For(model, f.now); used != 20 {
			t.Errorf("%s takes Fable's usage: %v", model, used)
		}
	}
	if f.refreshes.Load() != 0 || f.logins.Load() != 1 || f.logouts.Load() != 1 {
		t.Fatal("ordinary card read refreshed upstream or left its session open")
	}
	// The saved windows retain their model scope after a process restart.
	lastQuotas.Lock()
	lastQuotas.m, lastQuotas.loaded = nil, false
	lastQuotas.Unlock()
	a = lastKeyAllowance(p)
	if a.Full("claude-fable-5-1", 100, f.now).IsZero() || !a.Full("claude-sonnet-5-5", 100, f.now).IsZero() {
		t.Fatal("persisting the card lost Fable's scope")
	}
	ctx := context.WithValue(Asked(context.Background()), cardRefreshKey{}, cardRefresh{p.ID, ""})
	qs = PlanQuotas(ctx)
	if len(qs) != 1 || qs[0].Windows[0].Used != 40 || f.refreshes.Load() != 1 || f.logouts.Load() != 2 {
		t.Fatalf("explicit refresh did not update its card: %+v", qs)
	}
	f.cooldown.Store(true)
	qs = PlanQuotas(ctx)
	if len(qs) != 1 || qs[0].Error != "" || f.refreshes.Load() != 1 {
		t.Fatal("cooldown retried the upstream refresh or discarded the report")
	}
	// Routing's own read and the Usage page must date the same snapshot.
	r, err := keyWindows(context.Background(), p)
	if err != nil || !r.at.Equal(f.now.Add(-11*time.Minute)) || f.refreshes.Load() != 1 {
		t.Fatalf("routing read: %v at %v", err, r.at)
	}
	keyAllowances.Lock()
	gen := keyAllowances.gen
	keyAllowances.Unlock()
	readKeyAllowance(p, keyAllowanceID(p), gen)
	if r := keyRead(keyAllowanceID(p)); !r.at.Equal(f.now.Add(-11 * time.Minute)) {
		t.Fatal("routing cached the fetch time in place of the measurement time")
	}
}

func TestAsterGateSelfHostedSource(t *testing.T) {
	for _, base := range []string{
		"https://192.0.2.10/astergate", "https://192.0.2.10/astergate/",
		"https://192.0.2.10/astergate/v1", "https://192.0.2.10/astergate/v1/",
	} {
		for _, endpoint := range []string{"chat", "anthropic", "responses"} {
			p := Provider{Key: "agk_self-hosted-test"}
			switch endpoint {
			case "chat":
				p.Chat = base
			case "anthropic":
				p.Anthropic = base
			case "responses":
				p.Responses = base
			}
			src, ok := planQuotaSourceOf(p)
			if !ok || src.url != "https://192.0.2.10/astergate/usage-session/report" {
				t.Errorf("%s at %s has no prefixed allowance source: %+v", endpoint, base, src)
			}
			p.Key = "sk-unrelated-provider"
			if _, ok := planQuotaSourceOf(p); ok {
				t.Errorf("unrelated provider became AsterGate: %s", base)
			}
		}
	}
	for _, base := range []string{
		"http://192.0.2.10/astergate", "https://user:password@gateway.example.test/astergate",
		"https://gateway.example.test/astergate?key=secret", "https://gateway.example.test/astergate#fragment",
		"https://gateway.example.test/a/../astergate", "https://gateway.example.test/a%2fastergate",
	} {
		if _, ok := planQuotaSourceOf(Provider{Anthropic: base, Key: "agk_self-hosted-test"}); ok {
			t.Errorf("ambiguous or insecure AsterGate base accepted: %s", base)
		}
	}
}

func TestAsterGateInvalidExtraKeyNeverUsesGenericAuth(t *testing.T) {
	keyLimitsHome(t)
	dialed := dialsOut(t)
	p := Provider{Anthropic: "https://gateway.example.test/astergate", Key: "agk_self-hosted-test"}
	src, ok := planQuotaSourceOf(p)
	if !ok {
		t.Fatal("self-hosted AsterGate has no allowance source")
	}
	_, _, err := readPlanKey(context.Background(), p, src, "sk-unrelated-provider")
	if err == nil || len(dialed()) != 0 {
		t.Fatalf("invalid extra key fell through to a generic authenticated request: error=%v, dialed=%v", err, dialed())
	}
}

func TestAsterGateReadWhenAsked(t *testing.T) {
	keyLimitsHome(t)
	dialed := dialsOut(t)
	setQuotaReads(t, "asked")
	p := Provider{ID: "astergate-test", Anthropic: "https://astergate.cc", Key: "sk-aster-test"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	qs := PlanQuotas(context.Background())
	if len(qs) != 1 || !readDeferred(qs[0].Error) || len(dialed()) != 0 {
		t.Fatalf("automatic AsterGate read ignored When I ask: %+v, dialed %v", qs, dialed())
	}
}

func TestAsterGateIdleWindow(t *testing.T) {
	keyLimitsHome(t)
	f := asterGateServer(t, "https://beta.astergate.cc")
	f.idle.Store(true)
	p := Provider{ID: "astergate-test", Anthropic: "https://beta.astergate.cc", Key: "sk-aster-test"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{
		context.Background(),
		context.WithValue(Asked(context.Background()), cardRefreshKey{}, cardRefresh{p.ID, ""}),
	} {
		qs := PlanQuotas(ctx)
		if len(qs) != 1 || qs[0].Error != "" || len(qs[0].Windows) != 3 {
			t.Fatalf("unused five-hour window hid the account's quota card: %+v", qs)
		}
		w := qs[0].Windows[0]
		if w.Used != 0 || w.ResetsAt != nil || qs[0].ReadAt == nil || !qs[0].ReadAt.Equal(f.now.Add(-11*time.Minute)) {
			t.Fatalf("unused window invented usage, a reset or a measurement time: %+v", qs[0])
		}
		a, ok := KeyAllowance(p)
		if !ok || !a.Full("claude-opus-5-5", 100, f.now).IsZero() || a.Full("claude-fable-5-1", 100, f.now).IsZero() {
			t.Fatalf("unused window changed the weekly model limits: %v %+v", ok, a)
		}
	}
	if f.refreshes.Load() != 1 || f.logouts.Load() != 2 {
		t.Fatal("unused window stopped explicit refresh or left the usage session open")
	}
}

func TestAsterGateWindowWithoutReset(t *testing.T) {
	now := time.Now().UTC()
	reset := fmt.Sprintf(`"resets_at":%q`, now.Add(time.Hour).Format(time.RFC3339Nano))
	for _, c := range []struct {
		name, ratio, reset string
		valid              bool
	}{
		{"unused omitted", "0", "", true},
		{"unused null", "0", `"resets_at":null,`, true},
		{"used omitted", "0.2", "", false},
		{"used null", "0.2", `"resets_at":null,`, false},
		{"unknown usage", "null", "", false},
		{"zero time", "0", `"resets_at":"0001-01-01T00:00:00Z",`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := strings.Replace(asterGateReport(now), `"used_ratio":0.2`, `"used_ratio":`+c.ratio, 1)
			b = strings.Replace(b, reset+",", c.reset, 1)
			r, _, err := readAsterGateReport([]byte(b))
			if c.valid {
				if err != nil || len(r.ws) != 3 || r.ws[0].Used != 0 || r.ws[0].ResetsAt != nil {
					t.Fatalf("reported unused window was lost or given a reset: %+v %v", r, err)
				}
			} else if err == nil || len(r.ws) != 0 {
				t.Fatal("incomplete window became a known allowance")
			}
		})
	}
}

func TestAsterGateReadFailures(t *testing.T) {
	keyLimitsHome(t)
	f := asterGateServer(t, "https://astergate.cc")
	ctx := context.WithValue(Asked(context.Background()), cardRefreshKey{}, cardRefresh{"astergate-test", ""})
	f.limited.Store(true)
	r, err := asterGateWindows(ctx, "sk-aster-test", "https://astergate.cc")
	if err != nil || len(r.ws) != 3 || f.refreshes.Load() != 1 || f.reports.Load() != 1 {
		t.Fatalf("429 cooldown lost the cached report or retried: %v", err)
	}
	f.redirect.Store(true)
	if _, err := asterGateWindows(context.Background(), "sk-aster-test", "https://astergate.cc"); err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("usage followed a redirect: %v", err)
	}
	f.redirect.Store(false)
	f.down.Store(true)
	if _, err := asterGateWindows(context.Background(), "sk-aster-test", "https://astergate.cc"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("missing report became an empty allowance: %v", err)
	}
	if f.logins.Load() != f.logouts.Load() {
		t.Fatal("failed reads left usage sessions open")
	}
}

func TestAsterGateReportScope(t *testing.T) {
	b := asterGateReport(time.Now().UTC())
	for _, c := range []struct{ name, from, to string }{
		{"no ratio", `"used_ratio":0.2,`, ""},
		{"null ratio", `"used_ratio":0.2`, `"used_ratio":null`},
		{"negative ratio", `"used_ratio":0.2`, `"used_ratio":-1`},
		{"not a ratio", `"used_ratio":0.2`, `"used_ratio":20`},
		{"other version", `"protocol_version":1`, `"protocol_version":2`},
		{"not shared", `"account_quota_shared":true`, `"account_quota_shared":false`},
		{"hidden accounts", `"account_scope":"accounts"`, `"account_scope":"none"`},
		{"wrong model scope", `"scoped_model":"Fable"`, `"scoped_model":"Opus"`},
		{"missing week", `"kind":"weekly"`, `"kind":"unknown"`},
		{"duplicate window", `"kind":"weekly"`, `"kind":"five_hour"`},
		{"wrong account", `"account_id":"account-a","kind"`, `"account_id":"account-b","kind"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _, err := readAsterGateReport([]byte(strings.Replace(b, c.from, c.to, 1)))
			if err == nil || len(r.ws) != 0 {
				t.Fatal("unavailable or differently scoped usage became an allowance")
			}
		})
	}
	zero := strings.Replace(b, `"used_ratio":0.2`, `"used_ratio":0`, 1)
	if r, _, err := readAsterGateReport([]byte(zero)); err != nil || r.ws[0].Used != 0 {
		t.Fatal("a reported zero must remain a known zero")
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(b), &report); err != nil {
		t.Fatal(err)
	}
	accounts := report["accounts"].([]any)
	report["accounts"] = append(accounts, accounts[0])
	pooled, _ := json.Marshal(report)
	if r, _, err := readAsterGateReport(pooled); err == nil || len(r.ws) != 0 {
		t.Fatal("multiple upstream accounts became one invented percentage")
	}
	for _, base := range []string{
		"http://astergate.cc", "https://astergate.cc.attacker.invalid", "https://astergate.cc:8443",
		"http://beta.astergate.cc", "https://beta.astergate.cc.attacker.invalid", "https://beta.astergate.cc:8443",
		"https://astergate.cc@another.invalid", "https://beta.astergate.cc@another.invalid", "https://other.astergate.cc",
	} {
		if _, ok := planQuotaSourceOf(Provider{Anthropic: base}); ok {
			t.Errorf("usage key sent for an unrecognized origin: %s", base)
		}
	}
}

// The relay balance probe (balance_probe.go) asks a provider added by hand
// at /v1/usage and /api/usage/token with its key, at the host's root. An
// AsterGate key is read only through its encrypted usage session, so no
// AsterGate provider is probed: not the hosted sites, and not a
// self-hosted one, whose deployment prefix the probe would drop.
func TestAsterGateIsNeverBalanceProbed(t *testing.T) {
	for _, c := range []struct{ name, base, key string }{
		{"main", "https://astergate.cc", "sk-aster-test"},
		{"beta", "https://beta.astergate.cc", "sk-aster-test"},
		{"self-hosted IP path", "https://192.0.2.10/astergate", "agk_self-hosted-test"},
		{"self-hosted domain", "https://gateway.example.test", "agk_self-hosted-test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			probeSandbox(t)
			var mu sync.Mutex
			var probes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				keyed := strings.Contains(fmt.Sprint(r.Header), c.key)
				if keyed || strings.HasSuffix(r.URL.Path, "/v1/usage") || strings.HasSuffix(r.URL.Path, "/api/usage/token") {
					mu.Lock()
					probes = append(probes, r.Method+" "+r.Header.Get("X-Host")+r.URL.Path)
					mu.Unlock()
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			old := http.DefaultClient.Transport
			http.DefaultClient.Transport = rewrite{srv}
			t.Cleanup(func() { http.DefaultClient.Transport = old })
			p := Provider{ID: "astergate-test", Name: "AsterGate", Anthropic: c.base + "/v1", Key: c.key}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			if eligible, _ := BalanceProbeState(p); eligible {
				t.Error("AsterGate is offered the relay balance probe")
			}
			_, _, _ = Balance(context.Background(), p)
			KeyBalances(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if len(probes) != 0 {
				t.Fatalf("AsterGate key was sent to relay balance queries: %v", probes)
			}
		})
	}
}
