# Architecture

## Overview

mux is a single Go binary that acts as a unified MCP (Model Context Protocol) gateway. It connects AI agents to databases, upstream MCP servers, and private networks -- all through one endpoint.

```
                        +---------------------------------------------+
                        |                  mux Binary                  |
                        |                                             |
  Claude Desktop ------>|  stdio --+                                  |
                        |          |    +--------------+              |
  Claude Code --------->|  /mcp ---+---->  MCP Server  |              |
                        |          |    +------+-------+              |
  Browser ------------->|  /ui ----+           |                      |
                        |          |    +------+-------+              |
                        |          |    |  Tool Router  |              |
                        |          |    +--+---+---+---+              |
                        |          |       |   |   |                  |
                        |     +----+--+ +--+-+ | +-+----+            |
                        |     |Native | |Prox| | | Date  |            |
                        |     |  DB   | | y  | | | Time  |            |
                        |     +---+---+ +--+-+ | +------+            |
                        |         |        |   |                      |
                        |    +----+----+   |   |                      |
                        |    |WireGuard|   |   |                      |
                        |    | Tunnel  |   |   |                      |
                        |    +----+----+   |   |                      |
                        +---------+--------+---+----------------------+
                                  |        |   |
                          +-------+        |   |
                          v                v   v
                   +----------+    +--------------+
                   | Private  |    |   Upstream    |
                   | Database |    |  MCP Servers  |
                   | Servers  |    |(YouTrack etc.)|
                   +----------+    +--------------+
```

## Startup Flow

```
1. Parse CLI flags (--stdio, --http, --port, --config, --no-tray, --no-ui)

2. Load configuration
   +-- Read TOML file (~/.mux/config.toml)
   +-- Migrate legacy format ([mariadb] -> [[connections]])
   +-- Load secrets from OS keychain
   +-- Apply environment variable overrides
   +-- Apply defaults (ports, MTU, keepalive)

3. ERP provisioning (if configured)
   +-- GET {endpoint} with Bearer token
   +-- Parse JSON response (tunnels + connections)
   +-- Mark all items as source="erp"
   +-- Merge with local config (ERP tunnels shadow local on name collision)

4. Start WireGuard tunnels
   +-- For each tunnel config: create netstack TUN + WG device
   +-- Failed tunnels are logged and skipped
   +-- Result: map[name] -> *WGTunnel (or nil if failed)

5. Register MCP tools
   +-- For each connection:
   |   +-- If tunnel referenced but unavailable -> SKIP (fail-closed)
   |   +-- If type=proxy -> register via proxy package (with namespacing)
   |   +-- If type=db -> create driver with optional tunnel dialer
   |       +-- Register tools as {connection_name}_{tool_name}
   +-- Always register get_datetime

6. Start transport
   +-- If stdin is pipe -> stdio mode (ServeStdio)
   +-- If terminal -> HTTP mode
       +-- /mcp   -> MCP Streamable HTTP
       +-- /ui/   -> Embedded web UI + REST API
       +-- System tray (if available and not disabled)
```

## Package Dependencies

```
main.go
  +-- internal/config      (Config, Connection, TunnelConfig, ERPConfig)
  +-- internal/erp         (Fetch provisioning data)
  +-- internal/wireguard   (Tunnel manager + WG tunnels)
  +-- internal/tools       (DB tool registration + Dialer interface)
  +-- internal/proxy       (Upstream MCP proxy)
  +-- internal/ui          (Web UI + REST API)
  +-- internal/tray        (System tray icon)

internal/tools
  +-- internal/config      (Connection type)
  +-- mcp-go               (Tool definitions)

internal/wireguard
  +-- internal/config      (TunnelConfig type)
  +-- wireguard-go         (WireGuard protocol)
  +-- gvisor netstack      (Userspace TCP/IP)

internal/ui
  +-- internal/config      (Config access)
  +-- internal/erp         (ERP sync)
  +-- internal/tools       (Test connections, ClickHouse helper)
  +-- internal/proxy       (OAuth proxy mounting)
  +-- internal/wireguard   (Tunnel status)

internal/proxy
  +-- internal/config      (KeychainTokenStore)
  +-- mcp-go               (Client + transport)

internal/erp
  +-- internal/config      (Connection, TunnelConfig types)
```

