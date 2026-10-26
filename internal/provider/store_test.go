package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/filelock"
)

// storeInitial writes a deliberate test fixture through the guarded store.
func storeInitial(f file) error {
	previous, err := read()
	if err != nil {
		return err
	}
	f.revision = previous.revision
	return store(f)
}

func TestProviderStoreRejectsStaleGeneration(t *testing.T) {
	peerSyncHome(t)
	if err := Save(Provider{ID: "local", Name: "Initial", Chat: "http://127.0.0.1:1/v1", Key: "test-only", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	stale, err := read()
	if err != nil {
		t.Fatal(err)
	}
	latest, err := read()
	if err != nil {
		t.Fatal(err)
	}
	latest.Providers[0].Name = "User edit"
	if err := store(latest); err != nil {
		t.Fatal(err)
	}
	// Unrelated reads must not update the revision held by stale.
	load()
	stale.Providers[0].Models = []string{"m2"}
	before, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := store(stale); !errors.Is(err, errProvidersChanged) {
		t.Fatalf("stale write: %v", err)
	}
	if after, err := os.ReadFile(Path()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("conflict overwrote the completed user edit")
	}
	if err := store(file{}); err == nil {
		t.Fatal("a write with no checked read was allowed")
	}
}

func TestProviderStoreRefusesUnavailableLock(t *testing.T) {
	peerSyncHome(t)
	// A directory at the lock path makes acquiring a lock fail immediately.
	if err := os.MkdirAll(Path()+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := storeInitial(file{}); !errors.Is(err, errProvidersLocked) {
		t.Fatalf("write without a lock: %v", err)
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed lock wrote providers: %v", err)
	}
}

func TestProviderStoreCallbackCanSave(t *testing.T) {
	peerSyncHome(t)
	old := catalog.Changed
	t.Cleanup(func() { catalog.Changed = old })
	called := false
	catalog.Changed = func() {
		if called {
			return
		}
		called = true
		if err := Save(Provider{ID: "callback", Key: "test-only", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1"}}); err != nil {
			t.Error(err)
		}
	}
	if err := storeInitial(file{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("catalog callback did not run")
	}
	if _, err := Find("callback"); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStoreOtherProcess(t *testing.T) {
	if os.Getenv("MAGPIE_PROVIDERS_WRITER") == "" {
		t.Skip("run by TestProviderStoreKeepsOtherProcessSave")
	}
	p, err := Find("local")
	if err != nil {
		t.Fatal(err)
	}
	p.Name = "Saved by another process"
	fmt.Println("provider-writer-ready")
	if err := Save(*p); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStoreWaitsForOtherProcessLock(t *testing.T) {
	peerSyncHome(t)
	if err := Save(Provider{ID: "local", Name: "Initial", Chat: "http://127.0.0.1:1/v1", Key: "test-only", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	_, revision, err := readProviderFile()
	if err != nil {
		t.Fatal(err)
	}
	unlock, held := filelock.Lock(revision.path+".lock", time.Second)
	if !held {
		t.Fatal("could not take the parent process's lock")
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	other := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderStoreOtherProcess$", "-test.count=1")
	other.Env = append(os.Environ(), "MAGPIE_PROVIDERS_WRITER=1")
	out, err := other.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	defer func() { cancel(); <-done }()
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			if scanner.Text() == "provider-writer-ready" {
				close(ready)
			}
		}
		done <- other.Wait()
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("other process did not reach Save")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatalf("other process saved before the file lock was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if p, err := Find("local"); err != nil || p.Name != "Initial" {
		t.Fatalf("locked provider was changed: %v", err)
	}
	unlock()
	unlock = nil
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("other process did not finish after unlock")
	}
}

func TestProviderRenamePublishesMovedCatalog(t *testing.T) {
	peerSyncHome(t)
	p := Provider{ID: "local", Key: "test-only", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive(p.ID, p.Chat, []catalog.Model{{ID: "m1", Name: "Original metadata"}}); err != nil {
		t.Fatal(err)
	}
	old := catalog.Changed
	t.Cleanup(func() { catalog.Changed = old })
	called := false
	catalog.Changed = func() {
		called = true
		models, _, ok := catalog.Live("renamed")
		if !ok || len(models) != 1 || models[0].Name != "Original metadata" {
			t.Error("rename notified agents before moving the model cache")
		}
	}
	if err := Rename(p.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("rename did not publish the updated catalog")
	}
}

func TestProviderStoreKeepsOtherProcessSave(t *testing.T) {
	peerSyncHome(t)
	if err := Save(Provider{ID: "local", Name: "Initial", Chat: "http://127.0.0.1:1/v1", Key: "test-only", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	stale, err := read()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	other := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderStoreOtherProcess$", "-test.count=1")
	other.Env = append(os.Environ(), "MAGPIE_PROVIDERS_WRITER=1")
	if out, err := other.CombinedOutput(); err != nil {
		t.Fatalf("other process: %v\n%s", err, out)
	}
	stale.Providers[0].Models = []string{"m2"}
	if err := store(stale); !errors.Is(err, errProvidersChanged) {
		t.Fatalf("stale process write: %v", err)
	}
	if p, err := Find("local"); err != nil || p.Name != "Saved by another process" {
		t.Fatalf("other process's edit was lost: %v", err)
	}
}

func TestProviderStoreKeepsSymlink(t *testing.T) {
	peerSyncHome(t)
	target := filepath.Join(t.TempDir(), "providers.json")
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, Path()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := Provider{ID: "local", Key: "test-only", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	p.Name = "Edited"
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(Path()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("provider save replaced the symlink")
	}
	if _, err := os.Stat(target + ".lock"); err != nil {
		t.Fatalf("target's lock was not used: %v", err)
	}
	// The backup remains beside the configured path, where read recovers it.
	if err := os.WriteFile(target, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	Changed()
	got, err := Find(p.ID)
	if err != nil || got.Name != "local" {
		t.Fatalf("symlink backup recovery: %v", err)
	}
	got.Name = "Recovered edit"
	if err := Save(*got); err != nil {
		t.Fatalf("save after backup recovery: %v", err)
	}
}
