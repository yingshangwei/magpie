package peering

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/yetone/magpie/internal/peerwire"
)

func writeConfig(t *testing.T, path string, p Peer) {
	t.Helper()
	b, _ := json.Marshal(Config{Version: 1, Peers: []Peer{p}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPeerUpgradeRequiresDedicatedTokenAndNoBrowser(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken()}
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	})
	for _, tc := range []struct {
		token, origin, addr string
		code                int
	}{
		{"", "", "127.0.0.1:1", 401},
		{"Bearer wrong", "", "127.0.0.1:1", 401},
		{"Bearer " + p.Token, "null", "127.0.0.1:1", 403},
		{"Bearer " + p.Token, "http://localhost", "127.0.0.1:1", 403},
		{"Bearer " + p.Token, "", "203.0.113.1:1", 403},
	} {
		r := httptest.NewRequest("GET", ConnectPath, nil)
		r.RemoteAddr = tc.addr
		r.Header.Set(IDHeader, p.ID)
		r.Header.Set("Authorization", tc.token)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("got %d want %d", w.Code, tc.code)
		}
	}
}

func TestPeerHubReconnectsButNeverReplaysAndRevokesImmediately(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken(), Import: true}
	var calls atomic.Int32
	connections := make(chan *peerwire.Conn, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+p.Token || r.Header.Get(IDHeader) != p.ID {
			http.Error(w, "bad auth", 401)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := peerwire.New(ctx, ws, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			io.WriteString(w, "answer")
		}), peerwire.Options{})
		defer c.Close()
		select {
		case connections <- c:
		case <-ctx.Done():
			return
		}
		<-c.Done()
	}))
	defer server.Close()
	p.URL = "ws" + strings.TrimPrefix(server.URL, "http")
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler { return http.NotFoundHandler() })
	ready := make(chan struct{}, 4)
	h.Connected = func(context.Context, Peer) error {
		select {
		case ready <- struct{}{}:
		default:
		}
		return nil
	}
	stop := h.Start(ctx)
	defer stop()
	wait := func() {
		t.Helper()
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("peer never connected")
		}
	}
	wait()
	first := <-connections
	req, _ := http.NewRequestWithContext(ctx, "POST", "/v1/messages", strings.NewReader(`{}`))
	res, err := h.RoundTrip(p.ID, req, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(b) != "answer" {
		t.Fatalf("%q %v", b, err)
	}
	first.Close()
	wait()
	if calls.Load() != 1 {
		t.Fatal("reconnection replayed the completed request")
	}
	// Revocation is read on each call, without waiting for the two-second loop.
	p.Import = false
	writeConfig(t, path, p)
	req, _ = http.NewRequestWithContext(ctx, "POST", "/v1/messages", strings.NewReader(`{}`))
	if res, err = h.RoundTrip(p.ID, req, true); err == nil {
		res.Body.Close()
		t.Fatal("revoked import still works")
	}
	if calls.Load() != 1 {
		t.Fatal("revoked request reached the remote")
	}
	if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequestWithContext(ctx, "POST", "/_peer/provider-sync", strings.NewReader(`{}`))
	if res, err = h.RoundTrip(p.ID, req, false); err == nil {
		res.Body.Close()
		t.Fatal("corrupt config allowed control request")
	}
}

func peerDial(ctx context.Context, server string, p Peer) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(server, "http")+ConnectPath, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + p.Token}, IDHeader: {p.ID}},
	})
}

func TestPeerHubReservesBeforeProtocolHandshake(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken(), Import: true}
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler { return http.NotFoundHandler() })
	returned := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		returned <- struct{}{}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, _, err := peerDial(ctx, server.URL, p)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseNow()
	// Keep the first socket open without sending the peerwire version hello.
	second, response, err := peerDial(ctx, server.URL, p)
	if err == nil {
		second.CloseNow()
		t.Fatal("accepted another socket before the first protocol handshake")
	}
	if response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("second connection: response=%v error=%v", response, err)
	}
	select {
	case <-returned: // The rejected second upgrade has finished.
	case <-ctx.Done():
		t.Fatal("rejected upgrade did not finish")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/_peer/catalog", nil)
	if res, err := h.RoundTrip(p.ID, req, false); err == nil {
		res.Body.Close()
		t.Fatal("pending handshake accepted an outgoing operation")
	}
	first.CloseNow()
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("closed pending handshake did not finish")
	}
	third, _, err := peerDial(ctx, server.URL, p)
	if err != nil {
		t.Fatalf("closed handshake did not release its slot: %v", err)
	}
	third.CloseNow()
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("replacement handshake did not finish")
	}
}

