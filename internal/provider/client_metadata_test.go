package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientRepositoryMetadataNotInBalance(t *testing.T) {
	before := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = before })
	calls := 0
	http.DefaultClient = &http.Client{Transport: metadataTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		for k := range req.Header {
			if strings.EqualFold(k, "x-claude-code-wiggly-dove") {
				t.Error("balance query forwarded repository metadata")
			}
		}
		if req.Header.Get("Authorization") == "" || req.Header.Get("X-Fixture") != "kept" {
			t.Error("balance query lost necessary headers")
		}
		w := httptest.NewRecorder()
		w.Write([]byte(`{"credits":42}`))
		return w.Result(), nil
	})}
	for _, token := range []string{"", "fixture-token"} {
		p := Provider{ID: "privacy", Key: "fixture-key", BalanceURL: "http://127.0.0.1:1/balance", BalancePath: "credits", BalanceToken: token,
			Headers: map[string]string{"x-cLaUdE-cOdE-wIgGlY-dOvE": "private-project", "X-Fixture": "kept"}}
		if amount, ok, err := Balance(context.Background(), p); err != nil || !ok || amount != "42.00" {
			t.Fatalf("balance: %q, %v, %v", amount, ok, err)
		}
	}
	if calls != 2 {
		t.Fatalf("balance requests: %d, want two authentication paths", calls)
	}
}

func TestClientRepositoryMetadataRemovedBeforeSigning(t *testing.T) {
	signed := false
	p := Provider{Account: &Account{sign: func(_ context.Context, req *http.Request, _ []byte) error {
		signed = true
		for k := range req.Header {
			if strings.EqualFold(k, "x-claude-code-wiggly-dove") {
				t.Error("account signer received private repository metadata")
			}
		}
		req.Header.Set("Authorization", "fixture-signature")
		return nil
	}}}
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1", nil)
	req.Header["x-Claude-Code-Wiggly-Dove"] = []string{"first", "second"}
	req.Header["X-Claude-Code-Wiggly-Dove"] = []string{"third"}
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if err := p.Sign(context.Background(), req, Anthropic, nil); err != nil {
		t.Fatal(err)
	}
	if !signed || req.Header.Get("Authorization") != "fixture-signature" || req.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Error("privacy filtering changed the authentication contract")
	}
}
