package provider

// AsterGate's key-usage page opens a restricted usage session with an
// encrypted key. Its account windows are shared with the account, not a
// key balance. Keep the cookie in memory for this read and close it after.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const asterGateOrigin = "https://astergate.cc"

func asterGateWindows(ctx context.Context, key string) (keyReading, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return keyReading{}, err
	}
	c := &http.Client{Transport: http.DefaultClient.Transport, Jar: jar,
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	b, _, err := asterGateRequest(ctx, c, "POST", "/challenge", nil)
	if err != nil {
		return keyReading{}, err
	}
	var challenge struct {
		Challenge string `json:"challenge"`
		PublicKey string `json:"public_key"`
	}
	if json.Unmarshal(b, &challenge) != nil || challenge.Challenge == "" {
		return keyReading{}, fmt.Errorf("AsterGate usage: invalid challenge")
	}
	jwe, err := asterGateJWE(key, challenge.Challenge, challenge.PublicKey)
	if err != nil {
		return keyReading{}, err
	}
	login, _ := json.Marshal(map[string]string{"challenge": challenge.Challenge, "jwe": jwe})
	if _, _, err = asterGateRequest(ctx, c, "POST", "/login", login); err != nil {
		return keyReading{}, err
	}
	defer func() {
		logout, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, _, _ = asterGateRequest(logout, c, "POST", "/logout", nil)
	}()
	read := func() (keyReading, string, error) {
		b, _, err := asterGateRequest(ctx, c, "GET", "/report?timezone=UTC", nil)
		if err != nil {
			return keyReading{}, "", err
		}
		return readAsterGateReport(b)
	}
	r, account, err := read()
	if err != nil {
		return keyReading{}, err
	}
	if _, refresh := refreshing(ctx); !refresh {
		return r, nil
	}
	// Only the card's explicit refresh asks AsterGate to refresh its own
	// cached OAuth reading. A cooldown keeps the report just read.
	b, _, err = asterGateRequest(ctx, c, "GET", "/refresh-status", nil)
	if err != nil {
		return keyReading{}, err
	}
	var status struct {
		Accounts []struct {
			ID        string     `json:"account_id"`
			Supported bool       `json:"supported"`
			Next      *time.Time `json:"next_allowed_at"`
		} `json:"accounts"`
	}
	if json.Unmarshal(b, &status) != nil {
		return keyReading{}, fmt.Errorf("AsterGate usage: invalid refresh status")
	}
	allowed := false
	for _, a := range status.Accounts {
		if a.ID == account {
			allowed = a.Supported && (a.Next == nil || !a.Next.After(time.Now()))
		}
	}
	if !allowed {
		return r, nil
	}
	body, _ := json.Marshal(map[string]string{"account_id": account})
	_, code, err := asterGateRequest(ctx, c, "POST", "/refresh", body)
	if code == http.StatusTooManyRequests {
		return r, nil
	}
	if err != nil {
		return keyReading{}, err
	}
	r, _, err = read()
	return r, err
}

// asterGateRequest never follows a redirect or includes the API key in a
// header, URL or error. The API key appears only inside the encrypted JWE
// at login; subsequent requests carry the restricted session cookie.
func asterGateRequest(ctx context.Context, c *http.Client, method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, asterGateOrigin+"/usage-session"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-usage-client", "1")
	req.Header.Set("x-usage-origin", asterGateOrigin)
	req.Header.Set("Origin", asterGateOrigin)
	req.Header.Set("Referer", asterGateOrigin+"/key-usage")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("AsterGate usage: %s", resp.Status)
	}
	const limit = 1 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if len(b) > limit {
		return nil, resp.StatusCode, fmt.Errorf("AsterGate usage: report too large")
	}
	return b, resp.StatusCode, err
}

