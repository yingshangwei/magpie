package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

type peerView struct {
	peering.Peer
	Token    string `json:"token,omitempty"` // shadow the secret; revealed only by Pairing code
	Revision string `json:"revision"`
}

type peerChoice struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Models  []string `json:"models"`
	CanSync bool     `json:"canSync"`
}

// Peer administration stays on this computer, including when the web UI
// itself is shared. The native scheme marker cannot be supplied by HTTP.
func peerLocal(r *http.Request) bool {
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-IP", "Cf-Connecting-IP", "True-Client-IP", "Tailscale-User-Login"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	if apiFramed(r) {
		return false
	}
	if scheme, ok := r.Context().Value(windowSchemeKey{}).(string); ok && (scheme == "wails" || scheme == "http") {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	u, err := url.Parse("http://" + r.Host)
	if err != nil || u.User != nil || u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if origin != scheme+"://"+r.Host {
			return false
		}
	}
	return true
}

func peerInput(w http.ResponseWriter, r *http.Request, out any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("Device settings require JSON")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, peering.MaxConfigBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("Invalid device settings")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("Invalid device settings")
	}
	return nil
}

func peerRoutes(mux *http.ServeMux) {
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !peerLocal(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				writeJSON(w, map[string]string{"error": "Manage devices in the desktop app or a browser on this computer"})
				return
			}
			h(w, r)
		})
	}
	route("GET /api/peers", func(w http.ResponseWriter, r *http.Request) {
		c, err := peering.Load(peering.Path())
		if err != nil {
			fail(w, err)
			return
		}
		peers := make([]peerView, 0, len(c.Peers))
		for _, p := range c.Peers {
			peers = append(peers, peerView{Peer: p, Revision: peering.Revision(p)})
		}
		choices := []peerChoice{}
		for _, e := range provider.Catalog() {
			p := e.Provider
			if e.Group != "" || p.Peer != "" || p.IsRemoteMagpie() || !peering.ValidID(p.ID) {
				continue
			}
			i := slices.IndexFunc(choices, func(v peerChoice) bool { return v.ID == p.ID })
			if i < 0 {
				_, err := provider.ExportPeerConfig(p.ID, []string{e.Model}, false)
				choices = append(choices, peerChoice{ID: p.ID, Name: p.Name, Models: []string{}, CanSync: err == nil})
				i = len(choices) - 1
			}
			if !slices.Contains(choices[i].Models, e.Model) {
				choices[i].Models = append(choices[i].Models, e.Model)
			}
		}
		writeJSON(w, map[string]any{"peers": peers, "providers": choices})
	})
	route("POST /api/peers/draft", func(w http.ResponseWriter, r *http.Request) {
		var q struct{}
		if err := peerInput(w, r, &q); err != nil {
			fail(w, err)
			return
		}
		token := peering.NewToken()
		writeJSON(w, map[string]any{"version": 1, "id": "link-" + token[:12], "token": token})
	})
	route("POST /api/peers/save", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Peer     peering.Peer `json:"peer"`
			Revision string       `json:"revision"`
		}
		if err := peerInput(w, r, &q); err != nil {
			fail(w, err)
			return
		}
		err := peering.Update(func(c *peering.Config) error {
			i := slices.IndexFunc(c.Peers, func(p peering.Peer) bool { return p.ID == q.Peer.ID })
			if i < 0 {
				if q.Revision != "" {
					return errors.New("Device settings changed; reopen the device before saving")
				}
				c.Peers = append(c.Peers, q.Peer)
				return nil
			}
			if q.Revision != peering.Revision(c.Peers[i]) {
				return errors.New("Device settings changed; reopen the device before saving")
			}
			if q.Peer.Token == "" {
				q.Peer.Token = c.Peers[i].Token
			}
			c.Peers[i] = q.Peer
			return nil
		})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	for _, action := range []string{"remove", "pairing"} {
		route("POST /api/peers/"+action, func(w http.ResponseWriter, r *http.Request) {
			var q struct {
				ID       string `json:"id"`
				Revision string `json:"revision"`
			}
			if err := peerInput(w, r, &q); err != nil {
				fail(w, err)
				return
			}
			var selected peering.Peer
			change := func(c *peering.Config) error {
				i := slices.IndexFunc(c.Peers, func(p peering.Peer) bool { return p.ID == q.ID })
				if i < 0 || q.Revision != peering.Revision(c.Peers[i]) {
					return errors.New("Device settings changed; reopen the device before saving")
				}
				selected = c.Peers[i]
				if action == "remove" {
					c.Peers = slices.Delete(c.Peers, i, i+1)
				}
				return nil
			}
			var err error
			if action == "remove" {
				err = peering.Update(change)
			} else {
				var c peering.Config
				c, err = peering.Load(peering.Path())
				if err == nil {
					err = change(&c)
				}
			}
			if err != nil {
				fail(w, err)
				return
			}
			if action == "pairing" {
				writeJSON(w, map[string]any{"version": 1, "id": selected.ID, "token": selected.Token})
			} else {
				writeJSON(w, map[string]bool{"ok": true})
			}
		})
	}
	for _, action := range []string{"status", "refresh", "sync"} {
		method := http.MethodPost
		if action == "status" {
			method = http.MethodGet
		}
		route(method+" /api/peers/"+action, func(w http.ResponseWriter, r *http.Request) {
			var payload []byte
			if action != "status" {
				var q struct {
					Peer     string `json:"peer"`
					Provider string `json:"provider,omitempty"`
					Revision string `json:"revision,omitempty"`
				}
				if err := peerInput(w, r, &q); err != nil {
					fail(w, err)
					return
				}
				if action == "sync" && q.Revision == "" {
					fail(w, errors.New("Device settings changed; reopen the device before saving"))
					return
				}
				payload, _ = json.Marshal(q)
			}
			b, err := peerGateway(r.Context(), method, action, payload)
			if err != nil {
				fail(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		})
	}
}

// Use the actual gateway, which can run in a separate daemon. Never send
// browser cookies, pairing secrets or redirects through this local bridge.
func peerGateway(ctx context.Context, method, action string, body []byte) ([]byte, error) {
	u, err := url.Parse(gateway.URL())
	if err != nil || u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, errors.New("Device controls need a gateway listening on loopback")
	}
	u.Path = "/v1/peers/" + action
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "magpie/peer-gui")
	timeout := 35 * time.Second
	if action == "status" {
		timeout = 3 * time.Second
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Cannot reach the local gateway; start it and refresh")
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, peering.MaxConfigBytes+1))
	if err != nil || len(b) > peering.MaxConfigBytes || !json.Valid(b) {
		return nil, errors.New("Invalid local gateway response")
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			return nil, errors.New(e.Error.Message)
		}
		return nil, fmt.Errorf("Device operation failed (%d)", res.StatusCode)
	}
	return b, nil
}
