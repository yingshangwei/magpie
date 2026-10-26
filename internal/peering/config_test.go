package peering

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestPeerConfigurationFailsClosed(t *testing.T) {
	good := Config{Version: 1, Peers: []Peer{{ID: "office", Token: NewToken(), Exports: []Selection{{Provider: "claude", Models: []string{"claude-sonnet-5"}}}}}}
	b, _ := json.Marshal(good)
	if _, err := Decode(b); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Version = 0 },
		func(c *Config) { c.Peers = append(c.Peers, c.Peers[0]) },
		func(c *Config) { c.Peers[0].ID = "home--link" },
		func(c *Config) { c.Peers[0].ID = "home-" },
		func(c *Config) { c.Peers[0].ID = strings.Repeat("a", 49) },
		func(c *Config) { c.Peers[0].Token = "short" },
		func(c *Config) { c.Peers[0].Exports[0].Models = nil },
		func(c *Config) { c.Peers[0].Exports[0].Models = []string{"*"} },
		func(c *Config) { c.Peers[0].Exports[0].Keys = true },
		func(c *Config) { c.Peers[0].SyncReceive = []string{"*"} },
		func(c *Config) { c.Peers[0].URL = "ws://203.0.113.1:3425" },
	} {
		c, _ := Decode(b)
		change(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted invalid policy: %+v", c.Peers[0].Exports)
		}
	}
	for _, bad := range []string{`{}`, `{"version":1,"peers":[],"exportAll":true}`, string(b) + ` {}`, `{"version":1,`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Fatal("accepted invalid JSON policy")
		}
	}
	if good.Peers[0].Allows("codex", "claude-sonnet-5") || good.Peers[0].Allows("claude", "other") || !good.Peers[0].Allows("claude", "claude-sonnet-5") {
		t.Fatal("selection did not hold exact provider and model")
	}
}

func TestPeerURLRequiresTLSOutsideLoopback(t *testing.T) {
	for _, raw := range []string{"wss://example.test", "ws://127.0.0.1:3500", "ws://[::1]:3500", "ws://localhost:3500"} {
		got, err := DialURL(raw)
		if err != nil || !strings.HasSuffix(got, ConnectPath) {
			t.Fatalf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"http://localhost", "ws://example.test", "wss://u:p@example.test", "wss://example.test?token=secret", "wss://example.test?", "wss://example.test/#token", "wss://example.test/admin", "wss://example.test/%76"} {
		if _, err := DialURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestPeerExportCountIsBoundedAcrossProviders(t *testing.T) {
	models := make([]string, MaxSharedModels)
	for i := range models {
		models[i] = fmt.Sprintf("m%d", i)
	}
	c := Config{Version: 1, Peers: []Peer{{ID: "office", Token: NewToken(),
		Exports: []Selection{{Provider: "one", Models: models}}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Peers[0].Exports = append(c.Peers[0].Exports, Selection{Provider: "two", Models: []string{"another"}})
	if err := c.Validate(); err == nil {
		t.Fatal("accepted more models than the receiving catalog supports")
	}
}

func TestPeerConfigPrivateAndDoesNotOverwriteUnreadable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	c := Config{Version: 1, Peers: []Peer{{ID: "office", Token: NewToken()}}}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(Path())
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("pairing secrets readable by others: %v", info.Mode())
	}
	if err := os.WriteFile(Path(), []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(c); err == nil {
		t.Fatal("overwrote malformed config")
	}
	b, _ := os.ReadFile(Path())
	if string(b) != `{"version":` {
		t.Fatal("malformed config changed")
	}
}
