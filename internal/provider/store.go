package provider

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/lastgood"
	"github.com/yetone/magpie/internal/steady"
)

type fileRevision struct {
	path   string
	exists bool
	sum    [sha256.Size]byte
}

var (
	providersCommitMu   sync.Mutex
	errProvidersChanged = errors.New("providers changed during this edit; retry the operation")
	errProvidersLocked  = errors.New("providers could not be locked; another instance is saving or this filesystem does not support file locks")
)

func readProviderFile() ([]byte, *fileRevision, error) {
	p, err := edit.Target(Path())
	if err != nil {
		return nil, nil, err
	}
	b, err := steady.ReadFile(p)
	return b, &fileRevision{path: p, exists: !errors.Is(err, os.ErrNotExist), sum: sha256.Sum256(b)}, err
}

// commitProviders compares the generation read by this edit under the same
// process and file locks as every other provider writer. A background peer
// refresh can retry its small merge; a stale user edit must report a conflict.
func commitProviders(f file) error {
	if f.revision == nil {
		return errors.New("provider edits must start from a checked read")
	}
	providersCommitMu.Lock()
	defer providersCommitMu.Unlock()
	p, err := edit.Target(Path())
	if err != nil {
		return err
	}
	if p != f.revision.path {
		return errProvidersChanged
	}
	unlock, held := filelock.Lock(p+".lock", filelock.Wait)
	defer unlock()
	if !held {
		return errProvidersLocked
	}
	_, current, err := readProviderFile()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if *current != *f.revision {
		return errProvidersChanged
	}
	// A read group includes models its patterns matched; never store those.
	f.Groups = slices.Clone(f.Groups)
	for i := range f.Groups {
		f.Groups[i] = f.Groups[i].stored()
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// Recovery looks beside the configured path, even when it is a symlink.
	if err := lastgood.Keep(Path(), validProviders, 0o600); err != nil {
		return err
	}
	if err := writePrivate(p, append(b, '\n')); err != nil {
		return err
	}
	pruneIcons(f)
	return nil
}
