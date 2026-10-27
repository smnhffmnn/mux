# Remote Provisioning

## What It Is

Remote provisioning lets you configure mux instances from a central HTTP endpoint. Instead of manually editing `config.toml` on every developer's machine, you serve the configuration as JSON from one place. mux fetches it on startup (and on-demand via the web UI) and merges it with the local config.

This is useful for teams: set up one provisioning server, give each developer a token, and all their mux instances get the same database connections and WireGuard tunnels automatically.

## How mux Uses It

1. On startup, mux checks if both `endpoint` and `token` are configured (via TOML, secret store, or environment variables)
2. If yes, it sends a GET request to the endpoint with the token as a Bearer header
3. The JSON response is parsed into connections and tunnels
4. These are merged with local config (provisioned items are marked with `source: "provisioning"`)
5. Connections and tunnels from the provisioning server are available alongside local ones

Provisioned data is fetched fresh on every startup and is not cached between restarts.

A stdio invocation that bridges to an already-running instance does not fetch provisioning itself — it relays to the owning instance's connections and tunnels. See [Running more than one instance](configuration.md#running-more-than-one-instance).

## Configuration

### Via TOML

```toml
[provisioning]
endpoint = "https://provisioning.example.com/api/mux/config"
# Token stored as secret (key: "provisioning-token")
```

### Via Environment Variables

```bash
export MUX_PROVISIONING_ENDPOINT="https://provisioning.example.com/api/mux/config"
export MUX_PROVISIONING_TOKEN="your-provisioning-token"
export MUX_INSTANCE_ID="office-pc-container"   # optional, see "Instance identity" below
```

### Via Web UI

Open the mux web UI, navigate to the Provisioning section, enter the endpoint URL and token, and click Save. The token is stored in the secret store (vault, keychain, or file). Click "Sync" to fetch immediately.

## API Contract

### Request

```
GET {endpoint}
Authorization: Bearer {token}
Accept: application/json
X-Mux-Instance: {instance id}      (only when the installation has an identity, see below)
X-Mux-Hostname: {host name}        (with X-Mux-Instance, when the host name is known; informational)
```

mux sends this exact request. The endpoint must respond within 15 seconds.

### Instance identity

The bearer token identifies the *user*. When one user runs mux on two machines (a laptop and an office container, say), a server that keys tunnel credentials on the user hands both the same WireGuard private key — and a WireGuard server keeps one endpoint per key, so the two installations steal each other's return path for as long as both are up. The instance identity is the missing dimension: a stable name per installation that the server can key its own tunnel credentials on, one peer per instance.

- On first use mux generates the ID — the machine's host name as a slug plus a random suffix, e.g. `macbook-pro-3f9a1c` — and persists it in the file `instance` inside the config directory (`~/.config/mux/instance`). It is only a name, never key material, and it never changes afterwards.
- `MUX_INSTANCE_ID` overrides it — for containers, whose config directory only survives a rebuild when it is mounted, and for several instances on one machine that are meant to stay separate (see below). The value is alphanumeric segments joined by single `-` or `_`, at most 64 characters; upper case is folded to lower case.
- An ID that cannot be persisted (unwritable config directory, invalid value) is not sent at all: a fresh ID per start would register a new instance per start. mux logs why, fetches without identity and tries again on the next fetch.

What the server does with it is the server's business. A server that keys credentials per user ignores the headers; nothing changes. A server that supports per-instance credentials registers the installation on its first fetch and from then on delivers tunnel credentials that belong to this installation only — the response has the same shape as before, so mux needs no further configuration. Token-based connections stay user-scoped either way; mux still generates no key material locally.

