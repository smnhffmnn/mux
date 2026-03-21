# WireGuard Tunnels

## What Tunnels Enable

WireGuard tunnels allow mux to reach database servers on private networks -- without requiring the user's machine to have a system-wide VPN connection. The tunnel runs entirely inside the mux process.

```
User's Machine                    Private Network
+------------------+              +------------------+
|                  |              |                  |
|  mux Process     |   WireGuard  |  10.100.0.5:5432 |
|  +------------+  |   UDP tunnel |  (PostgreSQL)    |
|  | PostgreSQL +--+---->---------+-->               |
|  |   Driver   |  |              |                  |
|  +-----+------+  |              |  10.100.0.10:9000|
|        |         |              |  (ClickHouse)    |
|  +-----+------+  |              |                  |
|  | WireGuard  |  |              +------------------+
|  | (netstack) |  |                      ^
|  +-----+------+  |                      |
|        | UDP     |              +-------+----------+
|        +---------+------------->| WireGuard Server |
|                  |   Internet   | vpn.example.com  |
+------------------+              | :51820           |
                                  +------------------+
```

## Architecture: Userspace WireGuard

mux uses **wireguard-go** with **gVisor netstack** -- a complete WireGuard + TCP/IP stack running in userspace:

- **No root/admin required** -- no real TUN/TAP device is created
- **No kernel module** -- everything runs as a regular Go goroutine
- **No system-wide VPN** -- only mux traffic goes through the tunnel
- **Cross-platform** -- works on macOS, Linux, and Windows without special drivers

The gVisor netstack provides a virtual TCP/IP stack. When mux connects to a database, the TCP connection is routed through this virtual stack, encapsulated in WireGuard, and sent as UDP to the peer endpoint.

### Key Libraries

| Import | Purpose |
|--------|---------|
| `golang.zx2c4.com/wireguard/device` | WireGuard protocol engine |
| `golang.zx2c4.com/wireguard/conn` | UDP socket binding |
| `golang.zx2c4.com/wireguard/tun/netstack` | Virtual TUN + TCP/IP stack |

## Configuration

### Via TOML

```toml
[[tunnels]]
name = "office-vpn"
peer_public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
peer_endpoint = "vpn.example.com:51820"
allowed_ips = "10.100.0.0/16"
tunnel_address = "10.100.0.42/32"
dns = "10.100.0.1"            # optional
mtu = 1420                     # optional, default: 1420
keepalive = 25                 # optional, default: 25 seconds
# Private key: stored in keychain (key: "tunnel-office-vpn-private-key")
# Preshared key: stored in keychain (key: "tunnel-office-vpn-preshared-key")
```

### Via ERP

Tunnels can also come from the ERP provisioning endpoint. See [ERP Provisioning](erp-provisioning.md).

### Referencing Tunnels from Connections

```toml
[[connections]]
name = "production"
type = "postgresql"
host = "10.100.0.5"
port = 5432
user = "readonly"
database = "production"
tunnel = "office-vpn"    # <- references the tunnel above
```

When `tunnel` is set, the database connection is routed through that WireGuard tunnel instead of the machine's default network.

## Fail-Closed Logic

If a tunnel fails to start (bad keys, unreachable peer, etc.), all connections that reference it are **skipped**:

```
[wg] Tunnel "office-vpn" failed: handshake timeout
[mux] Skipping "production": tunnel "office-vpn" not available
[mux] Skipping "analytics": tunnel "office-vpn" not available
[mux] Registered: local-dev_query       <- no tunnel, works fine
```

This is intentional. A broken tunnel should not result in tools that always fail -- it's better to not expose them at all.

Connections without a tunnel reference are completely unaffected.

## Dialer Injection

Each database driver has a different mechanism for custom network routing:

### PostgreSQL (lib/pq)

Uses `pq.NewConnector()` with a dialer adapter:

```go
connector, _ := pq.NewConnector(dsn)
connector.Dialer(&pqDialerAdapter{dialer})  // adapts Dialer to pq.Dialer interface
db := sql.OpenDB(connector)
```

The `pqDialerAdapter` wraps our `Dialer` interface to implement `pq.Dialer` (which has `Dial` and `DialTimeout` methods).

### ClickHouse (clickhouse-go)

Sets `DialContext` on the options struct:

```go
opts.DialContext = func(ctx context.Context, addr string) (net.Conn, error) {
    return dialer.DialContext(ctx, "tcp", addr)
}
```

### MariaDB (go-sql-driver/mysql)

Registers a custom network name globally:

```go
networkName := fmt.Sprintf("wg-mux-%d", counter.Add(1))
mysql.RegisterDialContext(networkName, func(ctx context.Context, addr string) (net.Conn, error) {
    return dialer.DialContext(ctx, "tcp", addr)
})
// DSN uses custom network: "user:pass@wg-mux-1(host:port)/db"
```

Each MariaDB connection gets a unique network name (wg-mux-1, wg-mux-2, ...) because `mysql.RegisterDialContext` is a global registry.

## Key Format

WireGuard keys in mux config and ERP responses are **base64-encoded** (standard WireGuard format):

```
xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=
```

Internally, wireguard-go's IPC protocol requires **hex-encoded** keys. mux converts automatically:

```go
// base64 -> 32 bytes -> hex (64 chars)
func keyToHex(b64 string) (string, error) {
    raw, _ := base64.StdEncoding.DecodeString(b64)  // 32 bytes
    return hex.EncodeToString(raw), nil              // 64 hex chars
}
```

## Tunnel Lifecycle

```
1. mux startup
2. config.AllTunnels() returns local + ERP tunnels
3. wireguard.Manager.Start() iterates:
   a. Parse tunnel address, DNS, MTU
   b. Create netstack virtual TUN device
   c. Create WireGuard device
   d. Configure via IPC (keys, endpoint, allowed IPs, keepalive)
   e. Bring device up
   f. Store in manager map: tunnels["office-vpn"] = *WGTunnel
4. Connection registration:
   a. conn.Tunnel = "office-vpn" -> look up in manager
   b. Pass *WGTunnel as Dialer to DB constructor
5. mux shutdown:
   a. manager.Close() -> calls dev.Close() on all tunnels
   b. All WireGuard goroutines stop, UDP sockets close
```

## Limitations

- **TCP only**: The netstack approach only supports TCP connections through the tunnel. UDP services (e.g., DNS resolution via tunnel) work through the configured DNS field, but arbitrary UDP is not supported.
- **No tunnel changes at runtime**: Tunnel config changes require mux restart. The `/api/erp/sync` endpoint re-fetches connections but does not restart tunnels.
- **No health checks**: There's currently no active health monitoring of tunnels. The `IsUp()` check only reports whether the tunnel was successfully started, not whether the peer is currently reachable.
- **Single peer per tunnel**: Each tunnel connects to exactly one WireGuard peer. This is the standard WireGuard client pattern.

## Code Locations

| File | Content |
|------|---------|
| `internal/wireguard/tunnel.go` | `WGTunnel` type, `New()` constructor, `DialContext()`, IPC config building |
| `internal/wireguard/manager.go` | `Manager` type, `Start()`, `Get()`, `IsUp()`, `Close()` |
| `internal/config/config.go` | `TunnelConfig` type, keychain loading for tunnel keys |
| `main.go:98-108` | Tunnel startup + fail-closed connection registration |
| `internal/tools/tools.go` | `Dialer` interface definition |
