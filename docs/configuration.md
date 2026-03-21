# Configuration Reference

## Config File Location

Default: `~/.mux/config.toml`

Override with `--config /path/to/config.toml` or create it via the web UI.

## Loading Priority

Configuration is loaded in layers (highest priority wins):

1. **Environment variables** -- override everything
2. **OS Keychain** -- secrets (passwords, tokens, keys)
3. **TOML config file** -- non-sensitive settings
4. **Defaults** -- built-in fallback values

## TOML Reference

### Server

```toml
[server]
port = 7700          # HTTP port (default: 7700)
# auto_start = true  # Reserved for future use
```

### ERP Provisioning

```toml
[erp]
endpoint = "https://erp.example.com/api/mux/config"
# Token is stored in OS keychain (key: "erp-token"), not in TOML
```

See [ERP Provisioning](erp-provisioning.md) for details.

### Connections

Connections are defined as TOML array-of-tables. Any number of connections, any type.

#### PostgreSQL

```toml
[[connections]]
name = "production"
type = "postgresql"
host = "10.100.0.5"
port = 5432                              # default: 5432
user = "readonly_user"
database = "production"
read_only = true                         # optional, default: false
tunnel = "office-vpn"                    # optional, references a [[tunnels]] entry
instructions = "Production database."    # optional, added to MCP server instructions
# Password stored in keychain (key: "production-password")
```

**Tools registered**: `{name}_query`, `{name}_list_tables`, `{name}_describe_table`

#### ClickHouse

```toml
[[connections]]
name = "analytics"
type = "clickhouse"
host = "10.100.0.10"
port = 8123                 # default: 8123 (HTTP). Use 9000 for native protocol.
user = "analyst"
database = "default"        # default: "default"
secure = false              # optional, TLS for HTTP protocol
tunnel = "office-vpn"       # optional
instructions = "Analytics ClickHouse with marketing data."
# Password stored in keychain (key: "analytics-password")
```

**Port determines protocol**: 8123/8443 = HTTP, anything else = native TCP.

**Tools registered**: `{name}_query`, `{name}_list_databases`, `{name}_list_tables`, `{name}_describe_table`

#### MariaDB

```toml
[[connections]]
name = "local-dev"
type = "mariadb"
host = "localhost"
port = 3306                 # default: 3306
user = "root"
database = "myapp"
read_only = false           # optional
tunnel = ""                 # optional
# Password stored in keychain (key: "local-dev-password")
```

**Tools registered**: `{name}_query`, `{name}_execute`, `{name}_list_tables`, `{name}_describe_table`

Note: `execute` tool (write operations) is only available when `read_only = false`.

#### Proxy (MCP upstream)

Any MCP server reachable over HTTP can be proxied. mux connects to the upstream server, discovers its tools, and re-exposes them with a `{name}_` prefix. Authentication is handled via Bearer token or OAuth -- secrets are stored in the OS keychain, never in the config file.

```toml
# Bearer token proxy (e.g. YouTrack)
[[connections]]
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"
# Token stored in keychain (key: "youtrack-token")

# Bearer token proxy (e.g. Netdata Cloud)
[[connections]]
name = "netdata"
type = "proxy"
url = "https://app.netdata.cloud/api/v1/mcp"
# Token stored in keychain (key: "netdata-token")

# OAuth proxy (e.g. Sentry)
[[connections]]
name = "sentry"
type = "proxy"
url = "https://mcp.sentry.dev/mcp"
oauth = true                # enables OAuth 2.0 + PKCE flow
# OAuth tokens stored in keychain (key: "sentry-oauth-token")
```

**Tools registered**: `{name}_{upstream_tool}` -- all tools from the upstream server, prefixed.

### Tunnels

WireGuard tunnel definitions. Connections can reference tunnels by name.

```toml
[[tunnels]]
name = "office-vpn"
peer_public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
peer_endpoint = "vpn.example.com:51820"
allowed_ips = "10.100.0.0/16"              # comma-separated for multiple
tunnel_address = "10.100.0.42/32"
dns = "10.100.0.1"                         # optional, comma-separated
mtu = 1420                                 # optional, default: 1420
keepalive = 25                             # optional, default: 25 seconds
# Private key stored in keychain (key: "tunnel-office-vpn-private-key")
# Preshared key stored in keychain (key: "tunnel-office-vpn-preshared-key")
```

