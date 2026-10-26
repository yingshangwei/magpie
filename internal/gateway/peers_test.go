package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/usage"
)

type peerTestArrival struct {
	path string
	head http.Header
	body []byte
}

type peerGatewayPair struct {
	a, b             *Server
	aHTTP, bHTTP     *httptest.Server
	aPolicy, bPolicy peering.Peer
	mu               sync.Mutex
	wires            []net.Conn
	arrived          []peerTestArrival
}

func peerTestPolicy(t *testing.T, path string, p peering.Peer) {
	t.Helper()
	c := peering.Config{Version: 1, Peers: []peering.Peer{p}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := edit.WriteAtomic(path, b); err != nil {
		t.Fatal(err)
	}
}

func peerTestEventually(t *testing.T, what string, ready func() bool) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// Two Server values share the test's provider store, as remote gateway tests
// already do. Each hub has its own policy file; no environment switches while
// either is running. B dials A, and A remains the active provider transport.
func newPeerGatewayPair(t *testing.T, aPolicy, bPolicy peering.Peer) *peerGatewayPair {
	t.Helper()
	p := &peerGatewayPair{a: New(), b: New()}
	aPolicy.ID, bPolicy.ID = "office", "office"
	aPolicy.Name, bPolicy.Name = "Office", "Laptop"
	aPolicy.Token = peering.NewToken()
	bPolicy.Token = aPolicy.Token
	ah, bh := p.a.peerHub(), p.b.peerHub()
	ah.Path = filepath.Join(t.TempDir(), "a-peers.json")
	bh.Path = filepath.Join(t.TempDir(), "b-peers.json")
	// Inspect what crossed the wire before B's policy/normal gateway handles it.
	serveB := bh.Handler
	bh.Handler = func(id string) http.Handler {
		next := serveB(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "test request read failed", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(b))
			p.mu.Lock()
			p.arrived = append(p.arrived, peerTestArrival{r.URL.Path, r.Header.Clone(), b})
			p.mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
	// Keep the user's HTTP connection separate from the upgraded connection,
	// so cutting the latter tests a transport failure without canceling A's caller.
	wire := httptest.NewUnstartedServer(lanGuard(p.a.Handler()))
	wire.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			p.mu.Lock()
			p.wires = append(p.wires, c)
			p.mu.Unlock()
		}
	}
	wire.Start()
	p.aHTTP = httptest.NewServer(lanGuard(p.a.Handler()))
	p.bHTTP = httptest.NewServer(lanGuard(p.b.Handler()))
	bPolicy.URL = "ws" + strings.TrimPrefix(wire.URL, "http")
	p.aPolicy, p.bPolicy = aPolicy, bPolicy
	peerTestPolicy(t, ah.Path, aPolicy)
	peerTestPolicy(t, bh.Path, bPolicy)
	ctx, cancel := context.WithCancel(context.Background())
	stopB := bh.Start(ctx)
	stopA := ah.Start(ctx)
	t.Cleanup(func() {
		cancel()
		stopA()
		stopB()
		p.a.subscription.abortAll()
		p.b.subscription.abortAll()
		p.aHTTP.Close()
		p.bHTTP.Close()
		wire.Close()
	})
	peerTestEventually(t, "both WebSocket peers connected", func() bool {
		for _, h := range []*peering.Hub{ah, bh} {
			states, err := h.Status()
			if err != nil || len(states) != 1 || !states[0].Connected {
				return false
			}
		}
		return true
	})
	if aPolicy.Import {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := p.a.refreshPeer(ctx, aPolicy); err != nil {
			t.Fatalf("initial peer catalog: %v", err)
		}
	}
	return p
}

func (p *peerGatewayPair) disconnect() {
	p.mu.Lock()
	cs := slices.Clone(p.wires)
	p.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
}

func peerTestRoundTrip(t *testing.T, h *peering.Hub, method, path, body string, modelCall bool) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := h.RoundTrip("office", r, modelCall)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

func peerTestPost(t *testing.T, endpoint, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+TokenFor("pi"))
	r.Header.Set("User-Agent", "pi/1.0")
	r.Header.Set(SessionHeader, "peer-conversation")
	r.Header.Set("Session_id", "native-peer-session")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b)
}

