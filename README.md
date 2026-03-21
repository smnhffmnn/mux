# mux -- MCP Unified Exchange

Single-binary MCP gateway that gives AI agents unified access to databases, upstream MCP servers, and private networks -- all through one [Model Context Protocol](https://modelcontextprotocol.io) endpoint. No Docker, no Python, no runtime dependencies.

## Features

- **Native Database Tools** -- PostgreSQL, MariaDB, and ClickHouse via Go SQL drivers with dynamic connection management
- **Proxy Mounts** -- forward tool calls to upstream MCP servers (YouTrack, Sentry, Notion, Netdata, etc.) with Bearer token or OAuth 2.0 + PKCE
- **API Integrations** -- Microsoft Graph, Google Tag Manager, Firecrawl, Brave Search, OpenAI, ElevenLabs
- **WireGuard Tunnels** -- reach databases on private networks through userspace WireGuard (no root, no VPN client)
- **ERP Provisioning** -- centrally manage connections and tunnels from an ERP system
- **Dual Transport** -- stdio (Claude Desktop / piped agents) and Streamable HTTP (`/mcp`) with auto-detection
- **Credential Store** -- env vars > OS keychain > TOML config, secrets never in plaintext
- **Desktop App** -- native Wails v3 desktop app (Svelte 5 frontend) for managing connections, testing, and OAuth flows
- **Cross-Platform** -- darwin, linux, windows (amd64 + arm64)

## Installation

### Homebrew

```bash
brew install smnhffmnn/tap/mux
```

### GitHub Releases

Download the latest binary from [Releases](https://github.com/smnhffmnn/mux/releases).

### Build from Source

```bash
# Headless (all platforms, no CGO required)
go install github.com/smnhffmnn/mux@latest

# Desktop (macOS, requires CGO + Wails v3)
git clone https://github.com/smnhffmnn/mux.git
cd mux
make build
```

## Quick Start

```bash
# Run (auto-detects: terminal = desktop app + HTTP, pipe = stdio)
mux

# Custom port and config
mux --port 8080 --config ./my-config.toml
```

### Claude Desktop

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

### Claude Code

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

## Configuration

Config file: `~/.mux/config.toml` (see [`config.example.toml`](config.example.toml))

Priority (highest wins): Environment variables > OS Keychain > TOML file > Defaults

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
# Password stored in keychain (key: "production-password")

[[connections]]
name = "analytics"
type = "clickhouse"
host = "localhost"
port = 8123
user = "analyst"
database = "analytics"

[[connections]]
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"
# Token stored in keychain (key: "youtrack-token")

[[connections]]
name = "sentry"
type = "proxy"
url = "https://mcp.sentry.dev/mcp"
oauth = true

[[tunnels]]
name = "office-vpn"
peer_public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
peer_endpoint = "vpn.example.com:51820"
allowed_ips = "10.100.0.0/16"
tunnel_address = "10.100.0.42/32"
dns = "10.100.0.1"
# Private key stored in keychain (key: "tunnel-office-vpn-private-key")
```

See [docs/configuration.md](docs/configuration.md) for the full reference.

## Dual Mode

| Mode | Trigger | Use Case |
|------|---------|----------|
| **Desktop** | Run from terminal | Wails v3 GUI with connection management, OAuth flows, testing |
| **Stdio** | Piped stdin | Claude Desktop, Claude Code, any MCP client |
| **HTTP** | Desktop mode exposes `/mcp` | Claude Code, direct API access |

## Supported Connection Types

| Type | Category | Auth |
|------|----------|------|
| PostgreSQL | Database | Password |
| MariaDB | Database | Password |
| ClickHouse | Database | Password |
| MCP Proxy | Upstream MCP | Bearer token or OAuth 2.0 + PKCE |
| Microsoft Graph | API | Device code flow |
| Google Tag Manager | API | Service account |
| Firecrawl | API | API key |
| Brave Search | API | API key |
| OpenAI | API | API key |
| ElevenLabs | API | API key |
| HTTP | Generic API | Optional Bearer token |

## WireGuard Tunnels

mux can route database connections through userspace WireGuard tunnels to reach servers on private networks:

- No root/admin required -- runs entirely in userspace via gVisor netstack
- No system-wide VPN -- only mux traffic goes through the tunnel
- Fail-closed -- if a tunnel fails, connections using it are skipped entirely

See [docs/wireguard.md](docs/wireguard.md) for details.

## Documentation

| Document | Content |
|----------|---------|
| [docs/architecture.md](docs/architecture.md) | System overview, startup flow, key interfaces |
| [docs/configuration.md](docs/configuration.md) | Full TOML reference, keychain keys, env vars |
| [docs/erp-provisioning.md](docs/erp-provisioning.md) | ERP integration, assumptions, API contract |
| [docs/wireguard.md](docs/wireguard.md) | Tunnel architecture, fail-closed logic, dialer injection |
| [docs/deployment.md](docs/deployment.md) | Build variants, OS-specific notes, Docker, CI/CD |

## License

[MIT](LICENSE)
