// Package peerwire carries concurrent HTTP exchanges in both directions over
// one authenticated WebSocket. Authentication and routing belong to its caller.
package peerwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/net/http/httpguts"
)

const (
	Version             = 1
	MaxRequestBodyBytes = 32 << 20
	chunkBytes          = 32 << 10
	maxHeaderBytes      = 64 << 10
	maxWindowBytes      = 4 << 20
)

var (
	ErrClosed       = errors.New("peerwire: connection closed")
	ErrBusy         = errors.New("peerwire: too many concurrent requests")
	ErrBodyTooLarge = errors.New("peerwire: request body too large")
	ErrProtocol     = errors.New("peerwire: invalid protocol message")
)

// Options bounds the work one connection can retain. Zero values select the
// defaults. Invalid sizes are clamped to their documented limits.
type Options struct {
	// MaxConcurrent applies separately to incoming and outgoing exchanges.
	// The default is 8, and the maximum is 64. A full side rejects new work.
	MaxConcurrent int
	// MaxRequestBody defaults to 32 MiB and cannot exceed that protocol limit.
	MaxRequestBody int64
	// ResponseBufferBytes defaults to 256 KiB, between 32 KiB and 4 MiB.
	// Each stream has its own receive window, so a stalled reader pauses only
	// that stream. Callers must consume or close every response body.
	ResponseBufferBytes int
	// HandshakeTimeout defaults to 10 seconds; WriteTimeout to 15 seconds.
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	// HeartbeatInterval defaults to 30 seconds; HeartbeatTimeout to 10 seconds.
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
}

func (o Options) defaults() Options {
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 8
	}
	o.MaxConcurrent = min(o.MaxConcurrent, 64)
	if o.MaxRequestBody <= 0 || o.MaxRequestBody > MaxRequestBodyBytes {
		o.MaxRequestBody = MaxRequestBodyBytes
	}
	if o.ResponseBufferBytes <= 0 {
		o.ResponseBufferBytes = 256 << 10
	}
	o.ResponseBufferBytes = min(max(o.ResponseBufferBytes, chunkBytes), maxWindowBytes)
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 15 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 30 * time.Second
	}
	if o.HeartbeatTimeout <= 0 {
		o.HeartbeatTimeout = 10 * time.Second
	}
	return o
}

type message struct {
	Type    string      `json:"type"`
	Version int         `json:"version,omitempty"`
	ID      uint64      `json:"id,omitempty"`
	Method  string      `json:"method,omitempty"`
	Path    string      `json:"path,omitempty"`
	Header  http.Header `json:"header,omitempty"`
	Body    []byte      `json:"body,omitempty"`
	Status  int         `json:"status,omitempty"`
	Window  int         `json:"window,omitempty"`
	Error   string      `json:"error,omitempty"`
}

type writeItem struct {
	message message
	ctx     context.Context
	done    chan error
}

type responseResult struct {
	response *http.Response
	err      error
}

type outgoing struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	request  *http.Request
	response chan responseResult
	body     *responseBody
	started  bool // guarded by Conn.mu
	ended    bool // guarded by Conn.mu
}

// Conn implements http.RoundTripper without dialing, redirecting or retrying.
// Only relative request URLs beginning with one slash are accepted. A caller
// using an http.Client must first clone its URL and clear Scheme and Host.
type Conn struct {
	ws      *websocket.Conn
	handler http.Handler
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	ready   chan struct{}
	writeq  chan writeItem
	slots   chan struct{}
	nextID  atomic.Uint64
	wg      sync.WaitGroup

	mu  sync.Mutex
	err error
	out map[uint64]*outgoing
	in  map[uint64]*incoming
}

