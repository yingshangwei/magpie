package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/peering"
)

const peerUsage = `Connect two gateways over one bidirectional WebSocket:
  magpie peer init <link-id> <file>  write a private template with a new pairing token
  magpie peer apply <file>          validate and save this computer's peer configuration
  magpie peer list                  list configured links (never prints tokens)
  magpie peer status                show live connection status
  magpie peer refresh <link-id>      refresh this peer's shared model catalog
  magpie peer sync <link-id> <provider-id>  push the configured provider selection

Both computers use the same link id and token. Give only the dialing end a
url (wss://host/v1/peers/connect). It may be behind NAT. Across a network use
WSS; ws:// is accepted only on loopback, including a local SSH tunnel.
At least one end must be reachable. A relay for two unreachable ends is not included.

Edit the JSON template before applying. import enables models from the peer.
exports and syncSend contain {"provider":"id","models":["model-id"]}; empty
lists share nothing. syncSend may explicitly add "keys":true to send API keys.
syncReceive lists provider ids allowed to be copied here; receiveKeys must also
be true to receive their keys. Sync runs only when explicitly requested.
Native Claude and Codex configuration sync is disabled in code. Subscription,
plugin and remote-provider credentials are never copied. A copy without a key
is stored disabled. Copied providers use sync-<link-id>-<provider-id>.

The gateway reloads peers.json within two seconds. Changing policy closes the
link and cancels its requests. Model catalogs refresh on connection and every
30 seconds. Existing requests are never replayed after disconnection.
See docs/subsystems/peer-federation.md for a two-computer example.
`

func peerCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(peerUsage)
		return nil
	}
	switch args[0] {
	case "init":
		if len(args) != 3 || !peering.ValidID(args[1]) || strings.HasPrefix(args[2], "-") {
			return errors.New("usage: magpie peer init <link-id> <file>")
		}
		cfg := peering.Config{Version: 1, Peers: []peering.Peer{{ID: args[1], Name: args[1], Token: peering.NewToken()}}}
		b, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(args[2], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(append(b, '\n'))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Println("Private pairing template written to", args[2])
		return nil
	case "apply":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("usage: magpie peer apply <file>")
		}
		f, err := os.Open(args[1])
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, peering.MaxConfigBytes+1))
		if err != nil {
			return err
		}
		c, err := peering.Decode(b)
		if err != nil {
			return err
		}
		if err := peering.Save(c); err != nil {
			return err
		}
		fmt.Println("Peer configuration saved. The serving gateway reloads it within two seconds.")
		return nil
	case "list":
		if len(args) != 1 {
			return errors.New("usage: magpie peer list")
		}
		c, err := peering.Load(peering.Path())
		if err != nil {
			return err
		}
		for _, p := range c.Peers {
			fmt.Printf("%s\t%s\timport=%t\texports=%d\tsync-send=%d\tsync-receive=%d\toff=%t\n", p.ID, p.Name, p.Import, len(p.Exports), len(p.SyncSend), len(p.SyncReceive), p.Off)
		}
		return nil
	case "status":
		if len(args) != 1 {
			return errors.New("usage: magpie peer status")
		}
		return peerCall(http.MethodGet, "/v1/peers/status", nil)
	case "refresh":
		if len(args) != 2 || !peering.ValidID(args[1]) {
			return errors.New("usage: magpie peer refresh <link-id>")
		}
		return peerCall(http.MethodPost, "/v1/peers/refresh", map[string]string{"peer": args[1]})
	case "sync":
		if len(args) != 3 || !peering.ValidID(args[1]) || !peering.ValidID(args[2]) {
			return errors.New("usage: magpie peer sync <link-id> <provider-id>")
		}
		return peerCall(http.MethodPost, "/v1/peers/sync", map[string]string{"peer": args[1], "provider": args[2]})
	default:
		return errors.New("unknown peer command; use magpie peer --help")
	}
}

func peerCall(method, path string, payload any) error {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, gateway.URL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "magpie/peer-cli")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return errors.New("cannot reach the local gateway; start this branch's gateway first")
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, peering.MaxConfigBytes+1))
	if err != nil || len(b) > peering.MaxConfigBytes {
		return errors.New("invalid local gateway response")
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("peer operation failed (%d): %s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return nil
}
