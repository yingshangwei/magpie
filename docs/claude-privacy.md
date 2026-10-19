# Claude process privacy boundaries

Magpie's subscription bridge removes inherited Git overrides, custom
Anthropic headers and OTel export settings before starting Claude Code.
It disables Git system/global configuration and parent repository discovery
above its temporary working directory. Telemetry, error reporting, updates,
automatic memory and official marketplace autoinstall are disabled for
these runs. Account directories, proxy and certificate configuration still
reach the process. See the [bridge reference](subsystems/claude-subscription-bridge.md).

Magpie's Go request paths drop `x-claude-code-wiggly-dove` before signing,
including case variants, repeated values and provider-configured headers.
Model discovery and balance requests apply the same rule. Other protocol,
authentication and session headers keep their existing behavior. See
[providers and accounts](subsystems/providers-accounts.md).

Neither protection inspects requests generated inside the official CLI's
TLS connection. Account identity, source IP, client/version fingerprints,
tool inputs and content deliberately sent by a user remain outside this
guarantee. Removing this diagnostic header has been tested with fixtures;
compatibility with real services has not been checked.

## Optional outer Claude launch

Magpie wires Claude Code's settings but has no dedicated launcher that
controls every interactive Claude process. The following Bash function is
an explicit, per-launch example. Set `CLAUDE_PRIVACY_PROXY` to a verified
local HTTP proxy whose routing policy sends every external destination
through the intended exit. No vendor-domain allowlist is used.

```bash
claude_private() (
    : "${CLAUDE_PRIVACY_PROXY:?Set a verified local HTTP proxy first}"
    if ! [[ "$CLAUDE_PRIVACY_PROXY" =~ ^http://127[.]0[.]0[.]1:[0-9]+$ ]]; then
        printf '%s\n' 'Expected a local HTTP proxy' >&2; exit 1
    fi
    for name in $(compgen -e); do
        case "$name" in
            OTEL_*|ANTHROPIC_CUSTOM_HEADERS|CLAUDE_CODE_ENABLE_TELEMETRY|CLAUDE_CODE_ENABLE_FEEDBACK_SURVEY_FOR_OTEL)
                unset "$name" ;;
        esac
    done
    export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
    export DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 DISABLE_AUTOUPDATER=1
    export CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1
    export HTTP_PROXY="$CLAUDE_PRIVACY_PROXY" http_proxy="$CLAUDE_PRIVACY_PROXY"
    export HTTPS_PROXY="$CLAUDE_PRIVACY_PROXY" https_proxy="$CLAUDE_PRIVACY_PROXY"
    export ALL_PROXY="$CLAUDE_PRIVACY_PROXY" all_proxy="$CLAUDE_PRIVACY_PROXY"
    export NO_PROXY='localhost,127.0.0.1,::1' no_proxy='localhost,127.0.0.1,::1'
    exec claude "$@"
)
```

This keeps the interactive agent's working directory and Git tools usable.
It does not hide repository information from that agent. Review any
Claude settings that override its environment before using the example.
The nonessential-traffic switch also prevents a fresh `/usage` request;
Magpie's own usage helper retains its existing exception to this switch.

Proxy-aware clients receive the same proxy for every external host and
only loopback bypasses. Environment variables are not a network sandbox:
an independently configured tool, MCP server or browser can use another
route. There is no direct-fallback command in this example; the official
CLI's behavior when the proxy fails has not been verified. The example
was exercised only with a fake `claude` executable. It changes neither
running processes nor saved settings.

WebFetch's domain preflight is a separate disclosure of the requested
hostname. Magpie has no verified hook before an outer Claude makes that
request. This example leaves the preflight safety check enabled; it does
not provide selective hostname privacy for WebFetch. Filtering the
official CLI's final HTTPS headers would require a supported CLI hook or
TLS interception, neither implemented here.

## Offline verification

Run under an OS network sandbox that denies external connections and real
local relays, with a temporary home, fake CLI/Keychain commands and pinned
Go caches. `internal/testenv` alone is not that OS boundary. The focused
tests below use in-memory HTTP fixtures and local Git; they need no
network listeners or real Claude executable.

```sh
go test -tags nogui ./internal/catalog ./internal/provider ./internal/gateway \
  -run '^(TestClaudePrivacy|TestClientRepositoryMetadata)' -count=1
go test -tags nogui ./internal/gateway \
  -run '^(TestClaudeUsageSendsItsRequest|TestWarmClaudeAsksClaudeCodeOnce|TestClaudeAccountProxyEnv)$' -count=1
```

These establish environment/header contracts only. The complete suite
uses dynamic loopback listeners and needs a sandbox that allows its fake
services without allowing real local proxies. A denied listener is an
unverified integration check, not a passing test. Native Windows/WSL
execution, real service compatibility, final CLI TLS metadata and outer
tool routing need separate verification.
