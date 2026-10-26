package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/peering"
)

func TestPeerCLIPrivateTemplateAndStrictApply(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "pair.json")
	if err := peerCmd([]string{"init", "office", path}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := peering.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Peers) != 1 || c.Peers[0].Import || len(c.Peers[0].Exports) != 0 {
		t.Fatal("template grants access by default")
	}
	if err := peerCmd([]string{"init", "office", path}); err == nil {
		t.Fatal("overwrote pairing token")
	}
	if err := peerCmd([]string{"apply", path}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(peering.Path())
	for _, args := range [][]string{{"apply", path + ".missing"}, {"init", "office", "--wrong"}, {"apply", "--wrong"}, {"sync", "office"}, {"refresh", "office", "extra"}, {"unknown"}} {
		if err := peerCmd(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	after, _ := os.ReadFile(peering.Path())
	if string(before) != string(after) {
		t.Fatal("invalid command changed policy")
	}
	if !strings.Contains(helpFor([]string{"peer", "--help"}), "peer sync") {
		t.Fatal("peer help missing")
	}
}
