package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientRepositoryMetadataNotInModelList(t *testing.T) {
	before := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = before })
	calls := 0
	http.DefaultClient = &http.Client{Transport: metadataTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		for k := range req.Header {
			if strings.EqualFold(k, "x-claude-code-wiggly-dove") {
				t.Error("model discovery forwarded repository metadata")
			}
		}
		if req.Header.Get("Authorization") != "Bearer fixture-key" || req.Header.Get("X-Fixture") != "kept" {
			t.Error("model discovery lost necessary headers")
		}
		w := httptest.NewRecorder()
		w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
		return w.Result(), nil
	})}
	models, err := FetchURL(context.Background(), "http://127.0.0.1:1/models", "fixture-key", false,
		map[string]string{"x-cLaUdE-cOdE-wIgGlY-dOvE": "private-project", "X-Fixture": "kept"})
	if err != nil || calls != 1 || len(models) != 1 || models[0].ID != "fixture-model" {
		t.Fatalf("model discovery: calls %d, models %v, error %v", calls, models, err)
	}
}
