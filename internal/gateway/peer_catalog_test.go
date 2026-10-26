package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

func TestPeerCatalogWithManyModelsFitsItsOwnPayloadLimit(t *testing.T) {
	fresh(t)
	var ids []string
	var models []catalog.Model
	for i := range 1600 {
		id := fmt.Sprintf("model-%d", i)
		ids = append(ids, id)
		models = append(models, catalog.Model{ID: id, Name: strings.Repeat("display name ", 70), APIs: []string{"chat"}})
	}
	p := provider.Provider{ID: "many", Name: "Many models", Chat: "http://127.0.0.1:1/v1", Key: "test-only", Models: ids}
	if err := catalog.SaveLive(p.ID, p.Chat, models); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	policy := peering.Peer{Exports: []peering.Selection{{Provider: p.ID, Models: ids}}}
	data, err := json.Marshal(peerCatalog(peerEntries(policy)))
	if err != nil || len(data) <= peering.MaxConfigBytes || len(data) >= peerCatalogMaxBytes {
		t.Fatalf("fixture must exceed the config limit and fit the catalog limit: %d bytes, %v", len(data), err)
	}
	newPeerGatewayPair(t, peering.Peer{Import: true}, policy)
	got, err := provider.Find("peer-office")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Exposed()) != len(ids) {
		t.Fatalf("imported %d of %d selected models", len(got.Exposed()), len(ids))
	}
	// Configuration writes retain their smaller limit.
	var value any
	if err := decodePeerJSON(bytes.NewReader(data), &value); err == nil {
		t.Fatal("large catalog allowance also relaxed the configuration limit")
	}
}

func TestPeerRefreshKeepsLocalSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		selected  []string
		none      bool
		available []string
		want      []string
	}{
		{"subset", []string{"fake/m1"}, false, []string{"m1", "m2", "m3"}, []string{"fake/m1"}},
		{"implicit", nil, false, []string{"m1", "m2", "m3"}, []string{"fake/m1", "fake/m2"}},
		{"none", nil, true, []string{"m1", "m2", "m3"}, nil},
		{"removed", []string{"fake/m1", "fake/m2"}, false, []string{"m2", "m3"}, []string{"fake/m2"}},
		{"all-removed", []string{"fake/m1"}, false, []string{"m2", "m3"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			source := provider.Provider{ID: "fake", Key: "test-only", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1", "m2"}}
			if err := provider.Save(source); err != nil {
				t.Fatal(err)
			}
			pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: source.ID, Models: []string{"m1", "m2", "m3"}}}})
			p, err := provider.Find("peer-office")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(p.Models, []string{"fake/m1", "fake/m2"}) || p.NoModels {
				t.Fatalf("first import did not select the shared models: %+v", p)
			}
			p.Models, p.NoModels = slices.Clone(tc.selected), tc.none
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			source.Models = tc.available
			if err := provider.Save(source); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := pair.a.refreshPeer(ctx, pair.aPolicy); err != nil {
				t.Fatal(err)
			}
			after, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(after.Models, tc.want) || after.NoModels != (len(tc.want) == 0) {
				t.Fatalf("catalog refresh changed local selection: models=%v none=%v, want models=%v", after.Models, after.NoModels, tc.want)
			}
			var available []string
			for _, id := range tc.available {
				available = append(available, "fake/"+id)
			}
			var listed []string
			for _, m := range after.Available() {
				listed = append(listed, m.ID)
			}
			if !slices.Equal(listed, available) {
				t.Fatalf("available models were not refreshed: got %v, want %v", listed, available)
			}
			var exposed []string
			for _, m := range after.Exposed() {
				exposed = append(exposed, m.ID)
			}
			if !slices.Equal(exposed, tc.want) {
				t.Fatalf("served models changed after refresh: got %v, want %v", exposed, tc.want)
			}
		})
	}
}
