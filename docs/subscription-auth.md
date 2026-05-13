# Subscription Auth

Sage-router can route requests through your OpenAI, Anthropic, Google,
and GitHub Copilot **subscriptions** instead of metered API keys. The
gateway holds the OAuth credentials in encrypted form and refreshes them
automatically.

## When subscription auth helps

Use it when you already pay for a Plus / Pro / Enterprise plan and want
to route programmatic traffic through the same entitlement, without
billing it as separate API usage. A request served by a subscription
connection records `cost = 0` and `cost_source = "subscription"` in the
usage log; the dashboard shows "Subscription savings" — what those same
tokens would have cost at current API rates.

You still need an API key when:

- The provider doesn't ship subscription auth (OpenRouter, Ollama).
- The model you want is gated to API-key authentication.
- A subscription endpoint is rate-limited / blocked for your account.

Sage-router treats API-key and subscription connections as peers under
the same provider — you can run several of each in parallel and the
selector will pick the best by priority and state.

## What's supported

| Provider        | PKCE login   | CLI import           | API key fallback |
| --------------- | ------------ | -------------------- | ---------------- |
| OpenAI / ChatGPT | yes (port 1455) | `~/.codex/auth.json` | yes |
| Anthropic / Claude | yes (port 53692) | `~/.claude/.credentials.json` (or Keychain on macOS) | yes |
| Google / Gemini | import-only  | `~/.gemini/oauth_creds.json` | yes |
| GitHub Copilot  | import-only  | `~/.config/github-copilot/hosts.json` | (provider has no public API key) |

PKCE login binds a localhost port (1455 for OpenAI, 53692 for
Anthropic). Those numbers are fixed because they're hard-coded in each
provider's OAuth client allow-list. If the corresponding CLI tool
(Codex, Claude Code) is running, sage-router will report **port busy**
until it's stopped.

## Adding a subscription via the dashboard

1. Open the dashboard and go to **Providers → + Add Provider**.
2. Pick the provider.
3. Click **Login with subscription** (PKCE) or **Import from local CLI**.
4. The first time you do this, sage-router shows a terms-of-service
   modal. Accept it once — it persists across restarts.
5. For PKCE: a new tab opens at the provider's login page. After you
   approve, the tab redirects back to sage-router and the connection
   appears in the list.

If the dashboard reports **port busy**, stop whichever local CLI tool
is holding the port and click **Retry bind**.

## Adding a subscription via the CLI

The CLI writes directly to the SQLite store. It needs the master secret
to encrypt the tokens — set `SAGE_MASTER_SECRET` or place a 32-byte key
file at `~/.sage-router/master.key` (sage-router writes it for you on
startup).

```sh
# Run a PKCE flow — opens your browser to the provider's login page.
sage-router auth login --provider openai

# Import an existing CLI's credentials.
sage-router auth import --provider anthropic

# List the subscription connections currently configured.
sage-router auth status

# Remove one (or all subscription rows for a provider).
sage-router auth logout --provider openai
sage-router auth logout --id <connection-id>
```

### Exit codes

| Code | Meaning |
| ---- | ------- |
| 0    | success |
| 4    | bridge port is in use by another tool (stop Codex / Claude Code and retry) |
| 5    | no master secret available (set `SAGE_MASTER_SECRET` or write `~/.sage-router/master.key`) |
| 6    | you declined the terms of service |
| 1    | other error (see stderr) |

### Headless / cross-machine login

If the machine running sage-router has no browser, the CLI also accepts
a pasted redirect URL: open the authorize URL on any device, complete
the login, then paste the final `http://localhost:1455/?code=...&state=...`
back into the CLI prompt. The CLI parses `code` and `state` from the
pasted URL and verifies `state` against the one it generated for this
flow; mismatched states are rejected before any token exchange happens.

## Refresh, expiry, and recovery

Sage-router runs a refresh sweeper every minute. Tokens with less than 5
minutes left are refreshed proactively; tokens that fail an upstream
request with 401/403 are refreshed reactively. After three consecutive
refresh failures the connection is disabled and the dashboard shows a
**Re-authenticate** button on its row.

If a subscription is blocked at the provider level — Anthropic, in
particular, has explicit restrictions on third-party use of OAuth tokens
— the dashboard switches the button to **Switch to API key**, which lets
you paste a key without losing the connection's history.

## Privacy and security

- Access and refresh tokens are encrypted with AES-256-GCM derived
  from your master secret. The plaintext never touches disk.
- Tokens are never echoed to API responses, server logs, or the
  dashboard. The list endpoint returns expiry, account ID, and recent
  error text only when a connection is in a state where you need to
  act on it.
- The TOS gate runs on the first subscription action of each
  sage-router instance and persists across restarts.
- PKCE state values are never logged in full — only short prefixes for
  diagnostics.

## Troubleshooting

**"Bridge port unavailable"** — Codex CLI (port 1455) or Claude Code
(port 53692) is running on the same machine. Stop it, then use the
dashboard's **Retry bind** button or rerun the CLI command.

**"TOS required"** — the dashboard returns 412 on the first subscription
action; the CLI prompts on stdin. Accept once; it won't ask again.

**"invalid_grant"** during refresh — the upstream token has been
revoked. The CLI's `auth login --provider <p>` will create a new
connection. Delete the old one via `auth logout --id <id>` or the
dashboard.

**"invalid_client" / "blocked" / "forbidden"** in `last_error` — the
provider has rejected the client. The dashboard surfaces the
**Switch to API key** action; you'll need an API key from the
provider's developer console.

**Anthropic prompt-caching headers** — verified to work with
subscription tokens at the time of this release. If a future endpoint
change rejects them, the gating logic in `cost/cache.go` can be
extended to strip cache-control for Anthropic subscription
connections, mirroring how Gemini-subscription is handled today.
