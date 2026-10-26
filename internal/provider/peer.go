package provider

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/peering"
)

// CheckRename refuses a new id for a provider whose peer owns that id.
// Editors call it before saving other fields of a rename request.
func (p Provider) CheckRename(to string) error {
	if strings.ToLower(strings.TrimSpace(to)) != p.ID && (p.Peer != "" || p.PeerSource != "") {
		return errors.New("a peer-owned provider keeps its id; change its display name instead")
	}
	return nil
}

// freezePeerModels turns an explicit all selection into the current known
// list. A missing cache cannot establish what an older implicit pick meant.
func freezePeerModels(p *Provider) error {
	if p.Peer == "" || p.NoModels || len(p.Models) != 0 {
		return nil
	}
	models, _, ok := catalog.Live(p.ID)
	if !ok {
		return fmt.Errorf("%s has no readable previous peer catalog; select exact models or none before saving or refreshing", p.ID)
	}
	for _, m := range models {
		p.Models = append(p.Models, m.ID)
	}
	return nil
}

// RefreshPeerCatalog merges only the peer's model selection into the latest
// stored provider. It freezes old implicit picks before publishing the new
// catalog, so a new remote model never becomes an implicit local selection.
func RefreshPeerCatalog(peerID, name string, models []catalog.Model) error {
	if !peering.ValidID(peerID) {
		return errors.New("invalid peer id")
	}
	id := "peer-" + peerID
	ids := make([]string, 0, len(models))
	shared := make(map[string]bool, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
		shared[m.ID] = true
	}
	if name == "" {
		name = peerID
	}
	for range 4 {
		f, err := read()
		if err != nil {
			return err
		}
		i := slices.IndexFunc(f.Providers, func(p Provider) bool { return p.ID == id })
		p := normalize(Provider{ID: id, Name: name + " (WS)", Preset: RemoteMagpiePreset, Peer: peerID, Chat: "http://peer.invalid/v1", Models: slices.Clone(ids)})
		if i >= 0 {
			p = f.Providers[i]
			if p.Peer != peerID {
				return errors.New("peer provider id is already owned by a local provider")
			}
			if err := freezePeerModels(&p); err != nil {
				return err
			}
			p.Models = slices.DeleteFunc(slices.Clone(p.Models), func(m string) bool { return !shared[m] })
		} else if slices.ContainsFunc(f.Providers, func(p Provider) bool { return slices.Contains(p.Was, id) }) {
			return errors.New("peer provider id belongs to a renamed provider; restore its original id in providers.json before refreshing")
		}
		if err := validatePeerProvider(p); err != nil {
			return err
		}
		p.NoModels, p.PickedFrom = len(p.Models) == 0, slices.Clone(ids)
		if i < 0 || !sameStored(f.Providers[i], p) {
			if i < 0 {
				f.Providers = append(f.Providers, p)
			} else {
				f.Providers[i] = p
			}
			if err := store(f); errors.Is(err, errProvidersChanged) {
				continue // recompute from the user's completed save
			} else if err != nil {
				return err
			}
		}
		return catalog.SaveLive(id, p.Chat, models)
	}
	return errProvidersChanged
}

func validatePeerProvider(p Provider) error {
	if p.Peer == "" {
		return nil
	}
	if !peering.ValidID(p.Peer) || p.ID != "peer-"+p.Peer || !p.IsRemoteMagpie() ||
		p.Chat != "http://peer.invalid/v1" || p.Responses != "http://peer.invalid/v1" || p.Anthropic != "http://peer.invalid" ||
		p.Key != "" || len(p.Keys) != 0 || p.Account != nil || p.PeerSource != "" || p.Gemini != "" || p.ModelsURL != "" {
		return errors.New("a peer provider must use its own peer id, the peer transport and no API keys or custom model URL")
	}
	return nil
}
