package peerwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func pair(t *testing.T, left, right http.Handler, leftOpts, rightOpts Options) (*Conn, *Conn) {
	t.Helper()
	ctx := testContext(t)
	accepted := make(chan *Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		peer := New(ctx, ws, right, rightOpts)
		accepted <- peer
		<-peer.Done()
	}))
	t.Cleanup(server.Close)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := New(ctx, ws, left, leftOpts)
	var b *Conn
	select {
	case b = <-accepted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	if err := a.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func request(t *testing.T, ctx context.Context, method, path string, body io.Reader) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func readResponse(t *testing.T, c *Conn, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := c.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func waitSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDuplexJSON(t *testing.T) {
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "body read failed", 500)
				return
			}
			w.Header().Add("X-Peer", name)
			w.Header().Add("X-Peer", "second")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
				"token": r.Header.Get("X-Test-Token"), "body": string(b),
				"host": r.URL.Host, "request_uri": r.RequestURI,
			})
		})
	}
	a, b := pair(t, handler("a"), handler("b"), Options{}, Options{})
	for _, tc := range []struct {
		peer *Conn
		name string
	}{{a, "b"}, {b, "a"}} {
		r := request(t, testContext(t), "POST", "/v1/messages?beta=1", strings.NewReader(`{"model":"claude","stream":false}`))
		r.Header.Set("X-Test-Token", "test-only")
		resp, body := readResponse(t, tc.peer, r)
		if resp.StatusCode != http.StatusCreated || strings.Join(resp.Header.Values("X-Peer"), ",") != tc.name+",second" {
			t.Fatalf("status/headers: %d %v", resp.StatusCode, resp.Header)
		}
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got["method"] != "POST" || got["path"] != "/v1/messages" || got["query"] != "beta=1" ||
			got["token"] != "test-only" || got["body"] != `{"model":"claude","stream":false}` ||
			got["host"] != "" || got["request_uri"] != "/v1/messages?beta=1" {
			t.Fatalf("changed request: %v", got)
		}
	}
}

