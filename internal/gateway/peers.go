package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

const peerCatalogPath = "/_peer/catalog"
const peerSyncPath = "/_peer/provider-sync"
const peerCatalogMaxBytes = 8 << 20

type peerScope struct{ id string }
type peerScopeKey struct{}

func (s *Server) peerHub() *peering.Hub {
	s.peerOnce.Do(func() {
		s.peers = peering.New(peering.Path(), s.peerHandler)
		s.peers.Connected = s.refreshPeer
	})
	return s.peers
}

// Peer policy is independent of gateway keys. A remote link has no route
// to the GUI, account management, callbacks, other peers or status data.
func (s *Server) peerHandler(id string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.peerPolicy(r, id)
		if err != nil {
			writeError(w, provider.Chat, http.StatusForbidden, err.Error())
			return
		}
		if r.URL.RawQuery != "" || r.URL.RawPath != "" {
			writeError(w, provider.Chat, http.StatusForbidden, "peer operation is not allowed")
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == peerSyncPath {
			s.receivePeerConfig(w, r, p)
			return
		}
		entries := peerEntries(p)
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case peerCatalogPath:
				writePeerJSON(w, peerCatalog(entries))
				return
			case "/v1/models":
				models := make([]map[string]any, 0, len(entries))
				for _, e := range entries {
					models = append(models, modelObject(e))
				}
				writePeerJSON(w, map[string]any{"object": "list", "data": models})
				return
			}
		}
		if r.Method != http.MethodPost || !slices.Contains([]string{
			"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens",
			"/v1/embeddings", "/v1/rerank",
		}, r.URL.Path) {
			writeError(w, provider.Chat, http.StatusForbidden, "peer operation is not allowed")
			return
		}
		if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
			writeError(w, provider.Chat, http.StatusBadRequest, "peer requests must be uncompressed JSON")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (32<<20)+1))
		if err != nil || len(body) > 32<<20 {
			writeError(w, provider.Chat, http.StatusRequestEntityTooLarge, "peer request body exceeds 32 MiB")
			return
		}
		body, model, err := requestModel(body)
		if err != nil {
			writeError(w, protoOfPath(r.URL.Path), http.StatusBadRequest, "invalid peer model request")
			return
		}
		base, _ := askedAt(model)
		base = strings.TrimSuffix(base, "[1m]")
		allowed := []string{"__no_peer_models__"}
		sharedProvider := ""
		for _, e := range entries {
			allowed = append(allowed, e.ID)
			if base == e.ID {
				sharedProvider = e.Provider.ID
			}
		}
		if sharedProvider == "" {
			writeError(w, protoOfPath(r.URL.Path), http.StatusForbidden, "model is outside this peer's shared selection")
			return
		}
		if !peerAdvisorsAllowed(body, sharedProvider, entries) {
			writeError(w, protoOfPath(r.URL.Path), http.StatusForbidden, "advisor model is outside this peer's shared selection or its tools are ambiguous")
			return
		}
		ctx := access.WithIdentity(r.Context(), access.Identity{KeyID: "peer:" + id, KeyName: p.Name, Models: allowed})
		ctx = context.WithValue(ctx, peerScopeKey{}, peerScope{id})
		q := r.Clone(ctx)
		q.Header = peerHeaders(r.Header)
		peerSessionHeaders(q.Header, id, body)
		q.Header.Set("Authorization", "Bearer "+Token)
		q.Header.Set("User-Agent", "magpie/peer")
		q.Header.Set("Content-Type", "application/json")
		q.Header.Set(ViaHeader, id)
		q.Body, q.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		q.Host, q.RemoteAddr = "127.0.0.1", "peer:"+id
		// The same native Claude bridge, account selection, adapters and
		// streaming handlers used by local clients answer the request here.
		s.Handler().ServeHTTP(w, q)
	})
}

