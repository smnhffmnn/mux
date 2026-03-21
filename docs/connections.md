# Connection Types

Every connection in mux is defined as a `[[connections]]` entry in `config.toml`. Each connection's MCP tools are prefixed with the connection name: a connection named `mydb` exposes tools like `mydb_query`, `mydb_list_tables`, etc.

## Database Connections

### MariaDB

Connects to a MariaDB/MySQL server via the Go `mysql` driver.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default: 3306), `database`, `read_only` (default: false), `tunnel`, `instructions`

**Secret**: `{name}-password` in keychain

```toml
[[connections]]
name = "local-dev"
type = "mariadb"
host = "localhost"
port = 3306
user = "root"
database = "myapp"
read_only = false
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_query` | Execute a read-only SQL query (SELECT, SHOW, DESCRIBE, EXPLAIN, WITH). Returns JSON. |
| `{name}_execute` | Execute a write SQL statement (INSERT, UPDATE, DELETE). Returns affected rows. Blocked when `read_only = true`. |
| `{name}_list_tables` | List all tables in the configured database. |
| `{name}_describe_table` | Show column schema (name, type, nullability, keys, defaults). Parameter: `table` (required). |

### PostgreSQL

Connects to a PostgreSQL server via `lib/pq`.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default: 5432), `database`, `tunnel`, `instructions`

**Secret**: `{name}-password` in keychain

```toml
[[connections]]
name = "production"
type = "postgresql"
host = "10.100.0.5"
port = 5432
user = "readonly"
database = "production"
read_only = true
tunnel = "office-vpn"
instructions = "Production database. Read-only access."
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_query` | Execute a read-only SQL query (SELECT, SHOW, EXPLAIN, WITH). Returns JSON. |
| `{name}_list_tables` | List all tables (excludes `pg_catalog` and `information_schema`). Returns `schema.table` format. |
| `{name}_describe_table` | Show column schema. Parameter: `table` (required, supports `schema.table` notation, defaults to `public` schema). |

### ClickHouse

Connects to a ClickHouse server via `clickhouse-go`. Protocol is auto-detected from port: 8123/8443 = HTTP, anything else = native TCP.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default: 8123), `database` (default: `"default"`), `secure` (TLS for HTTP, default: false), `tunnel`, `instructions`

**Secret**: `{name}-password` in keychain

```toml
[[connections]]
name = "analytics"
type = "clickhouse"
host = "10.100.0.10"
port = 8123
user = "analyst"
database = "analytics"
secure = false
tunnel = "office-vpn"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_query` | Execute a read-only SQL query. Use fully qualified names (`database.table`) to query across databases. |
| `{name}_list_databases` | List all databases with engine and comment. |
| `{name}_list_tables` | List tables. Optional parameter: `database` (omit to list all non-system databases). |
| `{name}_describe_table` | Show column schema with types, defaults, and comments. Parameter: `table` (required, supports `database.table` notation). |

## API Connections

### Microsoft Graph

Accesses Microsoft 365 mail and SharePoint via the Microsoft Graph REST API. Authentication uses the OAuth 2.0 device code flow -- no client secret needed, just sign in with your browser when prompted.

**Required fields**: `name`, `type`

**Optional fields**: `scopes` (override default scopes), `tunnel`, `instructions`

**Secret**: `{name}-oauth-refresh-token` in keychain (stored automatically after first login)

Default scopes: `Mail.Read Mail.ReadWrite Mail.Send offline_access`

