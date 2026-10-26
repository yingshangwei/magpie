package peering

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/peerwire"
)

// Hub owns connections, but the gateway owns the narrow set of operations
// each peer may perform. Policy is read again for every operation.
type Hub struct {
	Path        string
	Handler     func(string) http.Handler
	Connected   func(context.Context, Peer) error
	mu          sync.Mutex
	links       map[string]*link
	states      map[string]State
	workers     map[string]worker
	ctx         context.Context
	run         *hubRun
	stopping    bool
	configErr   bool
	workersDone sync.WaitGroup
}

type link struct {
	conn   *peerwire.Conn
	policy Peer
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}
type hubRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type worker struct {
	policy Peer
	cancel context.CancelFunc
}

type State struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	Import    bool   `json:"import"`
	Status    string `json:"status"`
}

func New(path string, handler func(string) http.Handler) *Hub {
	return &Hub{Path: path, Handler: handler, links: map[string]*link{}, states: map[string]State{}, workers: map[string]worker{}}
}

var active atomic.Pointer[Hub]

type authenticatedPolicyKey struct{}

// AuthenticatedPolicy returns the local policy that authenticated this
// connection. Handlers must keep using this scope if configuration changes
// after their current-policy check; its slices must be treated as read-only.
func AuthenticatedPolicy(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(authenticatedPolicyKey{}).(Peer)
	return p, ok
}

// Start runs one reconciliation loop for the gateway's lifetime. A changed
// policy or token closes that link; reconnect never replays a model request.
func (h *Hub) Start(ctx context.Context) func() {
	h.mu.Lock()
	if running := h.run; running != nil {
		h.mu.Unlock()
		return func() { running.cancel(); <-running.done }
	}
	ctx, cancel := context.WithCancel(ctx)
	running := &hubRun{cancel: cancel, done: make(chan struct{})}
	h.run = running
	h.ctx = ctx
	h.stopping = false
	active.Store(h)
	h.mu.Unlock()
	go func() {
		defer func() {
			h.mu.Lock()
			active.CompareAndSwap(h, nil)
			h.run = nil
			close(running.done)
			h.mu.Unlock()
		}()
		h.reconcile(ctx)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				h.closeAll()
				return
			case <-tick.C:
				h.reconcile(ctx)
			}
		}
	}()
	return func() {
		cancel()
		<-running.done
	}
}

func (h *Hub) Peer(id string) (Peer, error) {
	c, err := Load(h.Path)
	if err != nil {
		return Peer{}, errors.New("peer configuration is unreadable; access refused")
	}
	p, ok := c.Find(id)
	if !ok {
		return Peer{}, errors.New("peer is disabled or not configured")
	}
	return p, nil
}

func (h *Hub) reconcile(ctx context.Context) {
	c, err := Load(h.Path)
	h.mu.Lock()
	h.configErr = err != nil
	var closing []context.CancelFunc
	for id, w := range h.workers {
		p, ok := c.Find(id)
		if err != nil || !ok || !reflect.DeepEqual(p, w.policy) {
			w.cancel()
			delete(h.workers, id)
		}
	}
	for id, l := range h.links {
		p, ok := c.Find(id)
		if err != nil || !ok || !reflect.DeepEqual(p, l.policy) {
			closing = append(closing, l.cancel)
		}
	}
	if err == nil {
		for _, p := range c.Peers {
			if !p.Off && p.URL != "" {
				if _, ok := h.workers[p.ID]; !ok {
					wc, cancel := context.WithCancel(ctx)
					h.workers[p.ID] = worker{p, cancel}
					h.workersDone.Add(1)
					go func() { defer h.workersDone.Done(); h.dialLoop(wc, p) }()
				}
			}
		}
	}
	h.mu.Unlock()
	for _, cancel := range closing {
		cancel()
	}
}

func (h *Hub) closeAll() {
	h.mu.Lock()
	h.stopping = true
	var connections []*link
	for _, l := range h.links {
		connections = append(connections, l)
	}
	for _, w := range h.workers {
		w.cancel()
	}
	h.workers = map[string]worker{}
	h.mu.Unlock()
	for _, l := range connections {
		l.cancel()
	}
	for _, l := range connections {
		<-l.done
	}
	h.workersDone.Wait()
}

