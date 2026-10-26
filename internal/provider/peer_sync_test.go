package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func peerSyncHome(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(h, "cache"))
	Changed()
}

func peerSyncConfig() PeerProviderConfig {
	return PeerProviderConfig{
		SourceID: "relay", Name: "Shared API", Chat: "https://api.example.test/v1",
		Models: []string{"allowed"}, Key: "sk-inference",
	}
}

func peerSyncStored(t *testing.T, id string) Provider {
	t.Helper()
	ps, err := Stored()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := find(ps, id)
	if !ok {
		t.Fatalf("missing stored provider %s", id)
	}
	return p
}

func TestPeerSyncCopiesExplicitAPIConfigWithoutCallingAccountsOrNetwork(t *testing.T) {
	peerSyncHome(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "sync must not make HTTP requests", http.StatusInternalServerError)
	}))
	defer srv.Close()
	source := Provider{
		ID: "relay", Name: "Shared API", Chat: srv.URL + "/v1", Responses: srv.URL + "/v1",
		Anthropic: srv.URL, Gemini: srv.URL + "/v1beta", Models: []string{"allowed"},
		PickedFrom: []string{"allowed", "hidden"}, Key: "sk-inference", KeyName: "first", KeyProtocol: Chat,
		Keys:        []KeyAccount{{Name: "second", Key: "sk-ant", Protocol: Anthropic}, {Name: "spare", Key: "sk-off", Off: true}},
		Headers:     map[string]string{"Authorization": "Bearer header-secret", "X-Custom": "private-value", "anthropic-version": "2023-06-01"},
		AccessKeyID: "cloud-account", SecretAccessKey: "cloud-secret", BalanceToken: "balance-secret",
		Proxy: "http://127.0.0.1:9080", Fallback: []string{"else/model"}, ExtraBody: ExtraBody{"region": json.RawMessage(`"local"`)},
	}
	if err := storeInitial(file{Providers: []Provider{source}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(source.ID, source.Chat, []catalog.Model{{ID: "allowed"}, {ID: "hidden"}}); err != nil {
		t.Fatal(err)
	}
	accountReads := AllBuilt.Load()
	cfg, err := ExportPeerConfig(source.ID, []string{"allowed"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Key != source.Key || cfg.KeyName != "first" || cfg.KeyProtocol != Chat || len(cfg.Keys) != 2 || !cfg.Keys[1].Off || cfg.Keys[0].Protocol != Anthropic {
		t.Fatal("API key identity, protocol or disabled state was lost")
	}
	id, err := ImportPeerConfig("machine-b", cfg, true)
	if err != nil || id != "sync-machine-b-relay" {
		t.Fatalf("import: %q, %v", id, err)
	}
	p := peerSyncStored(t, id)
	if p.PeerSource != "machine-b/relay" || p.Peer != "" || p.Off || !slices.Equal(p.Models, []string{"allowed"}) {
		t.Fatalf("unexpected ownership or model scope: id=%s source=%s models=%v off=%v", p.ID, p.PeerSource, p.Models, p.Off)
	}
	if p.Proxy != "" || len(p.Fallback) != 0 || p.AccessKeyID != "" || p.SecretAccessKey != "" || p.BalanceToken != "" || p.Account != nil || p.Vertex != nil || len(p.ExtraBody) != 0 {
		t.Fatal("machine-local or non-whitelisted configuration crossed the boundary")
	}
	if p.Key != source.Key || !reflect.DeepEqual(p.Keys, source.Keys) || !reflect.DeepEqual(p.Headers, source.Headers) {
		t.Fatal("allowed credentials did not survive the round trip")
	}
	if got := peerSyncStored(t, source.ID); !sameStored(source, got) {
		t.Fatal("the local provider with the same source id changed")
	}
	if calls.Load() != 0 || AllBuilt.Load() != accountReads {
		t.Fatal("configuration sync contacted a server or discovered accounts")
	}
	// The caller may reuse or edit its config after the import.
	cfg.Models[0], cfg.Headers["Authorization"], cfg.Keys[0].Key = "different", "different", "different"
	p = peerSyncStored(t, id)
	if p.Models[0] != "allowed" || p.Keys[0].Key != "sk-ant" || p.Headers["Authorization"] != "Bearer header-secret" {
		t.Fatal("stored config shared mutable data with its caller")
	}
}

func TestPeerSyncRequiresAnExplicitSubsetOfExposedModels(t *testing.T) {
	peerSyncHome(t)
	p := Provider{ID: "relay", Name: "API", Chat: "https://api.example.test/v1", Key: "sk-test", Models: []string{"allowed"}, PickedFrom: []string{"allowed", "hidden"}}
	if err := storeInitial(file{Providers: []Provider{p}}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "allowed"}, {ID: "hidden"}}); err != nil {
		t.Fatal(err)
	}
	for _, models := range [][]string{nil, {}, {"hidden"}, {"allowed", "hidden"}, {"allowed", "allowed"}, {" allowed"}} {
		if _, err := ExportPeerConfig(p.ID, models, true); err == nil {
			t.Errorf("accepted invalid scope %q", models)
		}
	}
	p.NoModels, p.Models = true, nil
	if err := storeInitial(file{Providers: []Provider{p}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportPeerConfig(p.ID, []string{"allowed"}, true); err == nil {
		t.Fatal("usage-only provider exported an unserved model")
	}
}

func TestPeerSyncMetadataDoesNotKeepOldSecrets(t *testing.T) {
	peerSyncHome(t)
	source := Provider{ID: "relay", Chat: "https://api.example.test/v1", Key: "sk-test", Models: []string{"allowed"}, Headers: map[string]string{
		"Authorization": "Bearer private", "Cookie": "session=private", "X-Custom": "also-private", "anthropic-version": "2023-06-01",
	}}
	if err := storeInitial(file{Providers: []Provider{source}}); err != nil {
		t.Fatal(err)
	}
	full, err := ExportPeerConfig(source.ID, source.Models, true)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ImportPeerConfig("b", full, true)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ExportPeerConfig(source.ID, source.Models, false)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Key != "" || len(metadata.Keys) != 0 || len(metadata.Headers) != 1 || metadata.Headers["anthropic-version"] != "2023-06-01" {
		t.Fatal("metadata export retained credentials or unknown custom headers")
	}
	if _, err := ImportPeerConfig("b", metadata, false); err != nil {
		t.Fatal(err)
	}
	p := peerSyncStored(t, id)
	if !p.Off || p.Key != "" || len(p.Keys) != 0 || len(p.Headers) != 1 || p.Headers["Authorization"] != "" {
		t.Fatal("metadata update retained earlier credentials or enabled the provider")
	}
}

func TestPeerSyncNativeAndMachineCredentialsStayBlocked(t *testing.T) {
	peerSyncHome(t)
	if NativePeerSyncEnabled {
		t.Fatal("native provider configuration sync must remain disabled in code")
	}
	base := Provider{ID: "relay", Chat: "https://api.example.test/v1", Key: "sk-test", Models: []string{"allowed"}}
	for _, modify := range []func(*Provider){
		func(p *Provider) { p.ID = "claude" },
		func(p *Provider) { p.ID = "codex" },
		func(p *Provider) { p.Preset = " Claude " },
		func(p *Provider) { p.Preset = "CODEX" },
		func(p *Provider) { p.Account = &Account{} },
		func(p *Provider) { p.Chat = "plugin://relay/v1" },
		func(p *Provider) { p.Peer = "another-machine" },
		func(p *Provider) { p.Preset = RemoteMagpiePreset },
		func(p *Provider) { p.Preset = VertexPreset },
		func(p *Provider) { p.Vertex = &Vertex{} },
	} {
		p := base
		modify(&p)
		if _, err := exportPeerProvider(p, p.Models, true); err == nil {
			t.Errorf("accepted forbidden provider kind: id=%s preset=%s peer=%s", p.ID, p.Preset, p.Peer)
		}
	}
	for _, id := range []string{"claude", "codex", "Claude", " codex ", "copilot", VertexPreset, RemoteMagpiePreset} {
		cfg := peerSyncConfig()
		cfg.SourceID = id
		if _, err := ImportPeerConfig("b", cfg, true); err == nil {
			t.Errorf("accepted forbidden source id %q", id)
		}
	}
	// Anthropic's API key endpoint is allowed; only the native sign-in is blocked.
	p := Provider{ID: "anthropic", Preset: "anthropic", Anthropic: "https://api.anthropic.com", Key: "sk-api", Models: []string{"claude-sonnet-test"}}
	if _, err := exportPeerProvider(p, p.Models, true); err != nil {
		t.Fatalf("ordinary Anthropic API provider was blocked: %v", err)
	}
}

func TestPeerSyncRejectsFieldsOutsideTheWireAllowList(t *testing.T) {
	for _, extra := range []string{
		`"account":{"refreshToken":"secret"}`, `"refreshToken":"secret"`, `"preset":"claude"`,
		`"vertex":{"credentials":"/private/file"}`, `"proxy":"http://proxy"`, `"fallback":["local/model"]`,
		`"nativePeerSyncEnabled":true`, `"keys":[{"key":"sk-test","refreshToken":"secret"}]`,
	} {
		var cfg PeerProviderConfig
		payload := `{"sourceID":"relay","name":"API","chat":"https://example.test/v1","models":["allowed"],` + extra + `}`
		if err := json.Unmarshal([]byte(payload), &cfg); err == nil {
			t.Errorf("accepted non-whitelisted field: %s", extra)
		}
	}
}

func TestPeerSyncRejectsUnauthorizedCredentialsAndInvalidEndpoints(t *testing.T) {
	peerSyncHome(t)
	for _, modify := range []func(*PeerProviderConfig){
		func(c *PeerProviderConfig) {},
		func(c *PeerProviderConfig) { c.Key = ""; c.Keys = []PeerProviderKey{{Key: "sk-spare"}} },
		func(c *PeerProviderConfig) {
			c.Key = ""
			c.Headers = map[string]string{"Authorization": "Bearer private"}
		},
		func(c *PeerProviderConfig) { c.Key = ""; c.Headers = map[string]string{"X-Custom": "private"} },
	} {
		cfg := peerSyncConfig()
		modify(&cfg)
		if _, err := ImportPeerConfig("b", cfg, false); err == nil {
			t.Fatal("peer without credential permission imported a secret")
		}
	}
	for _, base := range []string{"", "file:///private/key", "plugin://foo/v1", "https://user:pass@example.test/v1", "https://example.test/v1?key=secret", "https://example.test/v1?", "https://example.test/v1#fragment", "https://example.test/v1#", "https:///v1", "not-a-url"} {
		cfg := peerSyncConfig()
		cfg.Chat = base
		if _, err := ImportPeerConfig("b", cfg, true); err == nil {
			t.Errorf("accepted invalid endpoint %q", base)
		}
	}
	for _, modify := range []func(*PeerProviderConfig){
		func(c *PeerProviderConfig) { c.Models = nil },
		func(c *PeerProviderConfig) { c.KeyProtocol = Anthropic },
		func(c *PeerProviderConfig) { c.Headers = map[string]string{"X-Bad\nHeader": "value"} },
		func(c *PeerProviderConfig) { c.Headers = map[string]string{"X-Header": "bad\r\nvalue"} },
	} {
		cfg := peerSyncConfig()
		modify(&cfg)
		if _, err := ImportPeerConfig("b", cfg, true); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
	if ps, err := Stored(); err != nil || len(ps) != 0 {
		t.Fatal("rejected configuration changed the provider store")
	}
}

func TestPeerSyncOnlyReplacesItsOwnNamespacedProvider(t *testing.T) {
	peerSyncHome(t)
	cfg := peerSyncConfig()
	local := Provider{ID: "sync-b-relay", Name: "Local", Chat: "https://local.example.test/v1", Key: "sk-local", Models: []string{"local"}}
	if err := storeInitial(file{Providers: []Provider{local}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportPeerConfig("b", cfg, true); err == nil {
		t.Fatal("overwrote a local provider at the namespaced id")
	}
	if got := peerSyncStored(t, local.ID); !sameStored(got, local) {
		t.Fatal("collision modified the local provider")
	}
	local.PeerSource = "different/source"
	if err := storeInitial(file{Providers: []Provider{local}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportPeerConfig("b", cfg, true); err == nil {
		t.Fatal("overwrote another peer's provider")
	}
	if err := storeInitial(file{}); err != nil {
		t.Fatal(err)
	}
	id, err := ImportPeerConfig("b", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Key, cfg.Models, cfg.Off = "sk-replacement", []string{"replacement"}, true
	if _, err := ImportPeerConfig("b", cfg, true); err != nil {
		t.Fatal(err)
	}
	p := peerSyncStored(t, id)
	ps, _ := Stored()
	if len(ps) != 1 || p.Key != cfg.Key || !p.Off || !slices.Equal(p.Models, cfg.Models) {
		t.Fatal("owned configuration was not replaced exactly")
	}
}

func TestPeerSyncModelScopeDoesNotGrowWithTheCatalog(t *testing.T) {
	peerSyncHome(t)
	cfg := peerSyncConfig()
	id, err := ImportPeerConfig("b", cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(id, cfg.Chat, []catalog.Model{{ID: "allowed", Released: "2026-01-01"}, {ID: "new-unshared", Released: "2026-02-01"}}); err != nil {
		t.Fatal(err)
	}
	p := peerSyncStored(t, id)
	if models := p.Exposed(); len(models) != 1 || models[0].ID != "allowed" {
		t.Fatalf("sync scope expanded when the vendor added a model: %v", models)
	}
}

func TestPeerSyncUnreadableStoreIsNeverReplaced(t *testing.T) {
	peerSyncHome(t)
	writeProviderFile(t, `{broken JSON`)
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportPeerConfig("b", peerSyncConfig(), true); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("import with unreadable store: %v", err)
	}
	if _, err := ExportPeerConfig("relay", []string{"allowed"}, true); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("export with unreadable store: %v", err)
	}
	after, _ := os.ReadFile(Path())
	if !bytes.Equal(before, after) {
		t.Fatal("sync overwrote an unreadable provider store")
	}
}

func TestPeerSyncConcurrentImportsKeepEveryProvider(t *testing.T) {
	peerSyncHome(t)
	const n = 16
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			cfg := peerSyncConfig()
			cfg.SourceID = fmt.Sprintf("relay-%d", i)
			_, err := ImportPeerConfig("b", cfg, true)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ps, err := Stored()
	if err != nil || len(ps) != n {
		t.Fatalf("concurrent imports lost a provider: count=%d err=%v", len(ps), err)
	}
}