// New takes ownership of ws and starts the version handshake and read loop.
// Either endpoint may have dialed the WebSocket; both can send requests. Its
// handler should honor request cancellation just as an HTTP server's does.
// A nil handler returns 404. The caller must authenticate before calling New.
func New(ctx context.Context, ws *websocket.Conn, handler http.Handler, opts Options) *Conn {
	if handler == nil {
		handler = http.NotFoundHandler()
	}
	opts = opts.defaults()
	ctx, cancel := context.WithCancel(ctx)
	c := &Conn{
		ws: ws, handler: handler, opts: opts, ctx: ctx, cancel: cancel,
		ready: make(chan struct{}), writeq: make(chan writeItem, opts.MaxConcurrent*4+8),
		slots: make(chan struct{}, opts.MaxConcurrent),
		out:   make(map[uint64]*outgoing), in: make(map[uint64]*incoming),
	}
	// A request body is base64 in JSON. Header/path escaping fits the remainder.
	ws.SetReadLimit(MaxRequestBodyBytes*4/3 + 512<<10)
	c.writeq <- writeItem{message: message{Type: "hello", Version: Version}, ctx: ctx}
	c.wg.Add(3)
	go c.readLoop()
	go c.writeLoop()
	go c.heartbeat()
	return c
}

// Ready waits for the peer's compatible version, or for either context to end.
func (c *Conn) Ready(ctx context.Context) error {
	select {
	case <-c.ready:
		return c.Err()
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.closedError()
	}
}

func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// Err is nil while the connection is usable, and records its first failure.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) closedError() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

// Close aborts pending requests and the socket and waits for its I/O loops.
// As with net/http, a handler that ignores its context cannot be forcibly stopped.
func (c *Conn) Close() error {
	c.fail(ErrClosed)
	c.wg.Wait()
	return nil
}

func (c *Conn) fail(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	out, in := c.out, c.in
	c.out, c.in = make(map[uint64]*outgoing), make(map[uint64]*incoming)
	// enqueue also takes mu, so no frame retaining a body can arrive after this.
	draining := true
	for draining {
		select {
		case item := <-c.writeq:
			if item.done != nil {
				item.done <- err
			}
		default:
			draining = false
		}
	}
	c.mu.Unlock()
	c.cancel()
	_ = c.ws.CloseNow()
	for _, o := range out {
		o.stop()
		o.cancel()
		o.body.finish(err)
		o.response <- responseResult{err: err}
		<-c.slots
	}
	for _, i := range in {
		i.cancel()
	}
}