func TestSSEFlushArrivesBeforeHandlerFinishes(t *testing.T) {
	ctx := testContext(t)
	finish := make(chan struct{})
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
			_, _ = io.WriteString(w, "data: second\n\n")
		case <-r.Context().Done():
		}
	}), Options{}, Options{})
	resp, err := a.RoundTrip(request(t, ctx, "POST", "/v1/messages", strings.NewReader(`{"stream":true}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	for _, want := range []string{"event: message\n", "data: first\n", "\n"} {
		line, err := reader.ReadString('\n')
		if err != nil || line != want {
			t.Fatalf("first event: %q, %v", line, err)
		}
	}
	close(finish)
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "data: second\n\n" {
		t.Fatalf("last event: %q, %v", rest, err)
	}
}

func TestCancelBeforeHeadersPreservesConnection(t *testing.T) {
	ctx := testContext(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return
		}
		_, _ = io.WriteString(w, "still connected")
	}), Options{}, Options{})
	requestCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		resp, err := a.RoundTrip(request(t, requestCtx, "POST", "/hold", nil))
		if resp != nil {
			_ = resp.Body.Close()
		}
		result <- err
	}()
	waitSignal(t, ctx, entered)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitSignal(t, ctx, canceled)
	_, body := readResponse(t, a, request(t, ctx, "GET", "/fast", nil))
	if string(body) != "still connected" {
		t.Fatalf("body: %s", body)
	}
}

func TestClosingBodyCancelsRemoteHandler(t *testing.T) {
	ctx := testContext(t)
	canceled := make(chan struct{})
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}), Options{}, Options{})
	resp, err := a.RoundTrip(request(t, ctx, "GET", "/stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	waitSignal(t, ctx, canceled)
	if a.Err() != nil {
		t.Fatalf("body cancellation closed socket: %v", a.Err())
	}
}

func TestDisconnectAbortsPendingWithoutReplay(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers=%v", headers), func(t *testing.T) {
			ctx := testContext(t)
			entered, canceled := make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			a, b := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if headers {
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}), Options{}, Options{})
			result := make(chan error, 1)
			go func() {
				resp, err := a.RoundTrip(request(t, ctx, "POST", "/paid-request", nil))
				if err == nil {
					_, err = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				result <- err
			}()
			waitSignal(t, ctx, entered)
			_ = b.Close()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("disconnect returned a successful response")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			waitSignal(t, ctx, canceled)
			if requests.Load() != 1 {
				t.Fatalf("request was replayed: %d calls", requests.Load())
			}
		})
	}
}

func TestConcurrentRequestsBothDirections(t *testing.T) {
	ctx := testContext(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, r.Body)
	})
	a, b := pair(t, handler, handler, Options{}, Options{})
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for _, c := range []*Conn{a, b} {
		for n := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				payload := bytes.Repeat([]byte{byte(n)}, chunkBytes*3+17)
				r, _ := http.NewRequestWithContext(ctx, "POST", "/echo", bytes.NewReader(payload))
				resp, err := c.RoundTrip(r)
				if err != nil {
					results <- err
					return
				}
				defer resp.Body.Close()
				got, err := io.ReadAll(resp.Body)
				if err == nil && !bytes.Equal(got, payload) {
					err = fmt.Errorf("response mixed across requests: %d bytes", len(got))
				}
				results <- err
			}()
		}
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSlowConsumerDoesNotBlockOtherRequests(t *testing.T) {
	ctx := testContext(t)
	first, secondStarted, secondFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	chunk := bytes.Repeat([]byte("x"), chunkBytes)
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fast" {
			_, _ = io.WriteString(w, "fast")
			return
		}
		if _, err := w.Write(chunk); err != nil {
			return
		}
		close(first)
		close(secondStarted)
		if _, err := w.Write(chunk); err == nil {
			close(secondFinished)
		}
	}), Options{ResponseBufferBytes: chunkBytes}, Options{})
	resp, err := a.RoundTrip(request(t, ctx, "GET", "/slow", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	waitSignal(t, ctx, first)
	waitSignal(t, ctx, secondStarted)
	_, fast := readResponse(t, a, request(t, ctx, "GET", "/fast", nil))
	if string(fast) != "fast" {
		t.Fatalf("unrelated response: %q", fast)
	}
	select {
	case <-secondFinished:
		t.Fatal("sender exceeded the unread stream's receive window")
	default:
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(body, bytes.Repeat(chunk, 2)) {
		t.Fatalf("resumed stream: %d bytes, %v", len(body), err)
	}
	waitSignal(t, ctx, secondFinished)
}

func TestConcurrentLimitsRejectInsteadOfQueueing(t *testing.T) {
	ctx := testContext(t)
	entered := make(chan struct{}, 2)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-r.Context().Done()
	})
	t.Run("local", func(t *testing.T) {
		a, _ := pair(t, nil, handler, Options{MaxConcurrent: 1}, Options{})
		resp, err := a.RoundTrip(request(t, ctx, "GET", "/hold", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		waitSignal(t, ctx, entered)
		if _, err := a.RoundTrip(request(t, ctx, "GET", "/extra", nil)); !errors.Is(err, ErrBusy) {
			t.Fatalf("want busy, got %v", err)
		}
	})
	t.Run("remote", func(t *testing.T) {
		a, _ := pair(t, nil, handler, Options{}, Options{MaxConcurrent: 1})
		resp, err := a.RoundTrip(request(t, ctx, "GET", "/hold", nil))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		waitSignal(t, ctx, entered)
		extra, _ := readResponse(t, a, request(t, ctx, "GET", "/extra", nil))
		if extra.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status: %d", extra.StatusCode)
		}
	})
}

func TestRequestLimitsAndRelativePaths(t *testing.T) {
	ctx := testContext(t)
	var calls atomic.Int32
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(w, r.Body)
	}), Options{MaxRequestBody: 16}, Options{})
	for _, path := range []string{"https://example.invalid/v1/messages", "//example.invalid/v1/messages", "/%2fexample.invalid"} {
		_, err := a.RoundTrip(request(t, ctx, "GET", path, nil))
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("accepted path %q: %v", path, err)
		}
	}
	_, err := a.RoundTrip(request(t, ctx, "POST", "/v1/messages", strings.NewReader(strings.Repeat("x", 17))))
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("body limit: %v", err)
	}
	r := request(t, ctx, "GET", "/valid", nil)
	r.Header.Set("X-Test", "invalid\r\nheader")
	if _, err := a.RoundTrip(r); !errors.Is(err, ErrProtocol) {
		t.Fatalf("header validation: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached the handler: %d", calls.Load())
	}
	_, body := readResponse(t, a, request(t, ctx, "POST", "/valid", strings.NewReader(strings.Repeat("x", 16))))
	if len(body) != 16 {
		t.Fatalf("body boundary: %q", body)
	}
}

func TestReceiverChecksRequestBodyLimit(t *testing.T) {
	ctx := testContext(t)
	var calls atomic.Int32
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}), Options{}, Options{MaxRequestBody: 16})
	resp, _ := readResponse(t, a, request(t, ctx, "POST", "/valid", strings.NewReader(strings.Repeat("x", 17))))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("oversized request: status=%d handler calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestVersionMismatchFailsBeforeRequests(t *testing.T) {
	ctx := testContext(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":999}`))
		_, _, _ = ws.Read(ctx)
		_, _, _ = ws.Read(ctx)
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := New(ctx, ws, nil, Options{})
	defer c.Close()
	if err := c.Ready(ctx); !errors.Is(err, ErrProtocol) {
		t.Fatalf("version mismatch: %v", err)
	}
}

