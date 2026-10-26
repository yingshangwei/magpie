# Peer federation over WebSocket

Two gateways share explicitly selected providers and models over one
bidirectional WebSocket. Either computer can initiate it. Once connected,
both can send requests, so the computer offering models can be behind NAT.
At least one endpoint must be reachable; this feature does not supply a
public relay or NAT traversal when neither endpoint is reachable.

## Responsibilities and sources of truth

| Part | Responsibility | Source |
| --- | --- | --- |
| Local policy | Private `peers.json`, strict version 1 JSON, shared pairing token, exact provider/model selections and separate send/receive permissions | [`config.go`](../../internal/peering/config.go) |
| Link lifetime | Authenticate before upgrade, connect/reconnect, cancel on policy change, periodic catalog refresh | [`hub.go`](../../internal/peering/hub.go) |
| Wire | Version handshake, independent request ids in each direction, bounded concurrency and response buffers, streaming, cancellation and heartbeat | [`peerwire`](../../internal/peerwire/conn.go), [`streams`](../../internal/peerwire/stream.go) |
| Gateway boundary | Small operation allow list, exact model authorization, original local gateway handlers, local-only administration | [`peers.go`](../../internal/gateway/peers.go) |
| Imported provider | `peer-<link-id>` in `providers.json`, no API key, in-process transport to the existing link | [`peer.go`](../../internal/provider/peer.go), [`proxy.go`](../../internal/provider/proxy.go), [`transport.go`](../../internal/netproxy/transport.go) |
| Configuration copy | Explicit API configuration schema, scoped model list, optional API keys, copy ownership and native-account restriction | [`peer_sync.go`](../../internal/provider/peer_sync.go) |
| CLI | `peer init`, `apply`, `list`, `status`, `refresh`, `sync` | [`peers_cli.go`](../../internal/cli/peers_cli.go) |
| Device settings | Local pairing, exact model selection, status and explicit provider push in Settings → Sync and backup → Manage devices | [`peers.go`](../../internal/gui/peers.go), [`peers.js`](../../internal/gui/assets/peers.js) |

`peers.json` belongs to this computer and is not included in setup backups.
It is written atomically with mode 0600. A read or parse failure refuses
peer operations and closes active links at reconciliation. No peers file
means there are no links. CLI status and list never return pairing tokens.

## Two-computer setup

In the desktop app, open **Settings → Sync and backup → Device links →
Manage devices**. Choose **Create pairing**, name the other computer, choose
what this computer will share or receive, and Save. The resulting pairing
code goes into **Join pairing** on the other computer. Joining reads only
the link ID and secret from the code; it does not accept permissions from
it. Only the computer dialing fills in the other's reachable WSS address.
An empty address waits for a connection. A local SSH tunnel can use WS on
loopback. Pairing does not provide a public relay or change network sharing.

Each model has independent **Share calls** and **Copy config** selections.
Sharing calls keeps keys at the serving computer. Copying configuration
requires an explicit **Sync providers… → Sync now** action and the receiver's
exact source-provider allow list. Sending and receiving keys are separate
unchecked choices. Native Claude/Codex, subscription, plugin and remote
provider configurations are not offered for copying. No sharing, import or
credential permissions are enabled by creating a pairing.

The device list hides pairing tokens; **Pairing code** reveals one only
when asked. The GUI reads and saves locally even when a separate daemon
serves the gateway; live status, refresh and sync go to that daemon's local
control routes. An unreachable daemon is shown as an error, independently
of the saved device list. A failed policy read is never displayed as an
empty list. Device controls reject remote web clients, forwarded requests,
foreign origins, frames and non-loopback Host headers; use the desktop app
or a signed-in browser on the same computer.

Editors save one link under the same file lock as the CLI and check its
revision. An edit, removal or sync preview that became stale is refused;
unrelated links added meanwhile remain intact. A sync carries the displayed
revision through to the running gateway, which checks it before exporting.
Removing or disabling a link closes it at reconciliation; API provider
copies already received remain independent local providers.

The CLI is also available:

Use a gateway built from this branch on both ends. `peer init home-link
peer.json` writes a private template with a fresh random token; it grants
no permissions. Transfer that token privately, keep the same link id at
both ends, and edit a separate JSON file on each computer. Provider and
model ids are exact ids from that computer's model list.
Link and source-provider ids use lowercase letters and digits separated
by single dashes, with at most 48 characters.

For example, A has a reachable gateway and imports B's selected models:

```json
{
  "version": 1,
  "peers": [{
    "id": "home-link",
    "name": "Computer B",
    "token": "REPLACE_WITH_THE_GENERATED_PAIRING_TOKEN",
    "import": true,
    "syncReceive": ["my-api"]
  }]
}
```

