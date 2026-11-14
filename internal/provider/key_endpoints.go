package provider

import (
	"fmt"
	"net/url"
	"strings"
)

// KeyEndpoints is one key's complete set of upstream addresses. Missing
// protocols in an explicit set are disabled, never inherited from another site.
type KeyEndpoints struct {
	Chat      string `json:"chat,omitempty"`
	Responses string `json:"responses,omitempty"`
	Anthropic string `json:"anthropic,omitempty"`
	Gemini    string `json:"gemini,omitempty"`
	ModelsURL string `json:"modelsURL,omitempty"`
}

func (e KeyEndpoints) check() error {
	for _, raw := range []string{e.Chat, e.Responses, e.Anthropic, e.Gemini, e.ModelsURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("account endpoint must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	return nil
}

func checkAccountEndpoints(p Provider) error {
	for _, e := range p.AccountEndpoints {
		if e == (KeyEndpoints{}) {
			return fmt.Errorf("empty account endpoints: remove the override to follow the provider")
		}
		if err := e.check(); err != nil {
			return err
		}
	}
	return nil
}

func (p Provider) atKeyEndpoints(id string) Provider {
	e, ok := p.AccountEndpoints[id]
	if !ok {
		return p
	}
	p.Chat, p.Responses, p.Anthropic, p.Gemini, p.ModelsURL = e.Chat, e.Responses, e.Anthropic, e.Gemini, e.ModelsURL
	// Balances and decision APIs at the provider's default site are not this key's.
	p.BalanceURL, p.Decide = "", ""
	return p
}

// SetKeyEndpoints sets a key's own sites; an empty set restores inheritance.
func SetKeyEndpoints(id, ref string, e KeyEndpoints) error {
	p, err := Find(id)
	if err != nil {
		return err
	}
	if p.Account != nil || p.IsPlugin() {
		return fmt.Errorf("only API keys can have endpoint overrides")
	}
	if _, ok := findKey(p, ref); !ok {
		return fmt.Errorf("no such key")
	}
	if err := e.check(); err != nil {
		return err
	}
	if p.AccountEndpoints == nil {
		p.AccountEndpoints = map[string]KeyEndpoints{}
	}
	if e == (KeyEndpoints{}) {
		delete(p.AccountEndpoints, ref)
	} else {
		p.AccountEndpoints[ref] = e
	}
	if err := Save(*p); err != nil {
		return err
	}
	ForgetPlanQuotas()
	forgetKeyAllowances()
	return nil
}

// keyAlias is a former provider as its original account alone. Keep missing
// and disabled aliases closed rather than falling through to the other keys.
func (p Provider) keyAlias(alias string) (Provider, bool) {
	id, ok := p.KeyAliases[strings.ToLower(alias)]
	if !ok {
		return Provider{}, false
	}
	for _, k := range append([]KeyAccount{p.first()}, p.Keys...) {
		if keyID(k.Key) == id {
			q := p.WithKey(k)
			q.Keys = nil
			q.keyAliasID, q.keyAliasOf = id, len(p.KeysOn())
			q.Off = p.Off || k.Off
			if models, found := p.AccountModels[id]; found {
				q.Models = models
			}
			return q, true
		}
	}
	p.Key, p.Keys, p.Off = "", nil, true
	p.keyAliasID = id
	return p, true
}