func TestParentCancellationClosesConnection(t *testing.T) {
	a, b := pair(t, nil, nil, Options{}, Options{})
	a.cancel()
	waitSignal(t, testContext(t), b.Done())
	if err := b.Ready(testContext(t)); err == nil {
		t.Fatal("disconnected peer is ready")
	}
}

func TestHeartbeatClosesPeerThatWithholdsPong(t *testing.T) {
	ctx := testContext(t)
	ping := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OnPingReceived: func(context.Context, []byte) bool {
				signal(ping)
				return false // Inject a missing pong, rather than timing a healthy peer.
			},
		})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":1}`))
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := New(ctx, ws, nil, Options{HeartbeatInterval: 10 * time.Millisecond, HeartbeatTimeout: time.Second})
	defer c.Close()
	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, ctx, ping)
	waitSignal(t, ctx, c.Done())
	if c.Err() == nil {
		t.Fatal("peer that withheld pong stayed connected")
	}
}

func TestIncomingAbsolutePathNeverReachesHandler(t *testing.T) {
	ctx := testContext(t)
	var calls atomic.Int32
	a, b := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}), Options{}, Options{})
	// Bypass the sender's URL check to exercise the receiving trust boundary.
	_ = a.send(ctx, message{Type: "request", ID: 91, Method: "GET", Path: "https://example.invalid/secret", Window: chunkBytes})
	waitSignal(t, ctx, b.Done())
	if !errors.Is(b.Err(), ErrProtocol) || calls.Load() != 0 {
		t.Fatalf("absolute target reached handler: calls=%d error=%v", calls.Load(), b.Err())
	}
}

func TestHandlerPanicEndsResponse(t *testing.T) {
	ctx := testContext(t)
	a, _ := pair(t, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stream" {
			w.(http.Flusher).Flush()
		}
		panic("test panic; must not escape to the other peer")
	}), Options{}, Options{})
	resp, _ := readResponse(t, a, request(t, ctx, "GET", "/before-headers", nil))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("panic status: %d", resp.StatusCode)
	}
	resp, err := a.RoundTrip(request(t, ctx, "GET", "/stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if err == nil || strings.Contains(err.Error(), "test panic") {
		t.Fatalf("interrupted response: %v", err)
	}
	if a.Err() != nil {
		t.Fatalf("handler panic closed connection: %v", a.Err())
	}
}
