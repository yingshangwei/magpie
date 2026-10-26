package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/yetone/magpie/internal/peering"
	"golang.org/x/net/http/httpguts"
)

// NativePeerSyncEnabled is deliberately not a setting. Native Claude and
// Codex sign-ins cannot be copied by a peer, whatever either peer configures.
const NativePeerSyncEnabled = false

// PeerProviderKey is an API key only. It cannot carry a subscription account
// or acquire new fields when KeyAccount's local settings grow.
type PeerProviderKey struct {
	Name     string   `json:"name,omitempty"`
	Key      string   `json:"key"`
	Off      bool     `json:"off,omitempty"`
	Protocol Protocol `json:"protocol,omitempty"`
}

// PeerProviderConfig is the complete allow list for a configuration copy.
// Account tokens, presets, plugins, proxies, fallback and machine paths are
// not part of the wire format. Models is always an explicit, nonempty scope.
type PeerProviderConfig struct {
	SourceID    string            `json:"sourceID"`
	Name        string            `json:"name"`
	Chat        string            `json:"chat,omitempty"`
	Responses   string            `json:"responses,omitempty"`
	Anthropic   string            `json:"anthropic,omitempty"`
	Gemini      string            `json:"gemini,omitempty"`
	Models      []string          `json:"models"`
	Off         bool              `json:"off,omitempty"`
	Key         string            `json:"key,omitempty"`
	KeyName     string            `json:"keyName,omitempty"`
	KeyProtocol Protocol          `json:"keyProtocol,omitempty"`
	Keys        []PeerProviderKey `json:"keys,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// UnmarshalJSON rejects fields outside the wire allow list, including fields
// nested in a key. A caller cannot accidentally deserialize Provider here.
func (p *PeerProviderConfig) UnmarshalJSON(b []byte) error {
	type wire PeerProviderConfig
	var v wire
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	*p = PeerProviderConfig(v)
	return nil
}

// ExportPeerConfig copies only selected models of a stored API provider. It
// reads the stored catalog, never discovers or refreshes signed-in accounts.
// includeKeys also permits headers that may contain credentials.
func ExportPeerConfig(providerID string, models []string, includeKeys bool) (PeerProviderConfig, error) {
	if err := peerSourceID(providerID); err != nil {
		return PeerProviderConfig{}, err
	}
	f, err := read()
	if err != nil {
		return PeerProviderConfig{}, err
	}
	p, ok := find(f.Providers, providerID)
	if !ok {
		return PeerProviderConfig{}, fmt.Errorf("no stored API provider %q", providerID)
	}
	return exportPeerProvider(p, models, includeKeys)
}

func exportPeerProvider(p Provider, models []string, includeKeys bool) (PeerProviderConfig, error) {
	if err := peerSourceID(p.ID); err != nil {
		return PeerProviderConfig{}, err
	}
	preset := strings.ToLower(strings.TrimSpace(p.Preset))
	if !NativePeerSyncEnabled && (preset == "claude" || preset == "codex") {
		return PeerProviderConfig{}, errors.New("native Claude and Codex configuration sync is disabled in code")
	}
	if p.Account != nil || slices.Contains(accountIDs, preset) || p.Peer != "" ||
		preset == RemoteMagpiePreset || preset == VertexPreset || p.Vertex != nil {
		return PeerProviderConfig{}, errors.New("only a local API key provider can be synced")
	}
	cfg := PeerProviderConfig{
		SourceID: p.ID, Name: p.Name, Chat: p.Chat, Responses: p.Responses,
		Anthropic: p.Anthropic, Gemini: p.Gemini, Models: slices.Clone(models), Off: p.Off,
	}
	if includeKeys {
		cfg.Key, cfg.KeyName, cfg.KeyProtocol = p.Key, p.KeyName, p.KeyProtocol
		for _, k := range p.Keys {
			cfg.Keys = append(cfg.Keys, PeerProviderKey{Name: k.Name, Key: k.Key, Off: k.Off, Protocol: k.Protocol})
		}
	}
	for k, v := range p.Headers {
		if !includeKeys && !peerPublicHeader(k) {
			continue
		}
		if cfg.Headers == nil {
			cfg.Headers = map[string]string{}
		}
		cfg.Headers[k] = v
	}
	if err := cfg.check(includeKeys); err != nil {
		return PeerProviderConfig{}, err
	}
	exposed := map[string]bool{}
	for _, m := range p.Exposed() {
		exposed[m.ID] = true
	}
	for _, m := range cfg.Models {
		if !exposed[m] {
			return PeerProviderConfig{}, fmt.Errorf("model %q is not exposed by provider %q", m, p.ID)
		}
	}
	return cfg, nil
}

var peerSyncMu sync.Mutex

// ImportPeerConfig saves a copy owned by peerID/sourceID, never a same-named
// local provider. A copy without a primary API key is kept off, replacing
// any earlier credentials rather than silently borrowing them. No account
// discovery, model fetch or sign-in runs during a copy.
func ImportPeerConfig(peerID string, cfg PeerProviderConfig, allowKeys bool) (string, error) {
	if !peering.ValidID(peerID) {
		return "", errors.New("peer id must contain 1-48 lowercase letters, digits and dashes")
	}
	if err := cfg.check(allowKeys); err != nil {
		return "", err
	}
	p := Provider{
		ID: "sync-" + peerID + "-" + cfg.SourceID, PeerSource: peerID + "/" + cfg.SourceID,
		Name: strings.TrimSpace(cfg.Name), Chat: cfg.Chat, Responses: cfg.Responses,
		Anthropic: cfg.Anthropic, Gemini: cfg.Gemini, Models: slices.Clone(cfg.Models),
		Key: cfg.Key, KeyName: cfg.KeyName, KeyProtocol: cfg.KeyProtocol,
		Headers: maps.Clone(cfg.Headers), Off: cfg.Off || cfg.Key == "",
	}
	if p.Name == "" {
		p.Name = cfg.SourceID
	}
	for _, k := range cfg.Keys {
		p.Keys = append(p.Keys, KeyAccount{Name: k.Name, Key: k.Key, Off: k.Off, Protocol: k.Protocol})
	}
	peerSyncMu.Lock()
	defer peerSyncMu.Unlock()
	f, err := read()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(f.Providers, func(q Provider) bool { return q.ID == p.ID })
	if i >= 0 {
		if f.Providers[i].PeerSource != p.PeerSource {
			return "", fmt.Errorf("provider %q is not owned by this peer and source", p.ID)
		}
		f.Providers[i] = p
	} else {
		f.Providers = append(f.Providers, p)
	}
	// Save also discovers accounts and asks Available for model picks, and
	// rejects a keyless metadata copy. This narrowly constructed provider is
	// validated above and written once through the same atomic store.
	if err := store(f); err != nil {
		return "", err
	}
	return p.ID, nil
}

func peerSourceID(id string) error {
	if !NativePeerSyncEnabled && (strings.EqualFold(strings.TrimSpace(id), "claude") || strings.EqualFold(strings.TrimSpace(id), "codex")) {
		return errors.New("native Claude and Codex configuration sync is disabled in code")
	}
	if !peering.ValidID(id) {
		return errors.New("source provider id must contain 1-48 lowercase letters, digits and dashes")
	}
	if slices.Contains(accountIDs, id) || id == RemoteMagpiePreset || id == VertexPreset {
		return errors.New("subscription, remote and machine-credential providers cannot be synced")
	}
	return nil
}

func (cfg PeerProviderConfig) check(allowKeys bool) error {
	if err := peerSourceID(cfg.SourceID); err != nil {
		return err
	}
	if len(cfg.Models) == 0 || len(cfg.Models) > peering.MaxSharedModels {
		return errors.New("select 1-4096 exact models to sync")
	}
	seen := map[string]bool{}
	for _, m := range cfg.Models {
		if m == "" || len(m) > 512 || m != strings.TrimSpace(m) || strings.ContainsAny(m, "*?\r\n\x00") || seen[m] {
			return errors.New("synced models must be nonempty, distinct model ids")
		}
		seen[m] = true
	}
	endpoints := map[Protocol]string{Chat: cfg.Chat, Responses: cfg.Responses, Anthropic: cfg.Anthropic, Gemini: cfg.Gemini}
	hasURL := false
	for proto, base := range endpoints {
		if base == "" {
			continue
		}
		hasURL = true
		u, err := url.Parse(base)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(base, "#") {
			return fmt.Errorf("%s base URL must be HTTP(S) without user info, a query or a fragment", proto)
		}
	}
	if !hasURL {
		return errors.New("a synced provider needs an HTTP(S) base URL")
	}
	if !allowKeys && (cfg.Key != "" || len(cfg.Keys) != 0) {
		return errors.New("this peer is not allowed to send API keys")
	}
	if cfg.Key == "" && (len(cfg.Keys) != 0 || cfg.KeyName != "" || cfg.KeyProtocol != "") {
		return errors.New("additional keys and key settings require a primary API key")
	}
	keyOK := func(key string, proto Protocol) error {
		if key == "" || key != strings.TrimSpace(key) || strings.ContainsAny(key, "\r\n\x00") {
			return errors.New("an API key must be nonempty and contain no surrounding whitespace or line breaks")
		}
		if proto != "" && endpoints[proto] == "" {
			return errors.New("a key's protocol must have a matching base URL")
		}
		return nil
	}
	if cfg.Key != "" {
		if err := keyOK(cfg.Key, cfg.KeyProtocol); err != nil {
			return err
		}
	}
	for _, k := range cfg.Keys {
		if err := keyOK(k.Key, k.Protocol); err != nil {
			return err
		}
	}
	for k, v := range cfg.Headers {
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) {
			return errors.New("synced provider has an invalid HTTP header")
		}
		if !allowKeys && !peerPublicHeader(k) {
			return errors.New("this peer is not allowed to send headers that may contain credentials")
		}
	}
	return nil
}

// A custom header can hold a credential under any name. Only these protocol
// headers are copied without permission to send credentials; unknown ones
// require that permission just as Authorization and Cookie do.
func peerPublicHeader(name string) bool {
	switch strings.ToLower(name) {
	case "accept", "content-type", "anthropic-version", "anthropic-beta":
		return true
	}
	return false
}
