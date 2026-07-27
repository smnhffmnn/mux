# Configuration Reference

## Config File Location

Default: `~/.mux/config.toml`

Override with `--config /path/to/config.toml`.

## Loading Priority

Configuration is loaded in layers (highest priority wins):

1. **Environment variables** -- override connection settings (host, port, user, etc.)
2. **TOML config file** -- non-sensitive settings (connections, tunnels, server config)
3. **Defaults** -- built-in fallback values

Secrets (passwords, tokens, keys) have their own resolution chain:

1. **Encrypted Vault** -- if enabled and unlocked
2. **OS Keychain** -- macOS Keychain, GNOME Keyring, KWallet, or Windows Credential Manager
3. **File fallback** -- `~/.mux/secrets.toml` (chmod 600, used when no keyring is available)

## Sections

### Server

```toml
[server]
port = 7700                        # HTTP port (default: 7700)
stdio_proxy = "auto"               # stdio behaviour next to a running instance (default: "auto")
tls_cert = "/path/to/cert.pem"    # enables HTTPS server (required for vault WebAuthn)
tls_key = "/path/to/key.pem"
tls_port = 7701                    # HTTPS port (default: port + 1)
```

The HTTP port serves the MCP endpoint (`/mcp`) on localhost. The HTTPS port (if configured) serves the vault UI, WebAuthn flows, and approval pages on all interfaces.

`stdio_proxy` controls what a stdio invocation does when another instance is already serving MCP on `port`:

| Value | Behaviour |
|-------|-----------|
| `auto` (default) | Bridge to the running instance; start standalone if there is none |
| `never` | Always start a full standalone instance |
| `always` | Refuse to start unless an instance is already serving |

