# Configuration Reference

## Config File Location

Default: `~/.mux/config.toml`

Override with `--config /path/to/config.toml`.

## Loading Priority

Configuration is loaded in layers (highest priority wins):

1. **Environment variables** -- override everything
2. **OS Keychain** -- secrets (passwords, tokens, keys)
3. **TOML config file** -- non-sensitive settings
4. **Defaults** -- built-in fallback values

## Sections

### Server

```toml
[server]
port = 7700          # HTTP port (default: 7700)
```

### Connections

Defined as `[[connections]]` entries. See [connections.md](connections.md) for all types, fields, and examples.

### Tunnels

Defined as `[[tunnels]]` entries. See [tunnels.md](tunnels.md) for fields and architecture.

### Remote Provisioning

```toml
[erp]
endpoint = "https://provisioning.example.com/api/mux/config"
# Token stored in OS keychain (key: "provisioning-token"), not in TOML
```

See [provisioning.md](provisioning.md) for the full API contract.

## OS Keychain

Secrets are stored in the platform's native credential store (service name: `"mux"`):

- **macOS**: Keychain Access
- **Linux**: GNOME Keyring / KWallet (via Secret Service API)
- **Windows**: Windows Credential Manager

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
1. The web UI (recommended)
2. The `secret_set` MCP tool (at runtime)
3. Environment variables (override keychain)

