# Web UI & REST API

## Overview

mux includes an embedded web UI served at `/ui/`. The UI is a single HTML file (`internal/ui/static/index.html`) bundled into the binary via Go's `embed` package.

The REST API endpoints under `/api/` power the UI and can also be called directly.

## Endpoints

### `GET /api/status`

Returns the full state of the mux instance: server info, ERP status, tunnels, and connections.

**Response** `200 OK`:

```json
{
  "server": {
    "version": "1.2.0",
    "uptime": "2h15m30s",
    "transport": "http",
    "port": 7700
  },
  "erp": {
    "configured": true,
    "endpoint": "https://erp.example.com/api/mux/config",
    "tokenSet": true,
    "tunnels": 1,
    "connections": 2
  },
  "tunnels": [
    {
      "name": "office-vpn",
      "peerEndpoint": "vpn.example.com:51820",
      "tunnelAddress": "10.100.0.42/32",
      "source": "local",
      "connected": true
    }
  ],
  "connections": [
    {
      "name": "production",
      "type": "postgresql",
      "configured": true,
      "source": "local",
      "tunnel": "office-vpn",
      "details": {
        "host": "10.100.0.5",
        "port": "5432",
        "database": "production",
        "user": "readonly"
      },
      "secretsSet": {
        "password": true
      }
    },
    {
      "name": "youtrack",
      "type": "proxy",
      "configured": true,
      "source": "local",
      "details": {
        "url": "https://instance.myjetbrains.com/mcp"
      },
      "secretsSet": {
        "token": true
      }
    },
    {
      "name": "sentry",
      "type": "proxy",
      "configured": true,
      "source": "local",
      "details": {
        "url": "https://mcp.sentry.dev/mcp"
      },
      "secretsSet": {
        "oauth": true
      }
    }
  ]
}
```

Notes:
- `erp.configured` is true when both endpoint and token are set
- `tunnels[].connected` reflects whether the WireGuard tunnel started successfully
- `connections[].configured` is true when the connection has enough settings to function
- `connections[].source` is `"local"` or `"erp"`
- `secretsSet` shows whether secrets exist -- never exposes the actual values

---

### `POST /api/config`

Update fields for an existing connection. Secrets are saved to the OS keychain.

**Request**:

```json
{
  "connection": "production",
  "fields": {
    "host": "10.100.0.5",
    "port": "5432",
    "user": "readonly",
    "database": "production",
    "password": "secret123",
    "instructions": "Production database. Read-only."
  }
}
```

Supported fields: `host`, `port`, `user`, `database`, `url`, `instructions`, `password`, `token`.

**Response** `200 OK`:
```json
{"status": "saved"}
```

**Error responses**:
- `400` -- Invalid JSON
- `404` -- Connection not found
- `403` -- ERP-managed connection (cannot be edited)

Notes:
- `password` and `token` fields are saved to the OS keychain, not to the TOML file
- Non-secret fields are persisted to the TOML config file
- Only provided fields are updated; omitted fields are unchanged

---

### `POST /api/test-connection`

Test whether a connection is reachable. Performs a real connection attempt.

**Request**:

```json
{
  "connection": "production"
}
```

**Response** `200 OK`:

```json
{
  "connection": "production",
  "connected": true,
  "message": "Connected: PostgreSQL 16.2",
  "latency": "45ms"
}
```

Or on failure:

```json
{
  "connection": "production",
  "connected": false,
  "message": "dial tcp 10.100.0.5:5432: connection refused",
  "latency": "2003ms"
}
```

**Test behavior by connection type**:

| Type | What happens |
|------|-------------|
| `postgresql` | Opens SQL connection, pings, runs `SELECT version()` |
| `mariadb` | Opens SQL connection, pings, runs `SELECT VERSION()` |
| `clickhouse` | Opens connection via `tools.OpenClickHouseDB()`, pings, runs `SELECT version()` |
| `proxy` (bearer) | Creates MCP StreamableHTTP transport, calls `Initialize` + `ListTools` |
| `proxy` (OAuth) | Creates OAuth MCP client, calls `Initialize` + `ListTools` |