// asterGateJWE follows key-usage's RSA-OAEP-256 / A256GCM compact JWE.
func asterGateJWE(key, challenge, publicKey string) (string, error) {
	enc := base64.RawURLEncoding
	der, err := enc.DecodeString(publicKey)
	if err != nil {
		return "", fmt.Errorf("AsterGate usage: invalid public key")
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", fmt.Errorf("AsterGate usage: invalid public key")
	}
	rpk, ok := pub.(*rsa.PublicKey)
	if !ok || rpk.N.BitLen() < 2048 {
		return "", fmt.Errorf("AsterGate usage: invalid RSA public key")
	}
	cek, iv := make([]byte, 32), make([]byte, 12)
	if _, err = rand.Read(cek); err != nil {
		return "", err
	}
	if _, err = rand.Read(iv); err != nil {
		return "", err
	}
	ek, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, rpk, cek, nil)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	header := enc.EncodeToString([]byte(`{"alg":"RSA-OAEP-256","enc":"A256GCM"}`))
	payload, _ := json.Marshal(map[string]string{"key": key, "challenge": challenge, "origin": asterGateOrigin})
	sealed := gcm.Seal(nil, iv, payload, []byte(header))
	n := len(sealed) - gcm.Overhead()
	return strings.Join([]string{header, enc.EncodeToString(ek), enc.EncodeToString(iv), enc.EncodeToString(sealed[:n]), enc.EncodeToString(sealed[n:])}, "."), nil
}

func readAsterGateReport(b []byte) (keyReading, string, error) {
	var report struct {
		Service  string `json:"service"`
		Protocol string `json:"protocol"`
		Version  int    `json:"protocol_version"`
		Scope    string `json:"account_scope"`
		Shared   bool   `json:"account_quota_shared"`
		Accounts []struct {
			ID       string `json:"account_id"`
			Provider string `json:"provider"`
			Plan     string `json:"plan"`
			Windows  []struct {
				Account string     `json:"account_id"`
				Kind    string     `json:"kind"`
				Scope   string     `json:"scope"`
				Model   string     `json:"scoped_model"`
				Ratio   *float64   `json:"used_ratio"`
				Reset   *time.Time `json:"resets_at"`
				Checked time.Time  `json:"checked_at"`
			} `json:"windows"`
		} `json:"accounts"`
	}
	if json.Unmarshal(b, &report) != nil || report.Service != "astergate" || report.Protocol != "astergate-self-usage" || report.Version != 1 {
		return keyReading{}, "", fmt.Errorf("AsterGate usage: unsupported report")
	}
	// A key's pooled accounts cannot honestly be combined into one account's
	// percentages. Only the account scope verified by this protocol is read.
	if report.Scope != "accounts" || !report.Shared || len(report.Accounts) != 1 {
		return keyReading{}, "", fmt.Errorf("AsterGate usage: expected one shared account")
	}
	a := report.Accounts[0]
	if a.ID == "" || a.Provider != "claude" {
		return keyReading{}, "", fmt.Errorf("AsterGate usage: unsupported account")
	}
	r := keyReading{plan: a.Plan}
	seen := map[string]bool{}
	for _, w := range a.Windows {
		if w.Account != a.ID || w.Ratio == nil || *w.Ratio < 0 || *w.Ratio > 1 || w.Reset == nil || w.Reset.IsZero() || w.Checked.IsZero() || seen[w.Kind] {
			return keyReading{}, "", fmt.Errorf("AsterGate usage: incomplete quota window")
		}
		q := QuotaWindow{Used: *w.Ratio * 100, ResetsAt: w.Reset}
		switch {
		case w.Kind == "five_hour" && w.Scope == "account":
			q.Name, q.Span = "5 hours", 5*time.Hour
		case w.Kind == "weekly" && w.Scope == "account":
			q.Name, q.Span = "7 days", 7*24*time.Hour
		case w.Kind == "weekly_fable" && w.Scope == "model" && strings.EqualFold(w.Model, "fable"):
			q.Name, q.Span, q.Model = "Fable · 7 days", 7*24*time.Hour, "fable"
		default:
			return keyReading{}, "", fmt.Errorf("AsterGate usage: unsupported quota window")
		}
		seen[w.Kind] = true
		r.ws = append(r.ws, q)
		if r.at.IsZero() || w.Checked.Before(r.at) {
			r.at = w.Checked
		}
	}
	if !seen["five_hour"] || !seen["weekly"] {
		return keyReading{}, "", fmt.Errorf("AsterGate usage: missing account windows")
	}
	return r, a.ID, nil
}