func peerTestSSE(tool bool) string {
	start := `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_peer","type":"message","role":"assistant","model":"m1","content":[],"usage":{"input_tokens":7}}}`
	if tool {
		return sse(start,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_peer_1","name":"lookup","input":{}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Taipei\"}"}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
	}
	return sse(start,
		`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"sunny confirmed"}}`,
		`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
		`event: message_delta`+"\n"+`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		`event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
}

func TestPeerGatewayStreamsToolsWithSessionAndScopedCatalog(t *testing.T) {
	fresh(t)
	got := make(chan peerTestArrival, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- peerTestArrival{r.URL.Path, r.Header.Clone(), b}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, peerTestSSE(!bytes.Contains(b, []byte(`"tool_result"`))))
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Name: "B Vendor", Key: "vendor-secret", Anthropic: up.URL, Models: []string{"m1", "hidden"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "other-remote", Name: "Do not relay", Preset: provider.RemoteMagpiePreset, Chat: "http://127.0.0.1:1/v1", Models: []string{"fake/m1"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "peer-other", Name: "Do not nest", Preset: provider.RemoteMagpiePreset, Peer: "other", Chat: "http://peer.invalid/v1", Models: []string{"fake/m1"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "shared", Name: "Local group", Members: []string{"fake/m1"}}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{
		{Provider: "fake", Models: []string{"m1"}}, {Provider: "other-remote", Models: []string{"fake/m1"}}, {Provider: "peer-other", Models: []string{"fake/m1"}},
	}})
	status, list := peerTestRoundTrip(t, pair.a.peerHub(), http.MethodGet, peerCatalogPath, "", true)
	var models []catalog.Model
	if status != 200 || json.Unmarshal([]byte(list), &models) != nil || len(models) != 1 || models[0].ID != "fake/m1" || !slices.Equal(models[0].APIs, []string{"anthropic"}) || len(models[0].Keys) != 0 {
		t.Fatalf("peer catalog escaped scope or lost native protocol: %d %s", status, list)
	}
	if strings.Contains(list, "vendor-secret") {
		t.Fatal("catalog exposed an upstream credential")
	}
	tools := `"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]`
	first := `{"model":"peer-office/fake/m1","max_tokens":128,"stream":true,"metadata":{"user_id":"peer-user"},` + tools + `,"messages":[{"role":"user","content":"Check the weather"}]}`
	status, answer := peerTestPost(t, pair.aHTTP.URL+"/v1/messages", first)
	if status != 200 || !strings.Contains(answer, `"tool_use"`) || !strings.Contains(answer, "toolu_peer_1") || !strings.Contains(answer, "lookup") {
		t.Fatalf("tool call did not cross both gateways: %d %s", status, answer)
	}
	second := `{"model":"peer-office/fake/m1","max_tokens":128,"stream":true,"metadata":{"user_id":"peer-user"},` + tools + `,"messages":[{"role":"user","content":"Check the weather"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_peer_1","name":"lookup","input":{"city":"Taipei"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_peer_1","content":"sunny"}]}]}`
	status, answer = peerTestPost(t, pair.aHTTP.URL+"/v1/messages", second)
	if status != 200 || !strings.Contains(answer, "sunny confirmed") || !strings.Contains(answer, "message_stop") {
		t.Fatalf("tool result did not complete through both gateways: %d %s", status, answer)
	}
	for i := range 2 {
		select {
		case v := <-got:
			if v.path != "/v1/messages" || gjsonModel(v.body) != "m1" || !bytes.Contains(v.body, []byte(`"user_id":"peer-user"`)) {
				t.Fatalf("vendor request lost protocol/model/metadata: %s %s", v.path, v.body)
			}
			if v.head.Get("X-Api-Key") != "vendor-secret" && v.head.Get("Authorization") != "Bearer vendor-secret" {
				t.Fatal("B did not use its own vendor credential")
			}
			for _, name := range []string{SessionHeader, AgentHeader, ViaHeader, "Session_id"} {
				if v.head.Get(name) != "" {
					t.Errorf("vendor received private gateway header %s", name)
				}
			}
			if i == 1 && (!bytes.Contains(v.body, []byte(`"tool_result"`)) || !bytes.Contains(v.body, []byte(`"tool_use_id":"toolu_peer_1"`))) {
				t.Fatal("tool result identity did not survive forwarding")
			}
		default:
			t.Fatal("missing vendor call")
		}
	}
	pair.mu.Lock()
	arrivals := slices.Clone(pair.arrived)
	pair.mu.Unlock()
	var modelCalls int
	for _, a := range arrivals {
		if a.path != "/v1/messages" {
			continue
		}
		modelCalls++
		if a.head.Get(SessionHeader) != "peer-conversation" || a.head.Get("Session_id") != "native-peer-session" || a.head.Get(AgentHeader) != "pi" {
			t.Fatalf("wire lost agent/session identity: %v", a.head)
		}
		if a.head.Get("Authorization") != "" || a.head.Get("X-Api-Key") != "" || a.head.Get("Cookie") != "" {
			t.Fatal("wire carried caller or vendor credentials")
		}
	}
	if modelCalls != 2 {
		t.Fatalf("model exchange count=%d, want 2", modelCalls)
	}
	defer func() {
		if t.Failed() {
			for _, r := range usage.Load(time.Time{}) {
				if r.Provider == "fake" {
					t.Logf("B usage identity: agent=%q via=%q session=%q native=%q", r.Agent, r.Via, r.Session, r.NativeSession)
				}
			}
		}
	}()
	peerTestEventually(t, "B usage record with both session identities", func() bool {
		for _, r := range usage.Load(time.Time{}) {
			if r.Provider == "fake" && r.Session == peerSessionID("office", "peer-conversation") && r.NativeSession == peerSessionID("office", "native-peer-session") && r.Agent == "pi" && r.Via == "office" {
				return true
			}
		}
		return false
	})
	for _, model := range []string{"fake/hidden", "group/shared", "other-remote/fake/m1", "peer-other/fake/m1"} {
		status, body := peerTestRoundTrip(t, pair.a.peerHub(), http.MethodPost, "/v1/messages", `{"model":`+fmt.Sprintf("%q", model)+`,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, true)
		if status != http.StatusForbidden {
			t.Errorf("out-of-scope model %s: %d %s", model, status, body)
		}
	}
	select {
	case <-got:
		t.Fatal("an out-of-scope request reached the vendor")
	default:
	}
}

func TestPeerGatewayCancelReachesTheRemoteVendor(t *testing.T) {
	fresh(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse(`data: {"id":"chunk-1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{"content":"first"}}]}`))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Key: "k", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "fake", Models: []string{"m1"}}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, pair.aHTTP.URL+"/v1/chat/completions", strings.NewReader(`{"model":"peer-office/fake/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+Token)
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("stream: %d %s", res.StatusCode, b)
	}
	// The first chunk must arrive while the vendor is still waiting: neither
	// the tunnel nor either gateway may buffer the entire response.
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "first") {
		t.Fatalf("first streamed chunk: %q, %v", line, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("reply arrived without starting the vendor request")
	}
	cancel()
	res.Body.Close()
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("client cancellation did not cancel B's vendor request")
	}
}

func TestPeerGatewayOldConnectionCannotAcquireNewGrants(t *testing.T) {
	fresh(t)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, peerTestSSE(false))
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Key: "k", Anthropic: up.URL, Models: []string{"m1", "new-grant"}}); err != nil {
		t.Fatal(err)
	}
	a, b := New(), New()
	ah, bh := a.peerHub(), b.peerHub()
	ah.Path = filepath.Join(t.TempDir(), "a-peers.json")
	bh.Path = filepath.Join(t.TempDir(), "b-peers.json")
	initial := peering.Peer{ID: "office", Name: "Laptop", Token: peering.NewToken(), Exports: []peering.Selection{{Provider: "fake", Models: []string{"m1"}}}}
	peerTestPolicy(t, bh.Path, initial)
	changed := initial
	changed.Token = peering.NewToken()
	changed.Exports = []peering.Selection{{Provider: "fake", Models: []string{"m1", "new-grant"}}}
	nextPolicy := peering.Config{Version: 1, Peers: []peering.Peer{changed}}
	if err := nextPolicy.Validate(); err != nil {
		t.Fatal(err)
	}
	policyJSON, err := json.Marshal(nextPolicy)
	if err != nil {
		t.Fatal(err)
	}
	var expand, changedAfterAuthentication atomic.Bool
	serveB := bh.Handler
	bh.Handler = func(id string) http.Handler {
		next := serveB(id)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/messages" && expand.Load() {
				grant, ok := peering.AuthenticatedPolicy(r.Context())
				if !ok || grant.Token != initial.Token || !grant.Allows("fake", "m1") || grant.Allows("fake", "new-grant") {
					http.Error(w, "request did not carry the original connection grant", http.StatusInternalServerError)
					return
				}
				// The Hub's guard has already admitted the request. Change the
				// file before the gateway checks its model and candidates.
				if err := edit.WriteAtomic(bh.Path, policyJSON); err != nil {
					http.Error(w, "test policy update failed", http.StatusInternalServerError)
					return
				}
				changedAfterAuthentication.Store(true)
			}
			next.ServeHTTP(w, r)
		})
	}
	wire := httptest.NewServer(lanGuard(b.Handler()))
	aPolicy := peering.Peer{ID: "office", Name: "Office", Token: initial.Token, Import: true, URL: "ws" + strings.TrimPrefix(wire.URL, "http")}
	peerTestPolicy(t, ah.Path, aPolicy)
	// A dials the real B listener. B intentionally has no reconciliation
	// timer, so only the request's authorization checks can reject the change.
	ah.Connected = nil
	ctx, cancel := context.WithCancel(context.Background())
	stopA := ah.Start(ctx)
	t.Cleanup(func() {
		cancel()
		stopA()
		a.subscription.abortAll()
		b.subscription.abortAll()
		wire.Close()
	})
	peerTestEventually(t, "A connected to B before policy change", func() bool {
		states, err := ah.Status()
		return err == nil && len(states) == 1 && states[0].Connected
	})
	status, body := peerTestRoundTrip(t, ah, http.MethodPost, "/v1/messages", `{"model":"fake/m1","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"allowed before change"}]}`, true)
	if status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("original grant did not reach B's vendor: %d %s, calls=%d", status, body, calls.Load())
	}
	expand.Store(true)
	status, body = peerTestRoundTrip(t, ah, http.MethodPost, "/v1/messages", `{"model":"fake/new-grant","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"new permission on an old connection"}]}`, true)
	if !changedAfterAuthentication.Load() {
		t.Fatalf("policy was not changed after the authenticated Hub guard: %d %s", status, body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("old authenticated connection used a newly granted model: vendor calls=%d, want 1", n)
	}
	if status != http.StatusForbidden || !strings.Contains(body, "authorization changed") {
		t.Fatalf("old authenticated connection was not refused after policy change: %d %s", status, body)
	}
}

type peerTestCountingTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
}

func (t *peerTestCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "peer.invalid" {
		t.calls.Add(1)
	}
	return t.next.RoundTrip(r)
}

func TestPeerGatewayDoesNotReplayAfterAnUncertainDisconnect(t *testing.T) {
	fresh(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	var once, cancelOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		once.Do(func() { close(started) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(canceled) })
	}))
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "fake", Key: "k", Chat: up.URL + "/v1", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "fake", Models: []string{"m1"}}}})
	counter := &peerTestCountingTransport{next: pair.a.client.Transport}
	pair.a.client.Transport = counter
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, pair.aHTTP.URL+"/v1/chat/completions", strings.NewReader(`{"model":"peer-office/fake/m1","messages":[{"role":"user","content":"a turn with side effects"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+Token)
	finished := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(r)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode < 400 {
				err = fmt.Errorf("disconnected request answered %d", res.StatusCode)
			}
		}
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("B never received the request")
	}
	pair.disconnect()
	select {
	case err := <-finished:
		if n := counter.calls.Load(); n != 1 {
			t.Fatalf("A sent the same model request %d times after B had already received it; want no replay", n)
		}
		if err != nil {
			t.Fatalf("outer HTTP request should return a gateway error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("A did not finish after the WebSocket disconnected")
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("disconnect left B's vendor request running")
	}
}

func TestPeerGatewaySyncNeedsReceivePermissionAndBlocksNativeAccounts(t *testing.T) {
	fresh(t)
	pair := newPeerGatewayPair(t, peering.Peer{SyncReceive: []string{"metadata", "native-api", "claude", "codex"}}, peering.Peer{})
	for _, tc := range []struct {
		source, key string
		want        int
	}{
		{"unapproved", "", http.StatusForbidden},
		{"native-api", "sk-secret", http.StatusBadRequest},
		{"claude", "", http.StatusBadRequest},
		{"codex", "", http.StatusBadRequest},
		{"metadata", "", http.StatusOK},
	} {
		cfg := provider.PeerProviderConfig{SourceID: tc.source, Name: "API", Chat: "https://api.example.test/v1", Models: []string{"m1"}, Key: tc.key}
		b, _ := json.Marshal(cfg)
		status, body := peerTestRoundTrip(t, pair.b.peerHub(), http.MethodPost, peerSyncPath, string(b), false)
		if status != tc.want {
			t.Errorf("sync %s: %d %s, want %d", tc.source, status, body, tc.want)
		}
	}
	ps, err := provider.Stored()
	if err != nil || len(ps) != 1 || ps[0].ID != "sync-office-metadata" || !ps[0].Off || ps[0].Key != "" {
		t.Fatalf("receive policy did not leave exactly one disabled metadata copy: count=%d err=%v", len(ps), err)
	}
	status, _ := peerTestRoundTrip(t, pair.b.peerHub(), http.MethodPost, peerSyncPath, `{"sourceID":"metadata","name":"API","chat":"https://api.example.test/v1","models":["m1"],"account":{"refreshToken":"do-not-copy"}}`, false)
	if status != http.StatusBadRequest {
		t.Fatal("wire accepted an account object in a config copy")
	}
}

func TestPeerGatewayManagementIsLocalCLIOnly(t *testing.T) {
	fresh(t)
	key, err := access.Update("add-key", access.Change{Name: "Normal gateway client"})
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(settings.Settings{LAN: true}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{}, peering.Peer{})
	for _, path := range []string{"/v1/peers/status", "/api/providers", "/v1/magpie/quotas", claudeMCPPath + "fake-token"} {
		status, body := peerTestRoundTrip(t, pair.b.peerHub(), http.MethodGet, path, "", false)
		if status != http.StatusForbidden {
			t.Errorf("peer reached %s: %d %s", path, status, body)
		}
	}
	for _, path := range []string{"/v1/peers/refresh", "/v1/peers/sync"} {
		status, body := peerTestRoundTrip(t, pair.b.peerHub(), http.MethodPost, path, `{"peer":"office","provider":"fake"}`, false)
		if status != http.StatusForbidden {
			t.Errorf("peer invoked %s: %d %s", path, status, body)
		}
	}
	for _, tc := range []struct {
		name, remote, host, ua, origin string
		want                           int
	}{
		{"local CLI", "127.0.0.1:5001", "127.0.0.1", "local-cli", "", http.StatusOK},
		{"remote with gateway key", "198.51.100.7:5001", "127.0.0.1", "local-cli", "", http.StatusForbidden},
		{"browser with gateway key", "127.0.0.1:5001", "127.0.0.1", "Mozilla/5.0", "", http.StatusForbidden},
		{"same-origin browser", "127.0.0.1:5001", "127.0.0.1", "", "http://127.0.0.1", http.StatusForbidden},
		{"foreign Host", "127.0.0.1:5001", "untrusted.example", "local-cli", "", http.StatusForbidden},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/peers/status", nil)
		r.RemoteAddr, r.Host = tc.remote, tc.host
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("User-Agent", tc.ua)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		lanGuard(pair.a.Handler()).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, w.Code, w.Body.String(), tc.want)
		}
		if strings.Contains(w.Body.String(), pair.aPolicy.Token) {
			t.Error("management response exposed the pairing token")
		}
	}
}

func TestPeerGatewayNativeClaudeUsesBsExistingSubscriptionBridge(t *testing.T) {
	fresh(t)
	fakeClaude(t)
	claudeDir := filepath.Join(os.Getenv("HOME"), ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := mustJSON(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "peer-test-native-access", "refreshToken": "peer-test-native-refresh",
		"expiresAt": time.Now().Add(24 * time.Hour).UnixMilli(), "subscriptionType": "max",
	}})
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), credentials, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".claude.json"), mustJSON(map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "native-peer@example.test", "accountUuid": "native-peer-account"},
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	provider.ForgetAccounts()
	t.Cleanup(provider.ForgetAccounts)
	if p, err := provider.Find("claude"); err != nil || p.Account == nil {
		t.Fatalf("fixture did not expose a native Claude subscription: %v", err)
	}
	if err := provider.Save(provider.Provider{ID: "claude", Models: []string{"claude-sonnet-5"}}); err != nil {
		t.Fatal(err)
	}
	pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "claude", Models: []string{"claude-sonnet-5"}}}})
	status, answer := peerTestPost(t, pair.aHTTP.URL+"/v1/messages", `{"model":"peer-office/claude/claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hello through the peer"}]}`)
	if status != 200 || !strings.Contains(answer, "pid ") || !strings.Contains(answer, "turn 1") {
		t.Fatalf("native Claude bridge did not answer through the tunnel: %d %s", status, answer)
	}
	for _, c := range pair.a.Recent() {
		if c.Provider == "claude" {
			t.Fatal("A invoked the native subscription itself")
		}
	}
	found := false
	for _, c := range pair.b.Recent() {
		found = found || c.Provider == "claude" && c.Status == 200
	}
	if !found {
		t.Fatal("B did not record the request on its native Claude provider")
	}
	status, list := peerTestRoundTrip(t, pair.a.peerHub(), http.MethodGet, peerCatalogPath, "", true)
	if status != 200 || strings.Contains(list, "peer-test-native-") || strings.Contains(list, "native-peer@example.test") {
		t.Fatal("native model catalog leaked sign-in material")
	}
}