Only one instance per machine should own the tunnels, the config file and the vault — see [Running more than one instance](#running-more-than-one-instance) below.

### Running more than one instance

Clients that spawn mux themselves (Claude Desktop, Cursor) start one process each. Without bridging, every one of those processes would build its own tunnels, its own secret view and its own in-memory copy of `config.toml`, which breaks in ways that are hard to attribute:

- **Tunnels fight over the peer.** Provisioned WireGuard credentials are issued per user, so every instance receives the *same* private key and tunnel address. Each opens its own UDP socket, and the server keeps one endpoint per peer — whichever instance sent traffic last owns the return path. With a persistent keepalive they trade it back and forth indefinitely, so calls fail intermittently and succeed on retry.
- **Config writes overwrite each other.** Each process holds `config.toml` in memory and writes it back whole, so the last writer silently discards the other's changes.
- **Secret views diverge.** A secret rotated in one process stays stale in the others until they restart.

With `stdio_proxy = "auto"`, a stdio invocation that finds an instance serving on `port` becomes a thin relay instead: it fetches no provisioning, starts no tunnels, opens no vault, registers no connections and never writes config. (It still reads the config file and the plain keychain/`secrets.toml` entries while loading config — it just does not act on them.) Tool names and the owning instance's MCP instructions pass through unchanged, so clients see an identical surface either way.

**What bridging does and does not cover.** Only an instance that serves HTTP can be bridged to, and a stdio instance does not open a listener. So bridging takes effect when a desktop or headless instance is running — which is the normal setup, since that is what serves `/mcp` for HTTP clients. If the *only* mux processes on the machine are stdio ones spawned by clients, they cannot find each other and each runs standalone, with the collisions above. Two instances on two different machines share the same provisioned credentials and collide the same way; that needs per-instance credentials on the provisioning side and is out of scope here.

Use `never` when an instance on the same port should deliberately keep its own tunnels, config view and vault instead of relaying. Note that an instance on a *different* port never bridges anyway — only the instance's own configured port is probed.

### Connections

Defined as `[[connections]]` entries. See [connections.md](connections.md) for all types, fields, and examples.

### Tunnels

Defined as `[[tunnels]]` entries. See [tunnels.md](tunnels.md) for fields and architecture.

### Remote Provisioning (`[provisioning]`)

The `[provisioning]` section configures remote provisioning — a centralized endpoint that delivers connection and tunnel definitions to mux instances.

```toml
[provisioning]
endpoint = "https://provisioning.example.com/api/mux/config"
# Token stored as secret (key: "provisioning-token")
```

See [provisioning.md](provisioning.md) for the full API contract.

### Vault (Encrypted Secret Store)

The vault encrypts all secrets at rest using AES-256-GCM with Argon2id key derivation. Secrets are only accessible in memory while the vault is unlocked, and automatically wiped after a configurable inactivity timeout.

```toml
[vault]
enabled = true
exclusive = true                   # all secrets in vault only, skip legacy keyring/file
inactivity_timeout = "30m"         # auto-lock after inactivity (default: 30m)
webauthn_rp_id = "mux.example.com"
webauthn_origins = ["https://mux.example.com:7701"]
base_url = "https://mux.example.com:7701"
```

When `exclusive = true`, secrets written via `secret_set` are stored only in the vault. When `false`, secrets are written to both vault and the legacy store (keychain/file) for compatibility.

The vault requires TLS for WebAuthn. See the [Vault section in the README](../README.md#vault-encrypted-secret-store) for setup, unlock methods, MCP tools, and HTTP API reference.

## Secrets

Secrets (passwords, tokens, keys) are resolved in order: **Vault → OS Keychain → File fallback**. The first match wins.

### OS Keychain

Secrets are stored in the platform's native credential store (service name: `"mux"`):

- **macOS**: Keychain Access
- **Linux**: GNOME Keyring / KWallet (via Secret Service API)
- **Windows**: Windows Credential Manager

On headless Linux without a desktop environment, the keychain is unavailable and mux automatically falls back to the file store. The encrypted vault is the recommended alternative for headless deployments.

### File Fallback (`~/.mux/secrets.toml`)

When the OS keychain is unavailable (headless Linux, SSH, containers), mux reads and writes secrets from `~/.mux/secrets.toml` (created with chmod 600):

```toml
[secrets]
"mydb-password" = "secret"
"provisioning-token" = "my-token"
"youtrack-token" = "bearer-token"
```

The vault is the recommended replacement for this plaintext fallback.

### Key Naming Conventions

| Key | Content |
|-----|---------|
| `{name}-password` | Database password |
| `{name}-token` | API key or Bearer token |
| `{name}-oauth-token` | OAuth token JSON blob |
| `{name}-oauth-client-id` | OAuth client ID |
| `{name}-oauth-client-secret` | OAuth client secret |
| `{name}-oauth-refresh-token` | OAuth refresh token (microsoft-graph) |
| `tunnel-{name}-private-key` | WireGuard private key (base64) |
| `tunnel-{name}-preshared-key` | WireGuard preshared key (base64) |
| `provisioning-token` | Remote provisioning bearer token |

Secrets can be set via:
1. The `secret_set` MCP tool (at runtime, via your MCP client)
2. The web UI (desktop mode, at `http://localhost:7700/ui`)

### Environment Variables

Environment variables override **connection settings** (host, port, user, etc.), not secrets. They are intended for containerized deployments where no keychain or vault is available.

Supported variables follow a legacy naming convention tied to fixed connection names:

| Variable | Connection | Field |
|----------|-----------|-------|
| `MUX_PROVISIONING_TOKEN` | — | Provisioning bearer token |
| `MUX_PROVISIONING_ENDPOINT` | — | Provisioning endpoint URL |
| `MARIADB_DB_HOST`, `_PORT`, `_USER`, `_PASSWORD`, `_NAME` | `mariadb` | Connection fields |
| `POSTGRESQL_DB_HOST`, `_PORT`, `_USER`, `_PASSWORD`, `_NAME` | `postgresql` | Connection fields |
| `CLICKHOUSE_HOST`, `_PORT`, `_USER`, `_PASSWORD`, `_SECURE` | `clickhouse` | Connection fields |

These variables create or update a connection with the exact name shown above (e.g., `POSTGRESQL_DB_HOST` applies to a connection named `"postgresql"`). For connections with custom names, use the TOML config file and the secret resolution chain instead.