// RoundTrip buffers at most MaxRequestBody bytes of input and returns as soon
// as response headers arrive. Streaming response bytes are never retried.
func (c *Conn) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("peerwire: nil request")
	}
	closeBody := func() {}
	if req.Body != nil {
		closeBody = sync.OnceFunc(func() { _ = req.Body.Close() })
		defer closeBody()
	}
	path, err := requestPath(req.URL)
	if err != nil || !httpguts.ValidHeaderFieldName(req.Method) || !validHeader(req.Header) {
		return nil, fmt.Errorf("%w: invalid HTTP request", ErrProtocol)
	}
	if err := c.Ready(req.Context()); err != nil {
		return nil, err
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	reserved := true
	defer func() {
		if reserved {
			<-c.slots
		}
	}()
	var body []byte
	if req.Body != nil {
		if req.ContentLength > c.opts.MaxRequestBody {
			return nil, ErrBodyTooLarge
		}
		// Closing a caller's body also unblocks a pending Read when its request
		// or the connection ends; no extra unbounded body-reader goroutine is used.
		stopRequest := context.AfterFunc(req.Context(), closeBody)
		stopConn := context.AfterFunc(c.ctx, closeBody)
		body, err = io.ReadAll(io.LimitReader(req.Body, c.opts.MaxRequestBody+1))
		stopRequest()
		stopConn()
		if err != nil {
			return nil, err
		}
		if int64(len(body)) > c.opts.MaxRequestBody {
			return nil, ErrBodyTooLarge
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	ctx, cancel := context.WithCancel(c.ctx)
	o := &outgoing{ctx: ctx, cancel: cancel, request: req, response: make(chan responseResult, 2)}
	o.body = newResponseBody(c.opts.ResponseBufferBytes,
		func(n int) { c.returnWindow(id, n) },
		func() { c.removeOutgoing(id, nil, false) },
		func() { c.removeOutgoing(id, context.Canceled, true) })
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		cancel()
		return nil, c.closedError()
	}
	c.out[id] = o
	o.stop = context.AfterFunc(req.Context(), func() { c.removeOutgoing(id, req.Context().Err(), true) })
	reserved = false
	c.mu.Unlock()
	err = c.send(ctx, message{Type: "request", ID: id, Method: req.Method, Path: path,
		Header: req.Header.Clone(), Body: body, Window: c.opts.ResponseBufferBytes})
	if err != nil {
		c.removeOutgoing(id, err, true)
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, err
	}
	select {
	case result := <-o.response:
		return result.response, result.err
	case <-req.Context().Done():
		c.removeOutgoing(id, req.Context().Err(), true)
		return nil, req.Context().Err()
	case <-c.ctx.Done():
		return nil, c.closedError()
	}
}

func (c *Conn) removeOutgoing(id uint64, err error, notify bool) {
	c.mu.Lock()
	o := c.out[id]
	delete(c.out, id)
	c.mu.Unlock()
	if o == nil {
		return
	}
	o.stop()
	o.cancel()
	if err != nil {
		o.body.finish(err)
		o.response <- responseResult{err: err}
	}
	<-c.slots
	if notify {
		c.control(message{Type: "cancel", ID: id})
	}
}

func (c *Conn) returnWindow(id uint64, n int) {
	c.mu.Lock()
	o := c.out[id]
	active := o != nil && !o.ended
	c.mu.Unlock()
	if active {
		_ = c.send(o.ctx, message{Type: "window", ID: id, Window: n})
	}
}

func (c *Conn) enqueue(ctx context.Context, m message, done chan error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	err := c.err
	if err == nil {
		select {
		case c.writeq <- writeItem{message: m, ctx: ctx, done: done}:
		default:
			err = errors.New("peerwire: write queue full")
		}
	}
	c.mu.Unlock()
	if err != nil {
		c.fail(err)
	}
	return err
}

func (c *Conn) send(ctx context.Context, m message) error {
	done := make(chan error, 1)
	if err := c.enqueue(ctx, m, done); err != nil {
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.closedError()
	}
}

// The socket reader must never wait for a write or a response consumer.
func (c *Conn) control(m message) { _ = c.enqueue(c.ctx, m, nil) }

func (c *Conn) writeLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case item := <-c.writeq:
			err := item.ctx.Err()
			if err == nil {
				var encoded []byte
				encoded, err = json.Marshal(item.message)
				if err == nil {
					// A stream's cancellation must not cancel websocket.Write: the
					// library closes the whole socket when a write context expires.
					ctx, cancel := context.WithTimeout(c.ctx, c.opts.WriteTimeout)
					err = c.ws.Write(ctx, websocket.MessageText, encoded)
					cancel()
					if err != nil {
						c.fail(fmt.Errorf("peerwire: write failed: %w", err))
					}
				}
			}
			if item.done != nil {
				item.done <- err
			}
		}
	}
}

func (c *Conn) readLoop() {
	defer c.wg.Done()
	hello := false
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.fail(fmt.Errorf("peerwire: read failed: %w", err))
			return
		}
		var m message
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
			c.fail(ErrProtocol)
			return
		}
		if !hello {
			if m.Type != "hello" || m.Version != Version {
				c.fail(fmt.Errorf("%w: unsupported version", ErrProtocol))
				return
			}
			hello = true
			close(c.ready)
			continue
		}
		if m.ID == 0 || !c.receive(m) {
			c.fail(ErrProtocol)
			return
		}
	}
}

