# mux

Single-binary MCP gateway for databases, APIs, and tunnels.

## What is mux?

mux is a [Model Context Protocol](https://modelcontextprotocol.io) gateway that gives AI agents unified access to databases, upstream MCP servers, and API services through one endpoint. It connects to private networks via built-in WireGuard tunnels (no root required), runs as a desktop app with system tray or headless for servers and CI, and stores secrets in your OS keychain. No Docker, no Python, no runtime dependencies.

## Features

- **Databases** -- MariaDB, PostgreSQL, ClickHouse with schema introspection and query tools
- **MCP Proxy** -- forward tool calls to upstream MCP servers (YouTrack, Sentry, Notion, Netdata, etc.)
- **API Integrations** -- Microsoft Graph (mail + SharePoint), Google Tag Manager, OpenAI, ElevenLabs, Brave Search, Firecrawl
- **Generic HTTP** -- connect any REST API with optional Bearer auth
- **WireGuard Tunnels** -- reach databases on private networks through userspace WireGuard (no root, no VPN client)
- **Remote Provisioning** -- centrally manage connections and tunnels for your team from a single HTTP endpoint
- **Dual Transport** -- stdio (Claude Desktop) and Streamable HTTP (`/mcp`) with auto-detection
- **Desktop App** -- Wails v3 + Svelte 5 with system tray, web UI, OAuth flows, and connection testing
- **OS Keychain** -- secrets stored in macOS Keychain, GNOME Keyring, or Windows Credential Manager
- **Dynamic Config** -- add, remove, and manage connections at runtime via MCP tools
- **Cross-Platform** -- macOS, Linux, Windows (amd64 + arm64)

## Installation

### Homebrew (macOS)

```bash
brew tap smnhffmnn/tap
brew install mux
```

### GitHub Releases

Download the latest binary from [Releases](https://github.com/smnhffmnn/mux/releases).

### Build from Source

```bash
# Headless (all platforms, no CGO required)
go install github.com/smnhffmnn/mux@latest

# Desktop (macOS, requires CGO + Wails v3)
git clone https://github.com/smnhffmnn/mux.git
cd mux && make build
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
# Store password: run mux, then use the secret_set tool or web UI
```

Run mux:

```bash
mux
```

mux auto-detects the mode: terminal = desktop app + HTTP server on port 7700, piped stdin = stdio mode.

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

```bash
# HTTP server (for Claude Code, API access)
mux --http

# Stdio (for Claude Desktop, piped agents)
mux --stdio

# Custom port and config
mux --http --port 8080 --config ./my-config.toml
```

## Configuration

Config file: `~/.mux/config.toml`

Priority (highest wins): **Environment variables > OS Keychain > TOML file > Defaults**

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
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"
# Token stored in keychain (key: "youtrack-token")

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