Two standalone instances on the *same* machine read the same `instance` file and therefore present the same identity — for client-spawned processes that case is what [bridging](configuration.md#running-more-than-one-instance) is for. Instances that are meant to stay separate on one machine (own port, own `--config`) share the config directory and with it the identity: give all but one a distinct `MUX_INSTANCE_ID`.

Redirects are **not followed** -- a 3xx response is treated as an authentication error (common when an auth gateway intercepts expired tokens).

### Response

**HTTP 200** with JSON body:

```json
{
  "connections": [
    {
      "name": "production",
      "type": "postgresql",
      "host": "10.100.0.5",
      "port": 5432,
      "user": "readonly",
      "password": "secret-from-server",
      "database": "production",
      "readOnly": true,
      "tunnel": "office-vpn",
      "instructions": "Production PostgreSQL. Read-only."
    }
  ],
  "tunnels": [
    {
      "name": "office-vpn",
      "peerPublicKey": "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=",
      "peerEndpoint": "vpn.example.com:51820",
      "allowedIPs": "10.100.0.0/16",
      "tunnelAddress": "10.100.0.42/32",
      "dns": "10.100.0.1",
      "privateKey": "base64-encoded-private-key",
      "presharedKey": "base64-encoded-preshared-key",
      "mtu": 1420,
      "keepalive": 25
    }
  ]
}
```

### Error Responses

| Status | mux Behavior |
|--------|-------------|
| 200 | Parse JSON, merge into config |
| 301-399 | Treat as auth error ("token is likely expired or invalid") |
| 401, 403 | Auth error ("check your token") |
| Other 4xx/5xx | Log error with status code and response body (up to 1 MB) |
| Timeout (>15s) | Log error, continue with local config |
| HTML body | Treat as auth error (common with SSO gateways intercepting requests) |

On any error during startup, mux logs a warning and continues with local config only. On error during a UI sync, mux returns HTTP 502 to the UI.

## JSON Schema

### Connection Object

All fields use **camelCase** JSON keys.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique connection identifier |
| `type` | string | yes | One of the connection types listed in [connections.md](connections.md). The canonical list lives in `internal/config/identifiers.go`. |
| `host` | string | databases | Database hostname or IP |
| `port` | integer | no | Port number (defaults applied per type: MariaDB 3306, PostgreSQL 5432, ClickHouse 8123) |
| `user` | string | databases | Database username |
| `password` | string | no | Database password. If omitted, mux checks the secret store for `{name}-password`. |
| `database` | string | no | Database name (ClickHouse defaults to `"default"`) |
| `readOnly` | boolean | no | Restrict to read-only queries (default: false) |
| `secure` | boolean | no | TLS for ClickHouse HTTP (default: false) |
| `url` | string | proxy/http/api | Endpoint URL |
| `token` | string | no | API key or Bearer token. If omitted, mux checks the secret store for `{name}-token`. |
| `oauth` | boolean | no | Use OAuth 2.0 + PKCE for proxy connections (default: false). The `sentry`, `notion`, `asana-mcp`, and `higgsfield` types force `oauth: true` regardless of the JSON value — these proxy types only support OAuth. |
| `scopes` | string | no | OAuth scopes. Used by `microsoft-graph` and OAuth proxy types (e.g. `higgsfield` defaults to `openid email offline_access`). |
| `tunnel` | string | no | Name of a tunnel to route through |
| `instructions` | string | no | Instructions for AI agents describing the connection |
| `monthlyLimit` | integer | no | Monthly request limit (brave only) |
| `protectedFields` | string[] | no | youtrack-rest: fields never written through the connection, entries `"Project:Field"` or `"Field"` |
| `setOnceFields` | string[] | no | youtrack-rest: fields written only while still empty, same entry form |
| `auth` | string | no | ssh: `"key"` (default) or `"agent"` |
| `keyFile` | string | no | ssh: path of a private key file on the client, instead of the stored key |
| `hostKey` | string | no | ssh: the server's public key to pin (`"ssh-ed25519 AAAA…"`). Recommended for provisioned hosts — the server knows its machines, and no `known_hosts` has to be managed on the client. |
| `allowCommands` | string[] | no | ssh: commands the connection may run, entries as leading tokens (`"systemctl status"`) |
| `denyCommands` | string[] | no | ssh: commands it never runs, same form, wildcards `*`/`?` |
| `privateKey` | string | no | ssh: the private key (PEM). If omitted, mux checks the secret store for `{name}-private-key`. |
| `passphrase` | string | no | ssh: passphrase of an encrypted `privateKey`. If omitted, mux checks the secret store for `{name}-private-key-passphrase`. |
| `smtpHost` | string | no | imap: SMTP submission server for sending; signs in with `user` and `password` |
| `smtpPort` | integer | no | imap: SMTP port (default 587 with STARTTLS; 465 is implicit TLS) |
| `allowSend` | boolean | no | imap: register the send tools (default: false). Requires `smtpHost` and a `user` that is an email address — `smtpHost` alone does not enable sending. |
| `smtpSavesSent` | boolean | no | imap: the provider files mail submitted over SMTP in Sent itself, so mux stores no copy (default: false) |
| `fromName` | string | no | imap: display name in the From header of drafts and sent mail |

### Tunnel Object

WireGuard (the default when `type` is omitted):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique tunnel identifier |
| `type` | string | no | `"wireguard"` (default) or `"ssh"` |
| `peerPublicKey` | string | yes | WireGuard peer public key (base64) |
| `peerEndpoint` | string | yes | Peer endpoint (`host:port`) |
| `allowedIPs` | string | yes | Allowed IP ranges (CIDR, comma-separated) |
| `tunnelAddress` | string | yes | Local tunnel IP address (CIDR, e.g. `10.100.0.42/32`) |
| `dns` | string | no | DNS servers (comma-separated IPs) |
| `privateKey` | string | no | WireGuard private key (base64). If omitted, mux checks the secret store for `tunnel-{name}-private-key`. |
| `presharedKey` | string | no | WireGuard preshared key (base64). If omitted, mux checks the secret store. |
| `mtu` | integer | no | MTU (default: 1420) |
| `keepalive` | integer | no | Persistent keepalive in seconds (default: 25) |

SSH (`"type": "ssh"`):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique tunnel identifier |
| `host` | string | yes | SSH server hostname or IP |
| `port` | integer | no | SSH port (default: 22) |
| `user` | string | yes | SSH username |
| `auth` | string | no | `"key"` (default) or `"agent"` |
| `keyFile` | string | no | Path of a private key file on the client |
| `privateKey` | string | no | SSH private key (PEM). If omitted, mux checks the secret store for `tunnel-{name}-private-key`. |
| `passphrase` | string | no | Passphrase of an encrypted `privateKey`. If omitted, mux checks the secret store for `tunnel-{name}-private-key-passphrase`. |

`insecureHostKey` is not honoured from provisioning: mux clears it on provisioned SSH tunnels (with a log line) and verifies the host against the client's `known_hosts` files. For a provisioned ssh *connection*, deliver the host's key in `hostKey` instead.

## How Secrets Are Handled

Passwords and keys can be provided in the JSON response or stored locally on the client. mux applies this precedence:

1. **JSON response** -- if the provisioning server includes `password`, `token`, `privateKey` or `passphrase`, those values are used
2. **Local secret store** -- if a field is empty in the JSON, mux checks the local secret store (Vault → OS Keychain → File fallback, see [configuration.md](configuration.md#secrets))
3. **Neither** -- the connection/tunnel is skipped as incomplete

This means you can choose: distribute secrets from the server (simpler setup) or have users store secrets locally (the server never knows them).

For HTTP connections without their own token: if the connection URL points to the same host as the provisioning endpoint, mux automatically reuses the provisioning Bearer token.

## How Provisioned and Local Config Interact

- Provisioned items are marked with `source: "provisioning"`, local items with `source: "local"`
- **Tunnels**: On name collision, the provisioned tunnel wins (local tunnel with the same name is hidden)
- **Connections**: Both provisioned and local connections are included; no deduplication by name
- **UI protection**: Provisioned connections and tunnels cannot be edited or deleted in the web UI or via MCP tools
- **Config persistence**: Provisioned items are kept in memory only; they are never written to `config.toml`

## Example: Multiple Connection Types

```json
{
  "connections": [
    {
      "name": "production-db",
      "type": "mariadb",
      "host": "10.100.0.5",
      "port": 3306,
      "user": "app_readonly",
      "password": "secret123",
      "database": "production",
      "readOnly": true,
      "tunnel": "corp-vpn",
      "instructions": "Production MariaDB. Read-only. Contains customer and order data."
    },
    {
      "name": "analytics",
      "type": "clickhouse",
      "host": "10.100.0.10",
      "port": 8123,
      "user": "analyst",
      "password": "analytics_pass",
      "database": "analytics",
      "tunnel": "corp-vpn"
    },
    {
      "name": "internal-api",
      "type": "http",
      "url": "https://api.internal.example.com",
      "token": "api-key-here",
      "instructions": "Internal REST API. GET /api/v1/products for product catalog."
    }
  ],
  "tunnels": [
    {
      "name": "corp-vpn",
      "peerPublicKey": "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=",
      "peerEndpoint": "vpn.example.com:51820",
      "allowedIPs": "10.100.0.0/16",
      "tunnelAddress": "10.100.0.42/32",
      "dns": "10.100.0.1",
      "privateKey": "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY=",
      "mtu": 1420,
      "keepalive": 25
    }
  ]
}
```

## Building a Provisioning Server

The provisioning endpoint is a plain HTTP JSON API. Any language and framework works.

### Go

```go
package main

import (
	"encoding/json"
	"net/http"
)

var validToken = "your-secret-token"

type Connection struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Database string `json:"database,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
	Tunnel   string `json:"tunnel,omitempty"`
	URL      string `json:"url,omitempty"`
	Token    string `json:"token,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

type Tunnel struct {
	Name          string `json:"name"`
	PeerPublicKey string `json:"peerPublicKey"`
	PeerEndpoint  string `json:"peerEndpoint"`
	AllowedIPs    string `json:"allowedIPs"`
	TunnelAddress string `json:"tunnelAddress"`
	DNS           string `json:"dns,omitempty"`
	PrivateKey    string `json:"privateKey,omitempty"`
	MTU           int    `json:"mtu,omitempty"`
	KeepAlive     int    `json:"keepalive,omitempty"`
}

type Response struct {
	Connections []Connection `json:"connections"`
	Tunnels     []Tunnel     `json:"tunnels,omitempty"`
}

func main() {
	http.HandleFunc("/api/mux/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+validToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		resp := Response{
			Connections: []Connection{
				{Name: "production", Type: "postgresql", Host: "10.100.0.5", Port: 5432,
					User: "readonly", Password: "secret", Database: "production", ReadOnly: true},
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	http.ListenAndServe(":8080", nil)
}
```

### Node.js

```js
const express = require("express");
const app = express();

const VALID_TOKEN = "your-secret-token";

app.get("/api/mux/config", (req, res) => {
  const auth = req.headers.authorization;
  if (auth !== `Bearer ${VALID_TOKEN}`) {
    return res.status(401).json({ error: "unauthorized" });
  }

  res.json({
    connections: [
      {
        name: "production",
        type: "postgresql",
        host: "10.100.0.5",
        port: 5432,
        user: "readonly",
        password: "secret",
        database: "production",
        readOnly: true,
      },
    ],
    tunnels: [],
  });
});

app.listen(8080);
```

### Key Points

- The endpoint must return `Content-Type: application/json`
- The response body must start with `{` or `[` (mux rejects HTML responses as auth errors)
- Keep the response under 1 MB
- Use HTTPS in production
- Rotate tokens periodically; mux treats 401/403 as clear "check your token" errors
- You can serve different configs per user by inspecting the Bearer token
- Empty `tunnels` array is valid; the field is optional in the JSON

## Security Considerations

- Always use HTTPS for the provisioning endpoint
- The Bearer token authenticates the user; treat it like a password
- If the provisioning server sends database passwords in the response, those passwords travel over the network -- HTTPS is essential
- Consider per-user tokens so you can revoke access for individual users
- mux does not follow redirects, which prevents token leakage to SSO login pages
- The provisioning server should validate tokens before returning any data