B initiates the connection and exposes one model from its native Claude
provider and one from an API provider. Substitute actual enabled model ids:

```json
{
  "version": 1,
  "peers": [{
    "id": "home-link",
    "name": "Computer A",
    "url": "wss://a.example.com/v1/peers/connect",
    "token": "REPLACE_WITH_THE_GENERATED_PAIRING_TOKEN",
    "exports": [
      {"provider": "claude", "models": ["YOUR_CLAUDE_MODEL_ID"]},
      {"provider": "my-api", "models": ["YOUR_API_MODEL_ID"]}
    ],
    "syncSend": [
      {"provider": "my-api", "models": ["YOUR_API_MODEL_ID"]}
    ]
  }]
}
```

On each computer run `peer apply <its-file.json>`, then serve that gateway.
Only one end sets `url`. Across a network it must use WSS with a verified
certificate. The receiving gateway can use its existing `serve --tls-cert
<cert> --tls-key <key>` options; its configured listen address and IP allow
list must permit the connection. A TLS-terminating proxy may forward to a
loopback listener. Plain WS is accepted only on loopback, including through
an explicitly configured SSH tunnel. Pairing does not change the listen
address, firewall, ordinary LAN sharing, or gateway access keys.
The gateway cannot detect an unencrypted network hop hidden behind a
loopback TCP forwarder; that forwarder must itself use TLS or SSH.

`peer status` reports the link. A's `peer refresh home-link` refreshes the
list immediately; connection establishment and every 30 seconds also do
this. A displays **Computer B (WS)** as a separate provider. Its model ids
are `peer-home-link/claude/YOUR_CLAUDE_MODEL_ID` and
`peer-home-link/my-api/YOUR_API_MODEL_ID` in this example. The provider
supports the Chat, Responses and Messages gateway protocols using the
remote model's advertised capabilities; it is not tied to one protocol.
The imported provider's models can be selected in Providers as usual.

The first import selects the shared models. Later catalog refreshes keep
the local selection, including Select none, and remove models no longer
shared. Newly shared models become available to pick without being enabled
automatically. If every selected model disappears, the selection stays
empty until the user picks models again.

For a peer provider, CLI `models all` (or `models -`) saves the current
known list explicitly. It does not opt in to future remote additions.
An older implicit selection is frozen from the previous readable cache
before a refreshed list is published. If that cache is missing or broken,
refresh leaves both the provider and the cache unchanged and reports an
error: select exact models or none to recover. When that cache is missing
or broken, ordinary edits also need an explicit selection before saving.
Explicit selections can
still refresh and rebuild a missing cache. `peer status` reports a failed
background refresh; `peer refresh <id>` returns the detailed error.

Catalog refresh changes only the model selection and its listed-model
baseline. It preserves local names, Off and routing settings, and retries
its merge if another provider edit committed first. The stored ids of peer
tunnel providers and synced API copies belong to their peer and cannot be
renamed; their display names remain editable. Renaming an ordinary local
provider still requires updating any exact export/sync source ids in peer
configuration; existing synced copies are not migrated automatically.

To share in the other direction on the same connection, add exact `exports`
on A and enable `import` on B. No second connection is needed.

## Request path and permissions

An A client selects an imported model. A's normal provider adapter sends
through the peer transport. B verifies the model against its current
`exports` and passes the request through B's local gateway. A native Claude
model therefore uses B's existing Claude Code subscription bridge; its
sign-in and child process remain on B. Responses, including SSE chunks and
tool calls, return over the same link. This is model access, not an account
copy. B owns its normal provider/account selection and runtime behavior.

Missing or empty export lists share nothing. Model names are exact, with no
wildcards or group expansion. A request must name the remote provider as
well as the model; routing candidates on B are checked again. Routing
groups and remote providers cannot be re-exported to another peer. Thus
peer A → peer B → peer C is not supported. An ordinary authorized HTTP API
client may call B's imported peer provider, and B forwards that model
request over its existing connection to C. This uses B's normal gateway
access controls; it is not a generic TCP proxy or a two-peer relay service.
API credentials, account data, GUI, callbacks,
files and administrative routes are outside the tunnel's operation list.
The wire accepts only relative operation paths. Model access is limited to
Chat, Responses, Messages, count_tokens, embeddings and rerank; image/video
and file APIs are not exposed.

A native advisor's model must also be in the selected provider's export
list. This is checked on admission and again for each routing candidate;
a fallback cannot use another provider's unshared advisor model.