func (c *Conn) receive(m message) bool {
	switch m.Type {
	case "request":
		return c.accept(m)
	case "cancel", "window":
		c.mu.Lock()
		i := c.in[m.ID]
		c.mu.Unlock()
		if i != nil {
			if m.Type == "cancel" {
				i.cancel()
			} else if !i.addWindow(m.Window) {
				return false
			}
		}
		return true
	case "response", "data", "end":
	default:
		return false
	}
	c.mu.Lock()
	o := c.out[m.ID]
	if o == nil {
		c.mu.Unlock()
		return true // Frames already on the wire may follow a local cancellation.
	}
	valid := !o.ended
	switch m.Type {
	case "response":
		valid = valid && !o.started && m.Status >= 200 && m.Status <= 599 && validHeader(m.Header)
		if valid {
			if m.Header == nil {
				m.Header = make(http.Header)
			}
			o.started = true
			o.response <- responseResult{response: &http.Response{
				StatusCode: m.Status, Status: fmt.Sprintf("%d %s", m.Status, http.StatusText(m.Status)),
				Header: m.Header, Body: o.body, ContentLength: -1, Request: o.request,
				Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			}}
		}
	case "data":
		valid = valid && o.started && len(m.Body) > 0 && len(m.Body) <= chunkBytes
	case "end":
		valid = valid && o.started && len(m.Error) <= 1024
		if valid {
			o.ended = true
		}
	}
	c.mu.Unlock()
	if !valid {
		return false
	}
	switch m.Type {
	case "data":
		return o.body.push(m.Body)
	case "end":
		err := io.EOF
		if m.Error != "" {
			err = errors.New("peerwire: remote response interrupted")
		}
		o.body.finish(err)
	}
	return true
}

func (c *Conn) accept(m message) bool {
	u, err := parsePath(m.Path)
	if err != nil || !httpguts.ValidHeaderFieldName(m.Method) || !validHeader(m.Header) ||
		m.Window < chunkBytes || m.Window > maxWindowBytes {
		return false
	}
	if int64(len(m.Body)) > c.opts.MaxRequestBody {
		c.reject(m.ID, http.StatusRequestEntityTooLarge)
		return true
	}
	c.mu.Lock()
	if c.in[m.ID] != nil {
		c.mu.Unlock()
		return false
	}
	if c.err != nil {
		c.mu.Unlock()
		return true
	}
	if len(c.in) >= c.opts.MaxConcurrent {
		c.mu.Unlock()
		c.reject(m.ID, http.StatusServiceUnavailable)
		return true
	}
	ctx, cancel := context.WithCancel(c.ctx)
	i := &incoming{ctx: ctx, cancel: cancel, window: m.Window, credit: m.Window, wake: make(chan struct{}, 1)}
	c.in[m.ID] = i
	c.mu.Unlock()
	req := &http.Request{
		Method: m.Method, URL: u, RequestURI: m.Path, Header: m.Header,
		Body: io.NopCloser(bytes.NewReader(m.Body)), ContentLength: int64(len(m.Body)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	go c.serve(m.ID, i, req.WithContext(ctx))
	return true
}

func (c *Conn) reject(id uint64, status int) {
	c.control(message{Type: "response", ID: id, Status: status})
	c.control(message{Type: "end", ID: id})
}

func (c *Conn) heartbeat() {
	defer c.wg.Done()
	timer := time.NewTimer(c.opts.HandshakeTimeout)
	defer timer.Stop()
	select {
	case <-c.ready:
	case <-timer.C:
		c.fail(errors.New("peerwire: version handshake timed out"))
		return
	case <-c.ctx.Done():
		c.fail(c.ctx.Err())
		return
	}
	ticker := time.NewTicker(c.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			c.fail(c.ctx.Err())
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(c.ctx, c.opts.HeartbeatTimeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.fail(fmt.Errorf("peerwire: heartbeat failed: %w", err))
				return
			}
		}
	}
}

func requestPath(u *url.URL) (string, error) {
	if u == nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return "", ErrProtocol
	}
	path := u.RequestURI()
	_, err := parsePath(path)
	return path, err
}

func parsePath(path string) (*url.URL, error) {
	if len(path) == 0 || len(path) > 8<<10 || !strings.HasPrefix(path, "/") ||
		strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n\x00\\") {
		return nil, ErrProtocol
	}
	u, err := url.ParseRequestURI(path)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "//") {
		return nil, ErrProtocol
	}
	return u, nil
}

func validHeader(header http.Header) bool {
	size, count := 0, 0
	for name, values := range header {
		if !httpguts.ValidHeaderFieldName(name) {
			return false
		}
		size += len(name)
		for _, value := range values {
			count++
			size += len(value) + 4
			if !httpguts.ValidHeaderFieldValue(value) {
				return false
			}
		}
		if size > maxHeaderBytes || count > 256 {
			return false
		}
	}
	return true
}
