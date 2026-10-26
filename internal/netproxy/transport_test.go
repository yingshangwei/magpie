package netproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type peerTestTransport func(*http.Request) (*http.Response, error)

func (f peerTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPeerTransportNeverFallsBackToNetwork(t *testing.T) {
	dials, calls := 0, 0
	network := errors.New("network was reached")
	d := Dispatch(&http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, network
	}})
	peer := peerTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("peer")), Request: r}, nil
	})
	ctx := WithTransport(context.Background(), peer)
	r, _ := http.NewRequestWithContext(ctx, "POST", "http://peer.invalid/v1/messages", nil)
	res, err := d.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if calls != 1 || dials != 0 {
		t.Fatalf("peer calls=%d network dials=%d", calls, dials)
	}
	hold := errors.New("not authorized")
	if _, err := d.RoundTrip(r.WithContext(Hold(ctx, hold))); !errors.Is(err, hold) || calls != 1 {
		t.Fatalf("held request reached peer: calls=%d err=%v", calls, err)
	}
	cleared := WithTransport(ctx, nil)
	if _, err := d.RoundTrip(r.WithContext(cleared)); err == nil || dials != 0 {
		t.Fatalf("peer URL escaped to HTTP after clearing context: %v; dials=%d", err, dials)
	}
	q, _ := http.NewRequestWithContext(cleared, "POST", "http://ordinary.example.test/v1/messages", nil)
	if _, err := d.RoundTrip(q); !errors.Is(err, network) || dials != 1 || calls != 1 {
		t.Fatalf("normal provider inherited peer transport: err=%v calls=%d dials=%d", err, calls, dials)
	}
}
