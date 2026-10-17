# Tunnels

mux supports two tunnel types: **WireGuard** (userspace, built-in) and **SSH** (built-in client, port forwarding over an SSH connection). Both allow connections to reach servers on private networks without a system-wide VPN.

## WireGuard Tunnels

### What WireGuard Tunnels Enable

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
| `peer_endpoint` | string | yes | Peer endpoint (`host:port`). Hostnames are resolved to all their addresses, with runtime failover — see [Endpoint Resolution and Failover](#endpoint-resolution-and-failover). |
| `allowed_ips` | string | yes | Allowed IP ranges (CIDR). Comma-separated for multiple ranges. |
| `tunnel_address` | string | yes | Local tunnel IP (CIDR, e.g. `10.100.0.42/32`) |
| `dns` | string | no | DNS servers for resolution through the tunnel (comma-separated IPs) |
| `mtu` | integer | no | MTU (default: 1420) |
| `keepalive` | integer | no | Persistent keepalive interval in seconds (default: 25). Keep it above the ~15s failover window — shorter intervals can make a healthy idle tunnel look like failing traffic (mux logs a warning). |

**Secrets** (stored in vault, OS keychain, or secrets.toml — never in config.toml):
- `tunnel-{name}-private-key` -- WireGuard private key (base64, required)
- `tunnel-{name}-preshared-key` -- WireGuard preshared key (base64, optional)

A tunnel is considered enabled when it has a private key, peer public key, and peer endpoint.

## Endpoint Resolution and Failover

A hostname `peer_endpoint` is resolved to **all** of its addresses at startup, and the tunnel starts on the preferred one: IPv6 leads only when the local stack actually has a route to it — an AAAA record alone proves nothing about the current network (a v4-only home network still receives AAAA answers, and blindly preferring them would send every handshake to an unreachable address). The resolved candidates are logged at startup (`endpoint ... resolved to ...`).

The remaining addresses stay available as failover targets, via two complementary signals:

- **Failing traffic:** packets go out, nothing comes back, no handshake completes for ~15 seconds — the peer switches to the next routable candidate, re-resolving DNS after the list wraps around. Each switch is logged (`traffic failing via ... — switching endpoint to ...`), and a failing endpoint without an alternative is logged too.
- **Lost route:** failed sends produce no traffic signal at all (the OS rejects them locally), so when the current address has no local route anymore — roaming from a dual-stack into a v4-only network — the tunnel switches to a routable candidate directly (`no route to ... — switching endpoint to ...`).

An idle tunnel on a reachable address is never rotated (WireGuard handshakes lazily, so silence is not failure — with the default 25s keepalive this holds; see the `keepalive` field note).

## Fail-Closed Logic

If a tunnel fails to start (bad keys, unreachable peer, etc.), all connections that reference it are **skipped entirely**:

```
[wg] Tunnel "office-vpn" failed: handshake timeout
[mux] Skipping "production": tunnel "office-vpn" not available
[mux] Skipping "analytics": tunnel "office-vpn" not available
[mux] Registered: local-dev_query       <- no tunnel, works fine
```

This is intentional. A broken tunnel should not result in tools that always fail -- it is better to not expose them at all. Connections without a tunnel reference are unaffected.

Connection tests (the **Test** button in the desktop app) fail closed the same way: testing a connection whose tunnel is down reports `tunnel "office-vpn" not available` instead of connecting directly past the tunnel. A test only reports success when it reached the target the way the live tools would.

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
2. `Manager.Start()` iterates over all enabled tunnels (skipped entirely when a stdio invocation bridges to an already-running instance — it uses that instance's tunnels):
   - Parses tunnel address and DNS
   - Creates a netstack virtual TUN device
   - Creates a WireGuard device and configures it via IPC
   - Brings the device up
3. During connection registration, `tunnel = "office-vpn"` causes a lookup in the manager; the tunnel is passed as a `Dialer` to the database constructor
4. On shutdown, `Manager.Close()` shuts down all WireGuard devices and releases resources

## Limitations

- **TCP only**: The netstack approach only supports TCP connections. DNS resolution through the tunnel works via the `dns` field, but arbitrary UDP is not supported.
- **No runtime changes to existing tunnels**: changing a tunnel's configuration requires a mux restart. The provisioning sync starts tunnels that are new since startup but does not restart existing ones.
- **No active health checks for WireGuard**: `IsUp()` reports whether the tunnel started successfully, not whether the peer is currently reachable. Failing traffic triggers endpoint failover (see above) but is not reflected in `IsUp()`. SSH tunnels differ: a keepalive every 30 seconds marks a silent connection dead (`IsUp()` false, `/health` degraded) until the next use reconnects.
- **Single peer per tunnel**: Each tunnel connects to exactly one WireGuard peer (standard client pattern).
- **One owner per machine**: Provisioned credentials are issued per user, so two mux instances present the *same* WireGuard key. The server keeps one endpoint per peer, so whichever instance sent traffic last owns the return path — and with a persistent keepalive they take it from each other continuously. The symptom is calls that fail intermittently and succeed on retry. Only one instance should own the tunnels; stdio invocations bridge to it by default, see [Running more than one instance](configuration.md#running-more-than-one-instance). Two instances on two different machines hit this too and bridging cannot help there — that needs per-instance credentials from the provisioning side.

## SSH Tunnels

SSH tunnels provide TCP forwarding through an SSH connection (`direct-tcpip` channels). The SSH client is built in (`golang.org/x/crypto/ssh`, shared with the [ssh connection type](connections.md#ssh)); no OpenSSH client is involved.

### Configuration

```toml
[[tunnels]]
name = "bastion"
type = "ssh"
host = "bastion.example.com"
port = 22
user = "deploy"
# auth = "key"                        # default; "agent" uses the user's ssh-agent instead of a stored key
# key_file = "~/.ssh/id_ed25519"      # read the key from a file instead of the secret store
# insecure_host_key = false           # default: false (fail-closed, verify known_hosts)
# Private key stored as secret (key: "tunnel-bastion-private-key"); if it is encrypted,
# its passphrase as "tunnel-bastion-private-key-passphrase"
```

### SSH Tunnel Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique identifier, referenced by connections |
| `type` | string | yes | Must be `"ssh"` |
| `host` | string | yes | SSH server hostname or IP |
| `port` | integer | no | SSH server port (default: 22) |
| `user` | string | yes | SSH username |
| `auth` | string | no | `key` (default): a private key mux holds — the secret or `key_file`. `agent`: the ssh-agent of the user running mux (`SSH_AUTH_SOCK`); no key in mux, not available in containers. |
| `key_file` | string | no | Path to SSH private key file (alternative to the stored key) |
| `insecure_host_key` | boolean | no | Skip host key verification (default: false). **Not recommended for production.** |

**Secrets**:
- `tunnel-{name}-private-key` -- SSH private key (PEM, OpenSSH or PKCS format; may be passphrase-protected). Used when `key_file` is not set.
- `tunnel-{name}-private-key-passphrase` -- passphrase of an encrypted private key. mux decrypts the key in memory only; the key can stay as its owner has it. Ignored for an unencrypted key.

### Host Key Verification

SSH tunnels use **fail-closed** host key verification by default: the host must be in `known_hosts` in the mux config directory (`~/.config/mux/known_hosts`, the file provisioning and containers can manage) or in `~/.ssh/known_hosts`. The files are re-read at every connection attempt; lines the parser cannot read are skipped with a log line, as OpenSSH does — one stray line must not take every SSH connection down. An unknown host fails with the exact `known_hosts` line to add; a host whose key changed fails as a mismatch. The key types a file knows for the host are offered first in the handshake, as OpenSSH does. Set `insecure_host_key = true` only for development or when known_hosts management is not feasible; a provisioned tunnel ignores the flag.

### Auto-Reconnect and Keepalive

SSH tunnels automatically reconnect on connection loss and send keepalive packets to prevent idle disconnects. The tunnel provides a `DialContext` method (same interface as WireGuard tunnels), so connections reference SSH tunnels identically:

```toml
[[connections]]
name = "remote-db"
type = "mariadb"
host = "10.0.0.5"
port = 3306
user = "app"
tunnel = "bastion"     # same syntax as WireGuard tunnels
```
