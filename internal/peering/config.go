// Package peering owns explicit links between gateways. Its file contains
// pairing secrets and local policy, never an upstream subscription's login.
package peering

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

const ConnectPath = "/v1/peers/connect"
const IDHeader = "X-Peer-ID"
const MaxConfigBytes = 1 << 20
const MaxSharedModels = 4096

// Selection is an exact list. Neither a missing list nor '*' means all.
type Selection struct {
	Provider string   `json:"provider"`
	Models   []string `json:"models"`
	Keys     bool     `json:"keys,omitempty"` // only used by SyncSend
}

type Peer struct {
	ID          string      `json:"id"` // the same link id at both ends
	Name        string      `json:"name,omitempty"`
	URL         string      `json:"url,omitempty"` // only the dialing end needs it
	Token       string      `json:"token"`
	Off         bool        `json:"off,omitempty"`
	Import      bool        `json:"import,omitempty"`
	Exports     []Selection `json:"exports,omitempty"`
	SyncSend    []Selection `json:"syncSend,omitempty"`
	SyncReceive []string    `json:"syncReceive,omitempty"` // source provider ids
	ReceiveKeys bool        `json:"receiveKeys,omitempty"`
}

type Config struct {
	Version int    `json:"version"`
	Peers   []Peer `json:"peers"`
}

// Revision binds a local editor or sync preview to the policy it showed.
func Revision(p Peer) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Path() string { return filepath.Join(appdir.Config(), "peers.json") }

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func ValidID(id string) bool { return len(id) <= 48 && idPattern.MatchString(id) }

func NewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Decode rejects unknown fields so a misspelled policy cannot grant access.
func Decode(b []byte) (Config, error) {
	var c Config
	if len(b) > MaxConfigBytes {
		return c, errors.New("peer configuration exceeds 1 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid peer configuration: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, errors.New("peer configuration must contain one JSON object")
	}
	return c, c.Validate()
}

func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{Version: 1}, nil
	}
	if err != nil {
		return Config{}, errors.New("cannot read peer configuration")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return Config{}, errors.New("cannot read peer configuration")
	}
	return Decode(b)
}

var configMu = filelock.Mutex{Path: func() string { return Path() + ".lock" }}

// Save refuses to replace an unreadable configuration. Pairing policy is
// local to this computer and is not part of backup/setup synchronization.
func Save(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return Update(func(current *Config) error { *current = c; return nil })
}

// Update reads and edits under the same cross-process lock as Save. A
// device editor must not overwrite another device saved in the meantime.
func Update(change func(*Config) error) error {
	configMu.Lock()
	defer configMu.Unlock()
	c, err := Load(Path())
	if err != nil {
		return err
	}
	if err := change(&c); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > MaxConfigBytes {
		return errors.New("peer configuration exceeds 1 MiB")
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		return err
	}
	// Create the target privately before WriteAtomic preserves its mode.
	f, err := os.OpenFile(Path(), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(Path(), 0o600); err != nil {
		return err
	}
	return edit.WriteAtomic(Path(), append(b, '\n'))
}

func (c Config) Validate() error {
	if c.Version != 1 || len(c.Peers) > 32 {
		return errors.New("peer configuration needs version 1 and at most 32 links")
	}
	seen := map[string]bool{}
	for _, p := range c.Peers {
		if !ValidID(p.ID) || seen[p.ID] {
			return errors.New("peer ids must be unique lowercase letters and digits separated by single dashes (1-48 characters)")
		}
		seen[p.ID] = true
		if len(p.Name) > 120 || strings.ContainsAny(p.Name, "\r\n") {
			return errors.New("peer name is too long or contains a newline")
		}
		if len(p.Token) < 32 || len(p.Token) > 256 || strings.ContainsAny(p.Token, " \r\n\t") {
			return fmt.Errorf("peer %s needs a token of 32-256 non-whitespace characters", p.ID)
		}
		if p.URL != "" {
			if _, err := DialURL(p.URL); err != nil {
				return fmt.Errorf("peer %s: %w", p.ID, err)
			}
		}
		for _, selections := range [][]Selection{p.Exports, p.SyncSend} {
			providers := map[string]bool{}
			for _, s := range selections {
				if !ValidID(s.Provider) || providers[s.Provider] || s.Provider == "group" || len(s.Models) == 0 || len(s.Models) > MaxSharedModels {
					return fmt.Errorf("peer %s needs unique provider selections with explicit models", p.ID)
				}
				providers[s.Provider] = true
				models := map[string]bool{}
				for _, m := range s.Models {
					if m == "" || strings.TrimSpace(m) != m || len(m) > 512 || strings.ContainsAny(m, "*?\r\n\x00") || models[m] {
						return fmt.Errorf("peer %s has an invalid or duplicate model selection", p.ID)
					}
					models[m] = true
				}
			}
		}
		sharedModels := 0
		for _, s := range p.Exports {
			sharedModels += len(s.Models)
			if s.Keys {
				return errors.New("model sharing never exports keys; keys is only valid in syncSend")
			}
		}
		if sharedModels > MaxSharedModels {
			return errors.New("a peer can export at most 4096 models in total")
		}
		for _, id := range p.SyncReceive {
			if !ValidID(id) {
				return errors.New("syncReceive needs exact provider ids")
			}
		}
	}
	return nil
}

// DialURL permits plaintext only on literal loopback (including an SSH
// tunnel). Across a network WSS and a verified server certificate are required.
func DialURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", errors.New("peer URL must be a ws(s) address without credentials, query or fragment")
	}
	if u.Scheme != "wss" && (u.Scheme != "ws" || !loopback(u.Hostname())) {
		return "", errors.New("use wss for network peers; ws is allowed only on loopback")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = ConnectPath
	}
	if u.Path != ConnectPath {
		return "", fmt.Errorf("peer URL path must be %s", ConnectPath)
	}
	return u.String(), nil
}

func loopback(host string) bool {
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}

func (c Config) Find(id string) (Peer, bool) {
	for _, p := range c.Peers {
		if p.ID == id && !p.Off {
			return p, true
		}
	}
	return Peer{}, false
}

func (p Peer) Auth(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(p.Token), []byte(token)) == 1
}

func (p Peer) Allows(provider, model string) bool {
	for _, s := range p.Exports {
		if s.Provider == provider {
			for _, m := range s.Models {
				if m == model {
					return true
				}
			}
		}
	}
	return false
}
