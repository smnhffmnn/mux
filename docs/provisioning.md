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
```

### Via Web UI

Open the mux web UI, navigate to the Provisioning section, enter the endpoint URL and token, and click Save. The token is stored in the secret store (vault, keychain, or file). Click "Sync" to fetch immediately.

## API Contract

### Request

```
GET {endpoint}
Authorization: Bearer {token}
Accept: application/json
```

mux sends this exact request. The endpoint must respond within 15 seconds.

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
| `type` | string | yes | One of: `mariadb`, `postgresql`, `clickhouse`, `proxy`, `youtrack`, `sentry`, `netdata`, `notion`, `http`, `microsoft-graph`, `openai`, `elevenlabs`, `brave`, `firecrawl`, `google-tagmanager` |
| `host` | string | databases | Database hostname or IP |
| `port` | integer | no | Port number (defaults applied per type: MariaDB 3306, PostgreSQL 5432, ClickHouse 8123) |
| `user` | string | databases | Database username |
| `password` | string | no | Database password. If omitted, mux checks the secret store for `{name}-password`. |
| `database` | string | no | Database name (ClickHouse defaults to `"default"`) |
| `readOnly` | boolean | no | Restrict to read-only queries (default: false) |
| `secure` | boolean | no | TLS for ClickHouse HTTP (default: false) |
| `url` | string | proxy/http/api | Endpoint URL |
| `token` | string | no | API key or Bearer token. If omitted, mux checks the secret store for `{name}-token`. |
| `oauth` | boolean | no | Use OAuth 2.0 + PKCE for proxy connections (default: false) |
| `scopes` | string | no | OAuth scopes override (microsoft-graph only) |
| `tunnel` | string | no | Name of a tunnel to route through |
| `instructions` | string | no | Instructions for AI agents describing the connection |
| `monthlyLimit` | integer | no | Monthly request limit (brave only) |

### Tunnel Object

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | yes | Unique tunnel identifier |
| `peerPublicKey` | string | yes | WireGuard peer public key (base64) |
| `peerEndpoint` | string | yes | Peer endpoint (`host:port`) |
| `allowedIPs` | string | yes | Allowed IP ranges (CIDR, comma-separated) |
| `tunnelAddress` | string | yes | Local tunnel IP address (CIDR, e.g. `10.100.0.42/32`) |
| `dns` | string | no | DNS servers (comma-separated IPs) |
| `privateKey` | string | no | WireGuard private key (base64). If omitted, mux checks the secret store for `tunnel-{name}-private-key`. |
| `presharedKey` | string | no | WireGuard preshared key (base64). If omitted, mux checks the secret store. |
| `mtu` | integer | no | MTU (default: 1420) |
| `keepalive` | integer | no | Persistent keepalive in seconds (default: 25) |

## How Secrets Are Handled

Passwords and keys can be provided in the JSON response or stored locally on the client. mux applies this precedence:

1. **JSON response** -- if the provisioning server includes `password`, `token`, or `privateKey`, those values are used
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
- The Bearer token authenticates the mux instance; treat it like a password
- If the provisioning server sends database passwords in the response, those passwords travel over the network -- HTTPS is essential
- Consider per-user tokens so you can revoke access for individual users
- mux does not follow redirects, which prevents token leakage to SSO login pages
- The provisioning server should validate tokens before returning any data
