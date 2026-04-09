# Tunnels

## What Tunnels Enable

WireGuard tunnels allow mux to reach database servers on private networks without requiring a system-wide VPN. The tunnel runs entirely inside the mux process.

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

mux uses **wireguard-go** with **gVisor netstack** -- a complete WireGuard + TCP/IP stack in userspace:

- **No root/admin required** -- no real TUN/TAP device is created
- **No kernel module** -- everything runs as a regular Go goroutine
- **No system-wide VPN** -- only mux traffic goes through the tunnel
- **Cross-platform** -- works on macOS, Linux, and Windows without special drivers

The gVisor netstack provides a virtual TCP/IP stack. When mux connects to a database, the TCP connection routes through this virtual stack, gets encapsulated in WireGuard, and is sent as UDP to the peer endpoint.

## Configuration

### Via TOML

```toml
[[tunnels]]
name = "office-vpn"
peer_public_key = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
peer_endpoint = "vpn.example.com:51820"
allowed_ips = "10.100.0.0/16"
tunnel_address = "10.100.0.42/32"
dns = "10.100.0.1"            # optional, comma-separated
mtu = 1420                     # optional, default: 1420
keepalive = 25                 # optional, default: 25 seconds
# Private key: stored as secret (key: "tunnel-office-vpn-private-key")
# Preshared key: stored as secret (key: "tunnel-office-vpn-preshared-key"), optional
```

### Via Remote Provisioning

Tunnels can also come from the remote provisioning endpoint. See [provisioning.md](provisioning.md).

### Referencing Tunnels from Connections

```toml
[[connections]]
name = "production"
type = "postgresql"
host = "10.100.0.5"
port = 5432
user = "readonly"
database = "production"
tunnel = "office-vpn"    # references the tunnel above
```

When `tunnel` is set, the database connection routes through that WireGuard tunnel instead of the machine's default network.

## Tunnel Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique identifier, referenced by connections |
| `peer_public_key` | string | yes | WireGuard peer public key (base64) |
| `peer_endpoint` | string | yes | Peer endpoint (`host:port`). Hostnames are resolved to IP at startup. |
| `allowed_ips` | string | yes | Allowed IP ranges (CIDR). Comma-separated for multiple ranges. |
| `tunnel_address` | string | yes | Local tunnel IP (CIDR, e.g. `10.100.0.42/32`) |
| `dns` | string | no | DNS servers for resolution through the tunnel (comma-separated IPs) |
| `mtu` | integer | no | MTU (default: 1420) |
| `keepalive` | integer | no | Persistent keepalive interval in seconds (default: 25) |

**Secrets** (stored in OS keychain, not in TOML):
- `tunnel-{name}-private-key` -- WireGuard private key (base64, required)
- `tunnel-{name}-preshared-key` -- WireGuard preshared key (base64, optional)

A tunnel is considered enabled when it has a private key, peer public key, and peer endpoint.

## Fail-Closed Logic

If a tunnel fails to start (bad keys, unreachable peer, etc.), all connections that reference it are **skipped entirely**:

```
[wg] Tunnel "office-vpn" failed: handshake timeout
[mux] Skipping "production": tunnel "office-vpn" not available
[mux] Skipping "analytics": tunnel "office-vpn" not available
[mux] Registered: local-dev_query       <- no tunnel, works fine
```

This is intentional. A broken tunnel should not result in tools that always fail -- it is better to not expose them at all. Connections without a tunnel reference are unaffected.

## Key Format

WireGuard keys in config and provisioning responses are **base64-encoded** (standard WireGuard format, 44 characters with `=` padding). Each key decodes to exactly 32 bytes.

Generate a key pair with standard WireGuard tools:

```bash
wg genkey | tee private.key | wg pubkey > public.key
```

## Dialer Injection

Each database driver uses a different mechanism for custom network routing through the tunnel:

- **MariaDB** (`go-sql-driver/mysql`): Registers a custom network name per connection via `mysql.RegisterDialContext()`
- **PostgreSQL** (`lib/pq`): Uses `pq.NewConnector()` with a dialer adapter that implements `pq.Dialer`
- **ClickHouse** (`clickhouse-go`): Sets `DialContext` on the connection options struct
- **HTTP-based connections** (APIs, proxies): Sets `DialContext` on `http.Transport`

All drivers receive the tunnel's `DialContext` method, which routes TCP connections through the gVisor netstack.

## Lifecycle

1. mux starts and loads config (local + provisioned tunnels)
2. `Manager.Start()` iterates over all enabled tunnels:
   - Parses tunnel address and DNS
   - Creates a netstack virtual TUN device
   - Creates a WireGuard device and configures it via IPC
   - Brings the device up
3. During connection registration, `tunnel = "office-vpn"` causes a lookup in the manager; the tunnel is passed as a `Dialer` to the database constructor
4. On shutdown, `Manager.Close()` shuts down all WireGuard devices and releases resources

## Limitations

- **TCP only**: The netstack approach only supports TCP connections. DNS resolution through the tunnel works via the `dns` field, but arbitrary UDP is not supported.
- **No runtime changes**: Tunnel configuration changes require a mux restart. The provisioning sync re-fetches connections but does not restart tunnels.
- **No active health checks**: `IsUp()` reports whether the tunnel started successfully, not whether the peer is currently reachable.
- **Single peer per tunnel**: Each tunnel connects to exactly one WireGuard peer (standard client pattern).