func (h *Hub) dialLoop(ctx context.Context, p Peer) {
	u, err := DialURL(p.URL)
	if err != nil {
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		if l, _ := h.reserve(ctx, p); l != nil {
			h.setState(p, false, "connecting")
			dialCtx, cancel := context.WithTimeout(l.ctx, 15*time.Second)
			client := &http.Client{Transport: netproxy.Dispatch(&http.Transport{Proxy: netproxy.Func, DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			conn, response, err := websocket.Dial(dialCtx, u, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + p.Token}, IDHeader: {p.ID}}})
			cancel()
			if err == nil {
				backoff = time.Second
				h.serveConn(l, conn)
			} else {
				status := "connection failed"
				if response != nil {
					status = fmt.Sprintf("connection refused (%d)", response.StatusCode)
				}
				h.setState(p, false, status)
			}
			client.CloseIdleConnections()
			h.release(l)
		}
		wait := time.NewTimer(backoff + time.Duration(rand.Int64N(int64(backoff/2+1))))
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// The slot covers dialing, the HTTP upgrade and the protocol handshake, so a
// peer cannot start unbounded readers by leaving its version hello unfinished.
func (h *Hub) reserve(ctx context.Context, p Peer) (*link, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping || ctx.Err() != nil || (h.ctx != nil && h.ctx.Err() != nil) {
		return nil, http.StatusServiceUnavailable
	}
	if h.links[p.ID] != nil {
		return nil, http.StatusConflict
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &link{policy: p, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	h.links[p.ID] = l
	return l, 0
}

func (h *Hub) release(l *link) {
	l.cancel()
	h.mu.Lock()
	if h.links[l.policy.ID] == l {
		delete(h.links, l.policy.ID)
		if l.conn != nil {
			p := l.policy
			h.states[p.ID] = State{p.ID, p.Name, false, p.Import, "disconnected"}
		}
	}
	close(l.done)
	h.mu.Unlock()
}

// ServeHTTP authenticates before upgrading. Pairing tokens are accepted
// only here, never as gateway/admin keys, and browser-initiated links fail.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != ConnectPath || r.URL.RawQuery != "" || r.URL.RawPath != "" || r.Header.Get("Origin") != "" {
		http.Error(w, "peer connection refused", http.StatusForbidden)
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if r.TLS == nil && !loopback(host) {
		http.Error(w, "peer connections require TLS", http.StatusForbidden)
		return
	}
	p, err := h.Peer(r.Header.Get(IDHeader))
	if err != nil || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || !p.Auth(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
		http.Error(w, "peer authentication failed", http.StatusUnauthorized)
		return
	}
	h.mu.Lock()
	ctx := h.ctx
	h.mu.Unlock()
	if ctx == nil {
		ctx = r.Context()
	}
	l, code := h.reserve(ctx, p)
	if l == nil {
		http.Error(w, "peer connection unavailable", code)
		return
	}
	defer h.release(l)
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	h.serveConn(l, conn)
}

func (h *Hub) serveConn(l *link, ws *websocket.Conn) {
	p := l.policy
	ctx, cancel := context.WithCancel(l.ctx)
	defer cancel()
	next := h.Handler(p.ID)
	if next == nil {
		next = http.NotFoundHandler()
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := h.Peer(p.ID)
		if ctx.Err() != nil || err != nil || !reflect.DeepEqual(current, p) {
			http.Error(w, "peer authorization changed; reconnect required", http.StatusForbidden)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), authenticatedPolicyKey{}, p))
		next.ServeHTTP(w, r)
	})
	c := peerwire.New(ctx, ws, handler, peerwire.Options{})
	defer c.Close()
	ready, stop := context.WithTimeout(ctx, 10*time.Second)
	err := c.Ready(ready)
	stop()
	if err != nil {
		h.setState(p, false, "protocol handshake failed")
		return
	}
	current, err := h.Peer(p.ID)
	if err != nil || !reflect.DeepEqual(current, p) {
		h.setState(p, false, "configuration changed during handshake")
		return
	}
	h.mu.Lock()
	if ctx.Err() != nil || h.links[p.ID] != l {
		h.mu.Unlock()
		return
	}
	l.conn = c
	h.states[p.ID] = State{p.ID, p.Name, true, p.Import, "connected"}
	h.mu.Unlock()
	// Catalog refresh is outside the WS reader, bounded and canceled on
	// disconnect. It also propagates revocations without a process restart.
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			if h.Connected != nil && p.Import {
				fetch, finish := context.WithTimeout(ctx, 20*time.Second)
				err := h.Connected(fetch, p)
				finish()
				if err != nil && ctx.Err() == nil {
					h.setState(p, true, "connected; catalog refresh failed")
				} else if err == nil {
					h.setState(p, true, "connected")
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-c.Done():
	}
	cancel()
	<-refreshDone
}

func (h *Hub) setState(p Peer, connected bool, status string) {
	h.mu.Lock()
	h.states[p.ID] = State{p.ID, p.Name, connected, p.Import, status}
	h.mu.Unlock()
}

func (h *Hub) Status() ([]State, error) {
	c, err := Load(h.Path)
	if err != nil {
		return nil, errors.New("peer configuration is unreadable")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]State, 0, len(c.Peers))
	for _, p := range c.Peers {
		s := h.states[p.ID]
		if s.ID == "" {
			s = State{p.ID, p.Name, false, p.Import, "waiting for connection"}
		}
		if p.Off {
			s.Connected, s.Status = false, "disabled"
		}
		out = append(out, s)
	}
	return out, nil
}

// RoundTrip addresses a peer's operation, never a host named by the request.
// modelCall distinguishes use of an imported provider from local sync control.
func (h *Hub) RoundTrip(id string, r *http.Request, modelCall bool) (*http.Response, error) {
	p, err := h.Peer(id)
	if err != nil {
		return nil, err
	}
	if modelCall && !p.Import {
		return nil, errors.New("importing this peer's models is disabled")
	}
	h.mu.Lock()
	l := h.links[id]
	var conn *peerwire.Conn
	if l != nil {
		conn = l.conn
	}
	h.mu.Unlock()
	if l == nil || conn == nil || l.ctx.Err() != nil || !reflect.DeepEqual(p, l.policy) {
		return nil, errors.New("peer is offline or its configuration changed")
	}
	q := r.Clone(r.Context())
	q.URL.Scheme, q.URL.Host, q.Host, q.RequestURI = "", "", "", ""
	q.Header.Del("Authorization")
	q.Header.Del("X-Api-Key")
	q.Header.Del("Cookie")
	return conn.RoundTrip(q)
}

type transport struct{ id string }

func Transport(id string) http.RoundTripper { return transport{id} }

func (t transport) RoundTrip(r *http.Request) (res *http.Response, err error) {
	defer func() {
		noteFailure(r.Context(), err)
		if err == nil && res != nil && res.Body != nil {
			res.Body = observedBody{res.Body, r.Context()}
		}
	}()
	if r.URL.Host != "peer.invalid" || r.URL.Scheme != "http" {
		return nil, errors.New("peer transport refuses an external URL")
	}
	h := active.Load()
	if h == nil {
		return nil, errors.New("peer gateway is not running")
	}
	return h.RoundTrip(t.id, r, true)
}