Each incoming request uses a peer identity restricted to the exported
models. Caller authorization, cookies and arbitrary headers are discarded;
only protocol and session headers are carried through. Imported providers
never fall back from `peer.invalid` to ordinary HTTP. New models do not
expand a peer or synced provider's explicit model selection automatically.
On B, session identifiers are namespaced by link for usage and account
affinity. Native Claude bridge processes and saved turns also remain in
that link's ownership domain, separate from B's local clients and other
peers. Within one link, its clients share that link's permissions.

Changes to local policy are checked on each operation; the reconciliation
loop closes changed links within two seconds, including their requests.
Turning `import` off or losing the link makes the saved imported provider
unusable immediately, even if its old model names are still visible.
The cache is refreshed on reconnection. Removing a link does not silently
delete a provider the user may have referenced in groups or agent settings.

## Connection and failure behavior

The wire protocol is version 1; a mismatched version fails the handshake.
Each peer has one connection slot, including dialing, HTTP upgrade and
the protocol handshake. A changed pairing token or authorization requires
a new connection; the old session never acquires newly added permissions.
The connection carries up to eight requests in each direction by default,
with a 32 MiB request body limit and a separate 256 KiB response window per
request. Slow consumers apply backpressure to their own streams. Heartbeats
detect dead links; reconnect uses exponential backoff with jitter, capped
at 30 seconds. Closing a client response cancels its request on B.
Each link can export at most 4096 models in total. Catalog payloads are
limited to 8 MiB; policy files and configuration-copy payloads to 1 MiB.

A broken peer exchange is terminal for that request, even if A's ordinary
gateway retry or group fallback would otherwise try again. B may already
have started generating, so A cannot know whether it is safe to repeat it.
Reconnection serves future requests only. This does not promise exactly
once execution if a client submits a new request after a failure. A normal
HTTP error received completely from B still follows ordinary routing rules.
This rule also applies to embeddings and rerank group fallback: a broken
exchange returns its error without sending the request to another member.

## Provider configuration push

B runs `peer sync home-link my-api` to push the models named in its
`syncSend` entry. A must list `my-api` in `syncReceive`. This is an explicit
push, with no background configuration synchronization. The received
provider is `sync-home-link-my-api`, owned by that link and source; it
cannot replace an unrelated local provider. It connects directly to its
copied API endpoints, independently of subsequent peer connectivity.
Allowing configuration receipt trusts that peer to choose and update
those API endpoints, including private-network addresses. A received copy
is an ordinary local API provider: its owner can use, back up or explicitly
share it again. Model tunneling alone never transfers those credentials.

By default only API URLs, name, selected models and a narrow set of public
protocol headers are copied. A copy without a key is saved disabled.
To include API keys and other headers, B must set `keys: true` on that
`syncSend` entry and A must set `receiveKeys: true`. Sending a later keyless
copy removes the earlier copied credentials and disables that copy.
Synced models remain explicit rather than acquiring newly listed models.
Saving either kind of peer provider in the GUI keeps its link or copy
ownership, so a tunneled provider still needs no API key and a later
configuration push can update its existing copy.

`NativePeerSyncEnabled` is a compile-time `false` constant checked in
export and import. Native Claude and Codex configuration copying is
unavailable regardless of JSON settings. Other subscription accounts,
plugin account stores, remote providers and machine credentials are also
outside the copy schema. Proxies, fallbacks, local paths and runtime state
are not copied. This restriction does not prevent exporting their selected
local models through the request tunnel.

## Verification

Tests use isolated configuration homes and local fake HTTP/WebSocket
servers. They cover bidirectional calls, incremental SSE, cancellation,
backpressure, disconnect without replay (chat and retrieval), completed
HTTP error fallback, malformed frames, explicit model scope, refresh after
local selections or catalog changes, policy revocation, sanitized headers,
configuration permissions and GUI edits followed by another sync.
Device-editor tests cover a cancelled draft, redacted pairing codes, stale
edit/sync rejection, per-link changes, unreadable policies, local-only
administration and a separate gateway process. Browser tests cover pairing,
sharing/copy scopes, key consent, failed sync and removal at desktop and
narrow widths in all five languages, in Chromium and WebKit.
The native bridge test uses the existing fake Claude Code process fixture;
it does not establish real-account or two-physical-machine compatibility.

```sh
go test -race ./internal/peerwire ./internal/peering
go test -race -tags nogui ./internal/gateway ./internal/provider ./internal/cli ./internal/netproxy ./internal/gui -run TestPeer
go test -tags nogui -timeout 30m ./...
node --test internal/gui/tests/peer-devices.test.cjs
```