See [WireGuard](wireguard.md) for details.

## OS Keychain

Secrets are stored in the platform's native credential store:
- **macOS**: Keychain Access
- **Linux**: GNOME Keyring / KWallet (via Secret Service API)
- **Windows**: Windows Credential Manager

All keys use the service name `"mux"`.

### Key Naming Conventions

| Key | Content |
|-----|---------|
| `{connection-name}-password` | Database password |
| `{connection-name}-token` | Bearer token (proxy connections) |
| `{connection-name}-oauth-token` | OAuth token JSON blob |
| `{connection-name}-oauth-client-id` | OAuth client ID |
| `{connection-name}-oauth-client-secret` | OAuth client secret |
| `tunnel-{tunnel-name}-private-key` | WireGuard private key (base64) |
| `tunnel-{tunnel-name}-preshared-key` | WireGuard preshared key (base64) |
| `erp-token` | ERP provisioning bearer token |

Secrets can be set via:
1. The web UI (recommended)
2. Environment variables (override keychain)
3. Programmatically via `config.SaveSecret(key, value)`

## Environment Variables

### Server

| Variable | Description |
|----------|-------------|
| `MUX_PORT` | HTTP port (overrides `[server] port`) |

### ERP

| Variable | Description |
|----------|-------------|
| `MUX_ERP_ENDPOINT` | ERP provisioning URL |
| `MUX_ERP_TOKEN` | ERP bearer token |

### Database Connections (Legacy Format)

These create or update connections with fixed names for backward compatibility:

| Variable | Connection | Field |
|----------|-----------|-------|
| `MARIADB_DB_HOST` | mariadb | host |
| `MARIADB_DB_PORT` | mariadb | port |
| `MARIADB_DB_USER` | mariadb | user |
| `MARIADB_DB_PASSWORD` | mariadb | password |
| `MARIADB_DB_NAME` | mariadb | database |
| `CLICKHOUSE_HOST` | clickhouse | host |
| `CLICKHOUSE_PORT` | clickhouse | port |
| `CLICKHOUSE_USER` | clickhouse | user |
| `CLICKHOUSE_PASSWORD` | clickhouse | password |
| `CLICKHOUSE_SECURE` | clickhouse | secure |
| `POSTGRESQL_DB_HOST` | postgresql | host |
| `POSTGRESQL_DB_PORT` | postgresql | port |
| `POSTGRESQL_DB_USER` | postgresql | user |
| `POSTGRESQL_DB_PASSWORD` | postgresql | password |
| `POSTGRESQL_DB_NAME` | postgresql | database |

### Proxy Connections (Legacy Format)

| Variable | Connection | Field |
|----------|-----------|-------|
| `YOUTRACK_MCP_URL` | youtrack | url |
| `YOUTRACK_MCP_TOKEN` | youtrack | token |
| `SENTRY_MCP_URL` | sentry | url |
| `SENTRY_MCP_TOKEN` | sentry | token |

## Legacy Config Migration

Old-style configs with individual sections are automatically converted:

```toml
# Old format (still supported, auto-migrated):
[mariadb]
host = "localhost"
port = 3306
user = "root"
database = "mydb"

# Equivalent new format:
[[connections]]
name = "mariadb"
type = "mariadb"
host = "localhost"
port = 3306
user = "root"
database = "mydb"
```

Migration happens transparently during `config.Load()`. The old sections are read alongside the new `[[connections]]` format. On the next `Save()`, only the new format is written.

Migrated connection types:
- `[mariadb]` -> `Connection{Name: "mariadb", Type: "mariadb"}`
- `[clickhouse]` -> `Connection{Name: "clickhouse", Type: "clickhouse"}`
- `[postgresql]` -> `Connection{Name: "postgresql", Type: "postgresql"}`
- `[youtrack]` -> `Connection{Name: "youtrack", Type: "proxy"}`
- `[sentry]` -> `Connection{Name: "sentry", Type: "proxy", OAuth: true}`

## Default Values

| Setting | Default |
|---------|---------|
| Server port | 7700 |
| MariaDB port | 3306 |
| ClickHouse port | 8123 |
| PostgreSQL port | 5432 |
| ClickHouse default database | "default" |
| WireGuard MTU | 1420 |
| WireGuard keepalive | 25 seconds |
