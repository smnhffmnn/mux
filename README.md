# mux

Single-binary MCP gateway for databases, APIs, and tunnels.

## What is mux?

mux is a [Model Context Protocol](https://modelcontextprotocol.io) gateway that gives AI agents unified access to databases, upstream MCP servers, and API services through one endpoint. It connects to private networks via built-in WireGuard tunnels (no root required), runs as a desktop app with system tray or headless for servers and CI, and stores secrets in an encrypted vault or your OS keychain. No Docker, no Python, no runtime dependencies.

## Features

- **Databases** -- MariaDB, PostgreSQL, ClickHouse with schema introspection and query tools
- **MCP Proxy** -- forward tool calls to upstream MCP servers (YouTrack, Sentry, Notion, Netdata, etc.)
- **API Integrations** -- Microsoft Graph (mail + SharePoint), Google Tag Manager, Google Gemini, OpenAI, ElevenLabs, Brave Search, Firecrawl
- **Generic HTTP** -- connect any REST API with optional auth headers and file output
- **WireGuard Tunnels** -- reach databases on private networks through userspace WireGuard (no root, no VPN client)
- **Remote Provisioning** -- centrally manage connections and tunnels for your team from a single HTTP endpoint
- **Two Transports, Three Modes** -- stdio (Claude Desktop) and Streamable HTTP (`/mcp`), with auto-detection of desktop, headless, and stdio modes
- **Desktop App** -- Wails v3 + Svelte 5 with system tray, web UI, OAuth flows, and connection testing
- **Encrypted Vault** -- hardware-security-grade secret storage with AES-256-GCM, Argon2id key derivation, inactivity auto-lock, and WebAuthn/FIDO2 unlock (FaceID, YubiKey)
- **OS Keychain** -- alternative secret storage via macOS Keychain, GNOME Keyring, or Windows Credential Manager
- **Approval Flow** -- human-in-the-loop approval for privileged agent actions (git push, etc.) with configurable notifications (Telegram, Discord) and browser-based WebAuthn approval page
- **Dynamic Config** -- add, remove, and manage connections at runtime via MCP tools
- **Cross-Platform** -- macOS, Linux, Windows (amd64 + arm64)

## Installation

### Homebrew (macOS)

```bash
brew tap smnhffmnn/tap
brew install mux                # CLI only
brew install --cask mux         # Desktop app (installs to /Applications)
```

> **macOS Gatekeeper:** The app is not code-signed yet. On first launch macOS may show "mux is damaged and can't be opened". Fix with:
> ```bash
> xattr -cr /Applications/mux.app
> ```

### GitHub Releases

Download the latest binary from [Releases](https://github.com/smnhffmnn/mux/releases).

### Build from Source

```bash
# Headless (all platforms, no CGO required)
git clone https://github.com/smnhffmnn/mux.git
cd mux && CGO_ENABLED=0 go build -tags notray -ldflags "-s -w" -o mux .

# Desktop (macOS, requires CGO + Wails v3)
make build
```

## Quick Start

Create `~/.mux/config.toml`:

```toml
[[connections]]
name = "mydb"
type = "mariadb"
host = "localhost"
port = 3306
user = "root"
database = "myapp"
```

Run mux:

```bash
mux
```

mux auto-detects the mode: piped stdin = stdio mode, no display = headless HTTP server, terminal with display = desktop app + HTTP server on port 7700.

Set the database password (via your MCP client or the web UI):

```
secret_set key=mydb-password value=<your-password>
```

## Usage

### With Claude Desktop

Add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "mux": {
      "command": "mux"
    }
  }
}
```

Claude Desktop pipes stdin, so mux runs in stdio mode automatically.

### With Claude Code

Add to your project's `.mcp.json`:

```json
{
  "mcpServers": {
    "mux": {
      "type": "http",
      "url": "http://localhost:7700/mcp"
    }
  }
}
```

### Desktop Mode

Run `mux` from a terminal. A system tray icon appears (macOS) and the web UI is available at `http://localhost:7700/ui`. Use the web UI to manage connections, test them, configure OAuth flows, and set up remote provisioning.

### Headless Mode

On servers without a display (no `DISPLAY` or `WAYLAND_DISPLAY`), mux automatically starts in headless HTTP mode -- no flags needed:

```bash
mux                                        # auto-detects headless
mux --port 8080 --config ./my-config.toml  # custom port and config
```

## Configuration

Config file: `~/.mux/config.toml`

Connection settings are loaded with this priority (highest wins): **Environment variables > TOML file > Defaults**

Secrets (passwords, tokens, keys) have their own resolution chain: **Encrypted Vault > OS Keychain > File fallback** (`~/.mux/secrets.toml`)