```toml
[[connections]]
name = "outlook"
type = "microsoft-graph"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_auth_status` | Check authentication status. |
| `{name}_auth_login` | Start device code flow. Returns a `user_code` and verification URL. |
| `{name}_auth_poll` | Poll for device code completion. Parameter: `device_code` (required). |
| `{name}_list_conversations` | List inbox conversations grouped by thread. Parameter: `limit` (default: 20). |
| `{name}_get_conversation` | Get all messages in a conversation. Parameter: `conversation_id` (required). |
| `{name}_archive_conversation` | Move all inbox messages of a conversation to Archive. Parameter: `conversation_id` (required). |
| `{name}_delete_conversation` | Delete all inbox messages of a conversation. Parameter: `conversation_id` (required). |
| `{name}_create_reply_draft` | Create a reply draft (does NOT send). Parameters: `conversation_id`, `body` (required). |
| `{name}_create_forward_draft` | Create a forward draft (does NOT send). Parameters: `conversation_id`, `to` (required), `body` (optional). |
| `{name}_sp_list_sites` | Search SharePoint sites. Parameter: `query` (default: `*`). |
| `{name}_sp_get_site` | Get SharePoint site details. Parameter: `site_id` (required). |
| `{name}_sp_list_drives` | List document libraries for a site with quota info. Parameter: `site_id` (required). |
| `{name}_sp_list_items` | List files/folders in a drive. Parameters: `drive_id` (required), `item_id`, `order_by`, `top`, `page_token`. |
| `{name}_sp_get_item_versions` | Get file version history. Parameters: `drive_id`, `item_id` (required). |
| `{name}_sp_storage_report` | SharePoint storage usage report (admin only). Parameter: `period` (D7/D30/D90/D180). |
| `{name}_sp_search` | Search files/folders in a drive. Parameters: `drive_id`, `query` (required), `top`. |

### OpenAI

HTTP client for the OpenAI REST API with automatic Bearer authentication.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.openai.com`), `instructions`

**Secret**: `{name}-token` in keychain (API key)

```toml
[[connections]]
name = "openai"
type = "openai"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request to the OpenAI API. Parameter: `path` (required, e.g. `/v1/models`). |

Default instructions describe available endpoints (audio transcriptions, chat completions, embeddings, image generation, models).

### ElevenLabs

HTTP client for the ElevenLabs audio API with automatic `xi-api-key` authentication.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.elevenlabs.io`), `instructions`

**Secret**: `{name}-token` in keychain (API key)

```toml
[[connections]]
name = "elevenlabs"
type = "elevenlabs"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request to the ElevenLabs API. Parameter: `path` (required, e.g. `/v1/voices`). |

Default instructions describe available endpoints (TTS, streaming TTS, sound generation, voices, models, subscription).

### Brave Search

Web and local search via the Brave Search API. Includes client-side request counting and rate limit tracking.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.search.brave.com`), `monthly_limit` (0 = unlimited), `instructions`

**Secret**: `{name}-token` in keychain (API key)

```toml
[[connections]]
name = "brave"
type = "brave"
monthly_limit = 2000
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_web_search` | Web search. Parameters: `query` (required), `count` (default: 10, max: 20), `country`, `search_lang`, `freshness`. |
| `{name}_local_search` | Local business/place search. Parameters: `query` (required), `count` (default: 5). |
| `{name}_usage` | Show request count and rate limit stats for the current month. |

### Firecrawl

Web scraping, searching, and crawling via the Firecrawl API.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.firecrawl.dev`), `instructions`

**Secret**: `{name}-token` in keychain (API key)

```toml
[[connections]]
name = "firecrawl"
type = "firecrawl"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_scrape` | Scrape a URL and return clean markdown. Parameters: `url` (required), `formats`, `onlyMainContent`. |
| `{name}_search` | Web search with full page content. Parameters: `query` (required), `limit` (default: 5). |
| `{name}_crawl` | Start crawling from a URL. Parameters: `url` (required), `limit` (default: 10), `maxDepth` (default: 2). Returns job ID. |
| `{name}_crawl_status` | Check crawl job status. Parameter: `id` (required). |
| `{name}_map` | Discover URLs on a website. Parameter: `url` (required). |
| `{name}_usage` | Show Firecrawl credit usage for the current billing period. |

### Google Tag Manager

Full GTM management via the Tag Manager API v2. Authentication uses a Google service account.

**Required fields**: `name`, `type`