func TestPeerHubRejectsChangedAuthenticatedPolicy(t *testing.T) {
	for _, established := range []bool{false, true} {
		for _, change := range []string{"token", "exports", "sync-receive", "disabled", "corrupt"} {
			t.Run(fmt.Sprintf("established=%t/%s", established, change), func(t *testing.T) {
				p := Peer{ID: "office", Token: NewToken(), Import: true}
				path := filepath.Join(t.TempDir(), "peers.json")
				writeConfig(t, path, p)
				var calls atomic.Int32
				h := New(path, func(string) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						io.WriteString(w, "allowed")
					})
				})
				connected := make(chan struct{}, 1)
				h.Connected = func(context.Context, Peer) error {
					select {
					case connected <- struct{}{}:
					default:
					}
					return nil
				}
				// Leave reconciliation stopped to exercise the interval before its
				// next tick; authentication still has to hold for every operation.
				returned := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(returned)
					h.ServeHTTP(w, r)
				}))
				defer func() {
					server.Close()
					select {
					case <-returned:
					case <-time.After(5 * time.Second):
						t.Error("closed socket left the server handler running")
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ws, _, err := peerDial(ctx, server.URL, p)
				if err != nil {
					t.Fatal(err)
				}
				defer ws.CloseNow()
				var c *peerwire.Conn
				if established {
					c = peerwire.New(ctx, ws, nil, peerwire.Options{})
					defer c.Close()
					if err := c.Ready(ctx); err != nil {
						t.Fatal(err)
					}
					select {
					case <-connected:
					case <-ctx.Done():
						t.Fatal("server did not finish the original handshake")
					}
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/_peer/catalog", nil)
					res, err := c.RoundTrip(req)
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					if res.StatusCode != http.StatusOK || calls.Load() != 1 {
						t.Fatal("unchanged policy did not allow the original operation")
					}
				}
				switch change {
				case "token":
					p.Token = NewToken()
				case "exports":
					p.Exports = []Selection{{Provider: "claude", Models: []string{"model"}}}
				case "sync-receive":
					p.SyncReceive = []string{"my-api"}
				case "disabled":
					p.Off = true
				}
				writeConfig(t, path, p)
				if change == "corrupt" {
					if err := os.WriteFile(path, []byte(`{`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if !established {
					c = peerwire.New(ctx, ws, nil, peerwire.Options{})
					defer c.Close()
				}
				before := calls.Load()
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/_peer/provider-sync", strings.NewReader(`{}`))
				res, err := c.RoundTrip(req)
				if err == nil {
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					if res.StatusCode != http.StatusForbidden {
						t.Fatalf("old authenticated socket used changed policy: status %d", res.StatusCode)
					}
				}
				if calls.Load() != before {
					t.Fatal("old authenticated socket reached the operation handler")
				}
			})
		}
	}
}

func TestPeerHubOutboundHandshakeHoldsTheSameSlot(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted := make(chan struct{})
	remoteDone := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer close(remoteDone)
		defer ws.CloseNow()
		close(accepted)
		// Read frames without completing the remote version handshake.
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer remote.Close()
	p.URL = "ws" + strings.TrimPrefix(remote.URL, "http")
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler { return http.NotFoundHandler() })
	server := httptest.NewServer(h)
	defer server.Close()
	stop := h.Start(ctx)
	defer stop()
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("outbound upgrade never reached the remote")
	}
	ws, response, err := peerDial(ctx, server.URL, p)
	if err == nil {
		ws.CloseNow()
		t.Fatal("incoming upgrade bypassed the pending outgoing connection")
	}
	if response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("incoming upgrade: response=%v error=%v", response, err)
	}
	stop()
	select {
	case <-remoteDone:
	case <-ctx.Done():
		t.Fatal("stopping hub left its pending outbound socket open")
	}
}

func TestPeerHubStopClosesPendingAndRestartAccepts(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken(), Import: true}
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler { return http.NotFoundHandler() })
	ready := make(chan struct{}, 2)
	h.Connected = func(context.Context, Peer) error { ready <- struct{}{}; return nil }
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := h.Start(ctx)
	defer stop()
	ws, _, err := peerDial(ctx, server.URL, p)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	stop()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("stopping hub left its pending inbound socket open")
			}
			break
		}
	}
	ws, response, err := peerDial(ctx, server.URL, p)
	if err == nil {
		ws.CloseNow()
		t.Fatal("stopped hub accepted a new socket")
	}
	if response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stopped hub: response=%v error=%v", response, err)
	}
	stopAgain := h.Start(ctx)
	defer stopAgain()
	ws, _, err = peerDial(ctx, server.URL, p)
	if err != nil {
		t.Fatalf("restarted hub did not accept a socket: %v", err)
	}
	c := peerwire.New(ctx, ws, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "remote")
	}), peerwire.Options{})
	defer c.Close()
	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("restarted hub never completed the handshake")
	}
	// An old stop handle cannot clear the new run's provider transport.
	stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer.invalid/_peer/catalog", nil)
	res, err := Transport(p.ID).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(b) != "remote" {
		t.Fatalf("restarted hub's transport: body=%q error=%v", b, err)
	}
}

func TestPeerHubFailedUpgradeReleasesSlot(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken()}
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler { return http.NotFoundHandler() })
	req := httptest.NewRequest(http.MethodGet, ConnectPath, nil)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set(IDHeader, p.ID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusSwitchingProtocols {
		t.Fatal("accepted an ordinary HTTP request as a WebSocket upgrade")
	}
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := peerDial(ctx, server.URL, p)
	if err != nil {
		t.Fatalf("failed HTTP upgrade retained its slot: %v", err)
	}
	ws.CloseNow()
}

func TestPeerHubConcurrentUpgradesHaveOneWinner(t *testing.T) {
	p := Peer{ID: "office", Token: NewToken()}
	path := filepath.Join(t.TempDir(), "peers.json")
	writeConfig(t, path, p)
	h := New(path, func(string) http.Handler { return http.NotFoundHandler() })
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := h.Start(ctx)
	defer stop()
	type result struct {
		ws   *websocket.Conn
		code int
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	for range cap(results) {
		go func() {
			<-start
			ws, response, _ := peerDial(ctx, server.URL, p)
			code := 0
			if response != nil {
				code = response.StatusCode
			}
			results <- result{ws, code}
		}()
	}
	close(start)
	winners := 0
	for range cap(results) {
		got := <-results
		if got.ws != nil {
			defer got.ws.CloseNow()
			winners++
		} else if got.code != http.StatusConflict {
			t.Errorf("concurrent upgrade returned %d, want 409", got.code)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d upgraded sockets for one peer, want 1", winners)
	}
}