```toml
[server]
port = 7700

[[connections]]
name = "production"
type = "postgresql"
host = "10.100.0.5"
port = 5432
user = "readonly"
database = "production"
read_only = true
tunnel = "office-vpn"
instructions = "Production database. Read-only access."
# Password stored as secret (key: "production-password")

[[connections]]
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"
# Token stored as secret (key: "youtrack-token")

[[tunnels]]
name = "office-vpn"
peer_public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
peer_endpoint = "vpn.example.com:51820"
allowed_ips = "10.100.0.0/16"
tunnel_address = "10.100.0.42/32"
dns = "10.100.0.1"
# Private key stored as secret (key: "tunnel-office-vpn-private-key")
```

See [docs/configuration.md](docs/configuration.md) for the full reference.

## Vault (Encrypted Secret Store)

The vault encrypts all secrets at rest using AES-256-GCM with Argon2id key derivation. Secrets are only accessible in memory while the vault is unlocked, and automatically wiped after a configurable inactivity timeout. The vault is the recommended secret backend for headless deployments; without it, mux falls back to the OS keychain or a plaintext `secrets.toml` file.

### Setup

```toml
[server]
port = 7700                        # HTTP (localhost only, for MCP clients)
tls_cert = "/path/to/cert.pem"    # enables HTTPS server for WebAuthn/Vault
tls_key = "/path/to/key.pem"
tls_port = 7701                    # HTTPS port (default: port + 1), all interfaces

[vault]
enabled = true
exclusive = true                   # secrets only in vault, not in legacy keyring/file
inactivity_timeout = "30m"
webauthn_rp_id = "mux.example.com"
webauthn_origins = ["https://mux.example.com:7701"]
base_url = "https://mux.example.com:7701"
```

The dual-port architecture keeps MCP clients (`.mcp.json`) on plain HTTP localhost — unchanged across machines. The HTTPS port is only needed for browser-based WebAuthn and Vault unlock, reachable via Tailscale or LAN.

### Unlock Methods

1. **Passphrase** -- via MCP tool `vault_unlock` or `POST /vault/unlock`
2. **WebAuthn/FIDO2** -- via browser (FaceID, YubiKey) at `POST /vault/webauthn/login/*`

### MCP Tools

| Tool | Description |
|------|-------------|
| `vault_status` | Show vault state, secret count, credential info, remaining lock time |
| `vault_init` | Initialize vault with a passphrase (creates `~/.mux/vault.json` + `vault.key`) |
| `vault_unlock` | Unlock with passphrase |
| `vault_lock` | Lock immediately (wipes DEK from memory) |
| `vault_migrate` | Migrate existing secrets from keychain/file into the encrypted vault |

### Approval Flow

The approval flow requires the vault to be enabled. When agents need to perform privileged actions (e.g., `git push`), the approval system creates a request, notifies the user via configured notification channels, and blocks until the user approves via WebAuthn in their browser.

```
Agent wants to push → POST /vault/approval → Notification sent
→ User opens approval link on phone → FaceID/YubiKey → Approved → Agent proceeds
```

Configure notification channels via secrets (one or both):
```
# Telegram Bot API:
secret_set key=vault-telegram-bot-token value=<bot-token>
secret_set key=vault-telegram-chat-id value=<chat-id>

# Discord Webhook:
secret_set key=vault-discord-webhook value=https://discord.com/api/webhooks/...
```

If multiple channels are configured, notifications are sent to all of them. If none are configured, approval requests are logged to stdout.

A PreToolUse hook script for Claude Code is included at `scripts/approval-hook.sh`.

### HTTP API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/vault/status` | GET | Vault state |
| `/vault/init` | POST | Initialize vault |
| `/vault/unlock` | POST | Unlock with passphrase (rate-limited) |
| `/vault/lock` | POST | Lock vault |
| `/vault/migrate` | POST | Bulk migrate secrets |
| `/vault/webauthn/register/begin` | POST | Start credential registration |
| `/vault/webauthn/register/finish` | POST | Complete registration |
| `/vault/webauthn/login/begin` | POST | Start authentication |
| `/vault/webauthn/login/finish` | POST | Complete authentication (returns session token) |
| `/vault/webauthn/credentials` | GET | List registered credentials |
| `/vault/approval` | POST | Create approval request |
| `/vault/approval/{id}` | GET | Check approval status |
| `/vault/approval/{id}/grant` | POST | Grant (requires session token) |
| `/vault/approval/{id}/deny` | POST | Deny |
| `/vault/approve/{id}` | GET | Approval page (HTML) |
| `/vault/approvals` | GET | List pending approvals |

## Documentation

| Document | Description |
|----------|-------------|
| [Configuration](docs/configuration.md) | Complete TOML reference, environment variables, keychain conventions |
| [Connections](docs/connections.md) | Every connection type with fields, examples, and exposed MCP tools |
| [Tunnels](docs/tunnels.md) | WireGuard tunnel architecture, configuration, fail-closed logic |
| [Provisioning](docs/provisioning.md) | Remote provisioning API contract, JSON schema, server examples |
| [Deployment](docs/deployment.md) | Building, cross-compilation, systemd, Docker, CI/CD |

## License

[MIT](LICENSE)