---

### `POST /api/connections`

Create a new connection with default settings.

**Request**:

```json
{
  "name": "my-new-db",
  "type": "postgresql"
}
```

Valid types: `postgresql`, `clickhouse`, `mariadb`, `proxy`.

**Response** `200 OK`:
```json
{"status": "created"}
```

**Error responses**:
- `400` -- Missing name or type, or invalid JSON
- `409` -- Connection with that name already exists

Notes:
- Creates the connection with default values (e.g., default port for the type)
- Persists to the TOML config file immediately
- Use `POST /api/config` afterward to set host, user, password, etc.

---

### `DELETE /api/connections/{name}`

Delete a local connection.

**Response** `200 OK`:
```json
{"status": "deleted"}
```

**Error responses**:
- `400` -- No connection name provided
- `404` -- Connection not found
- `403` -- ERP-managed connection (cannot be deleted)

Notes:
- Only deletes from the local config (`cfg.Connections`)
- ERP-managed connections are protected and return 403
- Persists the change to the TOML config file

---

### `POST /api/erp/setup`

Save ERP provisioning settings.

**Request**:

```json
{
  "endpoint": "https://erp.example.com/api/mux/config",
  "token": "my-provisioning-token"
}
```

Both fields are optional -- only provided fields are updated.

**Response** `200 OK`:
```json
{"status": "saved"}
```

Notes:
- `endpoint` is saved to the TOML config file
- `token` is saved to the OS keychain (key: `erp-token`)

---

### `POST /api/erp/sync`

Fetch configuration from the ERP and merge with local config.

**Request**: No body required.

**Response** `200 OK`:
```json
{
  "status": "synced",
  "tunnels": 1,
  "connections": 2
}
```

**Error responses**:
- `400` -- ERP not configured (endpoint or token missing)
- `502` -- ERP fetch failed (timeout, HTTP error, bad JSON)

Notes:
- Fetches from the configured ERP endpoint with a 20-second timeout
- Replaces any previous ERP data (`cfg.SetERP()`)
- Newly fetched ERP connections are registered as MCP tools on the running server
- Does **not** restart WireGuard tunnels -- tunnel changes require mux restart

---

### `POST /api/oauth/start`

Start an OAuth 2.0 + PKCE authorization flow for a proxy connection.

**Request**:

```json
{
  "connection": "sentry"
}
```

**Response** `200 OK`:
```json
{
  "authURL": "https://sentry.io/oauth/authorize?client_id=...&code_challenge=...&state=..."
}
```

**Error responses**:
- `400` -- Connection not found, not an OAuth connection, or URL missing
- `500` -- OAuth discovery, client registration, or state generation failed

**Flow**:
1. mux probes the MCP URL to discover the OAuth authorization server metadata
2. If no client ID exists, mux performs dynamic client registration
3. mux generates PKCE code verifier/challenge and state parameter
4. Returns the authorization URL -- the UI opens this in a new window
5. User authorizes in the external OAuth provider
6. Provider redirects back to `/ui/oauth/callback`

---

### `GET /ui/oauth/callback`

OAuth callback handler. Not called directly -- the OAuth provider redirects here after authorization.

**Query parameters**:
- `code` -- Authorization code from the OAuth provider
- `state` -- State parameter to match the pending flow

**Response**: HTML page showing success or error, auto-closes after 3 seconds.

**On success**:
- Token is exchanged and stored in the OS keychain
- The proxy is mounted on the MCP server (tools become available)

**On error** (missing code, expired state, token exchange failure):
- HTML error page displayed

---

## Static Assets

All other paths under `/ui/` are served from the embedded `internal/ui/static/` directory.

Currently this contains only `index.html` -- a self-contained SPA with inline CSS and JavaScript.

## Code Location

| File | Content |
|------|---------|
| `internal/ui/handler.go` | All API handlers, OAuth flow, `Handler` type |
| `internal/ui/static/index.html` | Embedded web UI (SPA) |
| `main.go:136-152` | HTTP mux setup, UI handler mounting |