// An advisor spends the executor's account on another model. Every such
// model must belong to the same provider and the same explicit peer grant.
func peerAdvisorsAllowed(body []byte, pid string, entries []provider.Entry) bool {
	raw, ok := peerToolField(body, "tools")
	if !ok {
		return false
	}
	if len(raw) == 0 {
		return true
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false
	}
	for _, tool := range tools {
		rawType, ok := peerToolField(tool, "type")
		if !ok {
			return false
		}
		var typ string
		if len(rawType) > 0 && json.Unmarshal(rawType, &typ) != nil {
			return false
		}
		if !strings.HasPrefix(typ, "advisor_") {
			continue
		}
		rawModel, ok := peerToolField(tool, "model")
		var model string
		if !ok || json.Unmarshal(rawModel, &model) != nil {
			return false
		}
		model = strings.TrimSuffix(strings.TrimSpace(model), "[1m]")
		found := false
		for _, e := range entries {
			if e.Provider.ID == pid && (model == e.ID || model == e.Model) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// The request was validated before this scan. Reject duplicate tool fields
// so gjson and encoding/json cannot authorize and forward different values.
func peerToolField(object []byte, name string) (json.RawMessage, bool) {
	object = trimJSON(object)
	if len(object) == 0 || object[0] != '{' {
		return nil, false
	}
	var raw json.RawMessage
	ok := true
	eachMember(object, func(key, value []byte) bool {
		field, _ := unquoteJSON(key)
		if strings.EqualFold(field, name) {
			if raw != nil {
				ok = false
				return false
			}
			raw = value
		}
		return true
	})
	return raw, ok
}

// A connection cannot acquire permissions added after it authenticated.
// Keep the same grant through the model lookup and subsequent candidates,
// while still refusing a policy revoked since request admission.
func (s *Server) peerPolicy(r *http.Request, id string) (peering.Peer, error) {
	current, err := s.peerHub().Peer(id)
	if grant, ok := peering.AuthenticatedPolicy(r.Context()); ok {
		if err != nil || grant.ID != id || !reflect.DeepEqual(grant, current) {
			return peering.Peer{}, errors.New("peer authorization changed; reconnect required")
		}
		return grant, nil
	}
	return current, err
}

func peerHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, k := range append([]string{"Accept", "Anthropic-Version", "Anthropic-Beta", AgentHeader, SessionHeader, dshCompactHeader}, sessionHeaders...) {
		if vs := in.Values(k); len(vs) > 0 {
			out[http.CanonicalHeaderKey(k)] = slices.Clone(vs)
		}
	}
	return out
}

// Sessions remain stable across requests from this link, but cannot alias
// a local conversation or a conversation belonging to a different peer.
func peerSessionID(id, session string) string {
	sum := sha256.Sum256([]byte(session))
	return "peer-" + id + "-" + hex.EncodeToString(sum[:16])
}

func peerSessionHeaders(h http.Header, id string, body []byte) {
	native, displayed := conversationID(h, body), sessionOf(h)
	if displayed == "" {
		displayed = native
	}
	for _, name := range sessionHeaders {
		if value := strings.TrimSpace(h.Get(name)); value != "" {
			h.Set(name, peerSessionID(id, value))
		}
	}
	h.Set(SessionHeader, peerSessionID(id, displayed))
	if nativeSessionOf(h) == "" {
		h.Set("X-Session-ID", peerSessionID(id, native))
	}
}

func peerEntries(p peering.Peer) []provider.Entry {
	out := []provider.Entry{}
	for _, e := range provider.Catalog() {
		if e.Group == "" && !e.Provider.IsRemoteMagpie() && e.Provider.Peer == "" && p.Allows(e.Provider.ID, e.Model) {
			out = append(out, e)
		}
	}
	return out
}

// Copy only public model metadata. Key fingerprints and runtime objects
// from the local catalog never become part of the peer protocol.
func peerCatalog(es []provider.Entry) []catalog.Model {
	out := make([]catalog.Model, 0, len(es))
	for _, e := range es {
		m := catalog.Model{ID: e.ID, Name: e.Name + " · " + e.Provider.Name, Plain: e.PlainName(),
			Efforts: e.Efforts, Context: e.Context, Output: e.PublishedOutput(), Images: e.Images, ImageInput: e.ImageInput,
			Reasoning: e.Thinks(), Fast: len(e.Tiers) > 0, Tiers: e.Tiers}
		m.InputModalities, m.OutputModalities = e.Modalities()
		for _, endpoint := range nativeEndpoints(e) {
			m.APIs = append(m.APIs, map[string]string{"/v1/chat/completions": "chat", "/v1/responses": "responses", "/v1/messages": "anthropic"}[endpoint])
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) peerCandidateAllowed(r *http.Request, p provider.Provider, model string, body []byte) bool {
	scope, ok := r.Context().Value(peerScopeKey{}).(peerScope)
	if !ok {
		return true
	}
	policy, err := s.peerPolicy(r, scope.id)
	return err == nil && p.Peer == "" && !p.IsRemoteMagpie() && policy.Allows(p.ID, model) && peerAdvisorsAllowed(body, p.ID, peerEntries(policy))
}

func (s *Server) refreshPeer(ctx context.Context, peer peering.Peer) error {
	if !peer.Import {
		return errors.New("importing this peer's models is disabled")
	}
	id := "peer-" + peer.ID
	if err := provider.FileError(); err != nil {
		return err
	}
	if old, err := provider.Find(id); err == nil && old.Peer != peer.ID {
		return errors.New("peer provider id is already owned by a local provider")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, peerCatalogPath, nil)
	res, err := s.peerHub().RoundTrip(peer.ID, req, true)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("peer catalog returned %d", res.StatusCode)
	}
	var models []catalog.Model
	if err := decodePeerJSONLimit(res.Body, &models, peerCatalogMaxBytes); err != nil {
		return err
	}
	if len(models) > peering.MaxSharedModels {
		return errors.New("peer catalog is too large")
	}
	seen := map[string]bool{}
	for i := range models {
		m := &models[i]
		pid, mid, ok := strings.Cut(m.ID, "/")
		if !ok || !peering.ValidID(pid) || mid == "" || len(m.ID) > 600 || strings.ContainsAny(m.ID, "*?\r\n\x00") || seen[m.ID] {
			return errors.New("peer catalog has an invalid or duplicate model id")
		}
		seen[m.ID] = true
		m.Keys, m.Provider = nil, ""
	}
	// Writes from different peers (catalogs and provider sync) serialize.
	// Never hold this lock while waiting for the remote connection.
	s.peerEditMu.Lock()
	defer s.peerEditMu.Unlock()
	current, err := s.peerHub().Peer(peer.ID)
	if err != nil || !current.Import {
		return errors.New("peer import was disabled during refresh")
	}
	return provider.RefreshPeerCatalog(peer.ID, peer.Name, models)
}

func (s *Server) receivePeerConfig(w http.ResponseWriter, r *http.Request, peer peering.Peer) {
	var cfg provider.PeerProviderConfig
	if err := decodePeerJSON(r.Body, &cfg); err != nil {
		writeError(w, provider.Chat, http.StatusBadRequest, "invalid provider sync payload")
		return
	}
	if !slices.Contains(peer.SyncReceive, cfg.SourceID) {
		writeError(w, provider.Chat, http.StatusForbidden, "receiving this provider's configuration is not enabled")
		return
	}
	s.peerEditMu.Lock()
	defer s.peerEditMu.Unlock()
	// Recheck after the lock: permission may have been revoked meanwhile.
	peer, err := s.peerPolicy(r, peer.ID)
	if err != nil || !slices.Contains(peer.SyncReceive, cfg.SourceID) {
		writeError(w, provider.Chat, http.StatusForbidden, "provider sync permission was revoked")
		return
	}
	id, err := provider.ImportPeerConfig(peer.ID, cfg, peer.ReceiveKeys)
	if err != nil {
		writeError(w, provider.Chat, http.StatusBadRequest, err.Error())
		return
	}
	writePeerJSON(w, map[string]string{"id": id})
}

func decodePeerJSON(r io.Reader, out any) error {
	return decodePeerJSONLimit(r, out, peering.MaxConfigBytes)
}

func decodePeerJSONLimit(r io.Reader, out any, limit int) error {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil || len(b) > limit {
		return errors.New("peer payload is too large or unreadable")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid peer JSON")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("peer payload must contain one JSON value")
	}
	return nil
}

func writePeerJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

// Local CLI controls cannot be invoked over a tunnel or from a browser,
// even with a valid normal gateway key. They return no pairing/key values.
func (s *Server) peerControl(w http.ResponseWriter, r *http.Request) {
	if !local(r) || fromBrowser(r) || !loopbackHost(r.Host) {
		writeError(w, provider.Chat, http.StatusForbidden, "peer controls are local CLI operations")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/peers/status" {
		states, err := s.peerHub().Status()
		if err != nil {
			writeError(w, provider.Chat, http.StatusServiceUnavailable, err.Error())
			return
		}
		writePeerJSON(w, states)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, provider.Chat, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var q struct {
		Peer     string `json:"peer"`
		Provider string `json:"provider,omitempty"`
		Revision string `json:"revision,omitempty"`
	}
	if err := decodePeerJSON(r.Body, &q); err != nil {
		writeError(w, provider.Chat, http.StatusBadRequest, err.Error())
		return
	}
	peer, err := s.peerHub().Peer(q.Peer)
	if err != nil {
		writeError(w, provider.Chat, http.StatusBadRequest, err.Error())
		return
	}
	if q.Revision != "" && q.Revision != peering.Revision(peer) {
		writeError(w, provider.Chat, http.StatusConflict, "Device settings changed; reopen the device before saving")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if r.URL.Path == "/v1/peers/refresh" {
		if err := s.refreshPeer(ctx, peer); err != nil {
			writeError(w, provider.Chat, http.StatusBadGateway, err.Error())
			return
		}
		writePeerJSON(w, map[string]bool{"ok": true})
		return
	}
	if r.URL.Path != "/v1/peers/sync" {
		writeError(w, provider.Chat, http.StatusNotFound, "unknown peer operation")
		return
	}
	var grant *peering.Selection
	for _, g := range peer.SyncSend {
		if g.Provider == q.Provider {
			grant = &g
			break
		}
	}
	if grant == nil {
		writeError(w, provider.Chat, http.StatusForbidden, "sending this provider's configuration is not enabled")
		return
	}
	cfg, err := provider.ExportPeerConfig(grant.Provider, grant.Models, grant.Keys)
	if err != nil {
		writeError(w, provider.Chat, http.StatusBadRequest, err.Error())
		return
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, provider.Chat, http.StatusInternalServerError, "cannot encode provider configuration")
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, peerSyncPath, bytes.NewReader(b))
	res, err := s.peerHub().RoundTrip(peer.ID, req, false)
	if err != nil {
		writeError(w, provider.Chat, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// Do not reflect a remote error body that could contain credentials.
		writeError(w, provider.Chat, http.StatusBadGateway, fmt.Sprintf("peer refused provider sync (%d); check receive policy and native-provider restrictions", res.StatusCode))
		return
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := decodePeerJSON(res.Body, &result); err != nil || result.ID != "sync-"+peer.ID+"-"+grant.Provider {
		writeError(w, provider.Chat, http.StatusBadGateway, "peer returned an invalid sync result")
		return
	}
	writePeerJSON(w, result)
}