**Secret**: `{name}-token` in keychain (the entire service account JSON key file content)

```toml
[[connections]]
name = "gtm"
type = "google-tagmanager"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_list_accounts` | List all GTM accounts. |
| `{name}_list_containers` | List containers. Parameter: `account_id` (required). |
| `{name}_list_workspaces` | List workspaces. Parameter: `container_path` (required). |
| `{name}_list_tags` | List tags in a workspace. Parameter: `workspace_path` (required). |
| `{name}_create_tag` | Create a tag. Parameters: `workspace_path`, `tag_body` (JSON, required). |
| `{name}_update_tag` | Update a tag. Parameters: `tag_path`, `tag_body` (JSON, required). |
| `{name}_delete_tag` | Delete a tag. Parameter: `tag_path` (required). |
| `{name}_list_triggers` | List triggers. Parameter: `workspace_path` (required). |
| `{name}_create_trigger` | Create a trigger. Parameters: `workspace_path`, `trigger_body` (JSON, required). |
| `{name}_list_variables` | List variables. Parameter: `workspace_path` (required). |
| `{name}_create_variable` | Create a variable. Parameters: `workspace_path`, `variable_body` (JSON, required). |
| `{name}_create_version` | Create a container version. Parameters: `workspace_path`, `name` (required), `notes` (optional). |
| `{name}_publish_version` | Publish a version. Parameter: `version_path` (required). |

## Proxy Connections

### MCP Proxy

Connects to an upstream MCP server, discovers its tools, and re-exposes them with a `{name}_` prefix. Supports Bearer token and OAuth 2.0 + PKCE authentication.

**Required fields**: `name`, `type`, `url`

**Optional fields**: `oauth` (default: false), `tunnel`, `instructions`

**Secret**: `{name}-token` in keychain (Bearer token) or `{name}-oauth-token` (OAuth)

```toml
# Bearer token proxy
[[connections]]
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"

# OAuth proxy
[[connections]]
name = "sentry"
type = "proxy"
url = "https://mcp.sentry.dev/mcp"
oauth = true
```

**MCP tools exposed**: All tools from the upstream server, prefixed with `{name}_`.

URLs ending in `/sse` use SSE transport; everything else uses Streamable HTTP.

Proxy-type aliases (`youtrack`, `sentry`, `netdata`, `notion`) behave identically to `proxy` but provide semantic clarity.

### HTTP

Generic HTTP GET client for any REST API. Useful for internal APIs or services without a dedicated connection type.

**Required fields**: `name`, `type`, `url`

**Optional fields**: `tunnel`, `instructions`

**Secret**: `{name}-token` in keychain (optional Bearer token)

```toml
[[connections]]
name = "internal-api"
type = "http"
url = "https://api.internal.example.com"
instructions = "Internal product API. Use /api/v1/products to list products."
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request. Parameter: `path` (required, e.g. `/api/products`). Bearer token sent automatically if set. |

## Built-in Tools

These tools are always available regardless of configured connections.

### get_datetime

Returns the current date and time in ISO 8601 format.

| Parameter | Description |
|-----------|-------------|
| `timezone_offset` | Optional UTC offset in hours (e.g. 1 for CET, 2 for CEST). Defaults to UTC. |

### Config Management Tools

These tools allow AI agents to inspect and modify the mux configuration at runtime.

| Tool | Description |
|------|-------------|
| `type_list` | List all available connection types with their required fields. |
| `connection_list` | List all connections and tunnels with type, source, and whether secrets are set. |
| `connection_add` | Add a new connection. Parameters: `name`, `type` (required), plus type-specific fields. |
| `connection_delete` | Delete a local connection. Provisioned connections cannot be deleted. |
| `secret_set` | Store a secret in the OS keychain. Write-only. Parameters: `key`, `value` (required). |
| `secret_check` | Check which secrets are set in the keychain (true/false per key, never reveals values). Parameter: `connection` (optional filter). |