## Key Interfaces

### Dialer (`internal/tools/tools.go`)

```go
type Dialer interface {
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
```

Used by all database constructors. When nil, the driver uses its default TCP dialer. When a WireGuard tunnel is provided, it routes connections through the tunnel. Each database driver injects the dialer differently:

| Driver | Injection Method |
|--------|-----------------|
| PostgreSQL (lib/pq) | `pq.NewConnector()` + `connector.Dialer()` via adapter |
| ClickHouse | `clickhouse.Options.DialContext` field |
| MariaDB (go-sql-driver) | `mysql.RegisterDialContext()` with unique network name |

### ToolDef (`internal/tools/tools.go`)

```go
type ToolDef struct {
    Tool    mcp.Tool
    Handler server.ToolHandlerFunc
}
```

Each database type returns `[]ToolDef` from its `Tools()` method. `RegisterConnection()` prefixes tool names with the connection name and registers them on the MCP server.

### Mount (`internal/proxy/proxy.go`)

```go
type Mount struct {
    Name       string
    URL        string
    Headers    map[string]string          // static auth (bearer token)
    OAuth      *transport.OAuthConfig     // OAuth 2.0 + PKCE
    TokenStore *config.KeychainTokenStore
}
```

Proxied upstream MCP servers. Each upstream tool is re-exported as `{mount_name}_{upstream_tool_name}`.

## Tool Namespacing

Every tool registered on the MCP server is prefixed with its connection name:

```
Connection "production" (PostgreSQL):
  -> production_query
  -> production_list_tables
  -> production_describe_table

Connection "analytics" (ClickHouse):
  -> analytics_query
  -> analytics_list_databases
  -> analytics_list_tables
  -> analytics_describe_table

Connection "youtrack" (Proxy):
  -> youtrack_get_issue
  -> youtrack_search_issues
  -> youtrack_create_issue
  -> ... (all upstream tools)
```

This prevents name collisions when multiple connections of the same type exist.

## Fail-Closed Tunnel Logic

```
Connection "production" references tunnel "office-vpn"

IF tunnel "office-vpn" started successfully:
  -> production tools registered with tunnel dialer
  -> Queries route through WireGuard

IF tunnel "office-vpn" failed to start:
  -> production tools NOT registered
  -> Log: "[mux] Skipping 'production': tunnel 'office-vpn' not available"
  -> Other connections without this tunnel are unaffected
```

This prevents broken tool registrations that would fail on every call.

## Config Merge (ERP + Local)

```
Local config:                    ERP response:
  tunnels:                         tunnels:
    - office-vpn                     - office-vpn (different config)
  connections:                     connections:
    - local-dev (postgresql)         - production (postgresql)
    - youtrack (proxy)               - analytics (clickhouse)

After merge:
  AllTunnels():
    - office-vpn (ERP version wins)
  AllConnections():
    - production (erp)
    - analytics (erp)
    - local-dev (local)
    - youtrack (local)
```

ERP tunnels shadow local tunnels with the same name. Connections are simply concatenated (ERP first, then local).

## Dual Transport

mux auto-detects its transport mode:

| Condition | Mode | Use Case |
|-----------|------|----------|
| stdin is a pipe | stdio | Claude Desktop, piped agents |
| stdin is a terminal | HTTP | Web UI, Claude Code, direct API |
| `--stdio` flag | stdio | Force stdio |
| `--http` flag | HTTP | Force HTTP |

In HTTP mode:
- `/mcp` -- MCP Streamable HTTP endpoint
- `/ui/` -- Embedded web UI (SPA)
- `/` -- Redirects to `/ui/`
