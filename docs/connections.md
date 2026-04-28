# Connection Types

Every connection in mux is defined as a `[[connections]]` entry in `config.toml`. Each connection's MCP tools are prefixed with the connection name: a connection named `mydb` exposes tools like `mydb_query`, `mydb_list_tables`, etc.

## Database Connections

### MariaDB

Connects to a MariaDB/MySQL server via the Go `mysql` driver.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default: 3306), `database`, `read_only` (default: false), `tunnel`, `instructions`

**Secret**: `{name}-password` in secret store

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

**Secret**: `{name}-password` in secret store

```toml
[[connections]]
name = "production"
type = "postgresql"
host = "10.100.0.5"
port = 5432
user = "readonly"
database = "production"
tunnel = "office-vpn"
instructions = "Production database."
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

**Secret**: `{name}-password` in secret store

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

**Secret**: `{name}-oauth-refresh-token` in secret store (stored automatically after first login)

Default scopes: `User.Read Mail.Read Mail.ReadWrite Mail.Send offline_access`

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
| `{name}_search_messages` | Search messages across all mail folders using KQL. Parameters: `query` (required), `limit` (optional). |
| `{name}_create_reply_draft` | Create a reply draft (does NOT send). Parameters: `conversation_id`, `body` (required). |
| `{name}_create_draft` | Create a new email draft from scratch (does NOT send). Parameters: `to`, `subject`, `body` (required), `cc`, `bcc` (optional). |
| `{name}_create_forward_draft` | Create a forward draft (does NOT send). Parameters: `conversation_id`, `to` (required), `body`, `cc`, `bcc` (optional). |
| `{name}_list_attachments` | List attachments of a message. Parameter: `message_id` (required). |
| `{name}_get_attachment` | Get attachment content (base64-encoded). Parameters: `message_id`, `attachment_id` (required). |
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

**Secret**: `{name}-token` in secret store (API key)

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

**Secret**: `{name}-token` in secret store (API key)

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

### Recraft

HTTP client for the Recraft image generation API with automatic Bearer authentication. The base URL already includes `/v1`, so all paths must be specified **without** a `/v1` prefix. Note that Recraft returns WebP images by default, even when `response_format` is set to `"url"`.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://external.api.recraft.ai/v1`), `instructions`

**Secret**: `{name}-token` in secret store (API key)

```toml
[[connections]]
name = "recraft"
type = "recraft"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request to the Recraft API. Parameter: `path` (required, e.g. `/users/me`). |
| `{name}_post` | HTTP POST request with JSON body to the Recraft API. Parameters: `path` (required, e.g. `/images/generations`), `body` (required, JSON string). |

Default instructions describe available endpoints:

| Endpoint | Description |
|----------|-------------|
| `POST /images/generations` | Generate image from text prompt. Body: `prompt`, `model` (`recraftv4`, `recraftv4_vector`, `recraftv4_pro`, `recraftv4_pro_vector`, `recraftv3`, `recraftv3_vector`), `size`, `style`, `n`, `response_format`. V4 styles: `realistic_image`, `digital_illustration`. V3 styles: `realistic_image`, `digital_illustration`, `vector_illustration`. Sizes: 1024x1024, 1365x1024, 1024x1365, 1536x1024, 1024x1536, 1820x1024, 1024x1820, 1024x2048, 2048x1024, and more. |
| `GET /users/me` | User info and remaining credits. |
| `POST /images/vectorize` | Vectorize a raster image (multipart: file). |
| `POST /images/removeBackground` | Remove image background (multipart: file). |
| `POST /images/crispUpscale` | Crisp upscale (multipart: file). |
| `POST /images/creativeUpscale` | Creative upscale with enhancement (multipart: file). |
| `POST /images/imageToImage` | Image-to-image transformation (multipart: file, prompt, strength). |
| `POST /images/inpaint` | Inpainting (multipart: file, mask, prompt). |
| `POST /images/replaceBackground` | Replace image background (multipart: file, prompt). |
| `POST /styles` | Create a style reference (multipart: files). |

### Ideogram

HTTP client for the Ideogram image generation API with automatic `Api-Key` header authentication.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.ideogram.ai`), `instructions`

**Secret**: `{name}-token` in secret store (API key)

```toml
[[connections]]
name = "ideogram"
type = "ideogram"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request to the Ideogram API. Parameter: `path` (required). |
| `{name}_post` | HTTP POST request with JSON body to the Ideogram API. Parameters: `path` (required, e.g. `/v1/ideogram-v3/generate`), `body` (required, JSON string). |

Default instructions describe available endpoints:

| Endpoint | Description |
|----------|-------------|
| `POST /v1/ideogram-v3/generate` | Generate image from text prompt. Body: `prompt`, `rendering_speed` (`FLASH`, `TURBO`, `DEFAULT`, `QUALITY`), `resolution`, `aspect_ratio`, `style_type` (`AUTO`, `GENERAL`, `REALISTIC`, `DESIGN`, `FICTION`), `magic_prompt`, `num_images`. |
| `POST /v1/ideogram-v3/remix` | Remix an existing image (multipart: image, prompt). |
| `POST /v1/ideogram-v3/edit` | Edit an image (multipart: image, mask, prompt). |
| `POST /v1/ideogram-v3/reframe` | Reframe/extend an image (multipart: image, resolution). |
| `POST /v1/ideogram-v3/replace-background` | Replace image background (multipart: image, prompt). |
| `POST /v1/ideogram-v3/describe` | Describe an image (multipart: image). |

### Brave Search

Web and local search via the Brave Search API. Includes client-side request counting and rate limit tracking.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.search.brave.com`), `monthly_limit` (0 = unlimited), `instructions`

**Secret**: `{name}-token` in secret store (API key)

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

**Secret**: `{name}-token` in secret store (API key)

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

**Secret**: `{name}-token` in secret store (the entire service account JSON key file content)

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

### Asana

REST API client for Asana project management. Uses Personal Access Token authentication.

**Required fields**: `name`, `type`

**Secret**: `{name}-token` in secret store (Personal Access Token)

```toml
[[connections]]
name = "asana"
type = "asana"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_me` | Get the current authenticated Asana user. |
| `{name}_workspaces` | List all accessible Asana workspaces. |
| `{name}_projects` | List projects in a workspace. Parameters: `workspace` (required), `archived` (optional). |
| `{name}_sections` | List sections in a project. Parameter: `project` (required). |
| `{name}_tasks` | List tasks. Parameters: `project` or `section` (one required), `assignee`, `workspace`, `completed` (optional). |
| `{name}_get_task` | Get detailed information about a specific task. Parameter: `task` (required). |
| `{name}_create_task` | Create a new task. Parameters: `name` (required), `workspace`, `project`, `section`, `assignee`, `due_on`, `notes` (optional). |
| `{name}_update_task` | Update an existing task. Parameters: `task` (required), `name`, `assignee`, `due_on`, `notes`, `completed` (optional). |
| `{name}_search` | Search tasks in a workspace. Parameters: `workspace` (required), `text`, `assignee`, `completed`, `project`, `due_on_before`, `due_on_after` (optional). |

### IMAP

Connects to an IMAP mailbox with conversation threading (subject-based grouping). Supports MIME parsing, RFC 2047 encoded headers, and common mailbox operations.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default: 993), `tunnel`, `instructions`

**Secret**: `{name}-password` in secret store

```toml
[[connections]]
name = "mail"
type = "imap"
host = "imap.example.com"
port = 993
user = "user@example.com"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_list_conversations` | List inbox conversations grouped by thread. Returns latest message, participants, and message count. Parameter: `limit` (optional). |
| `{name}_get_conversation` | Get all messages in a conversation thread with full body text. Parameter: `conversation_id` (required). |
| `{name}_search_messages` | Search messages by text, grouped by conversation. Parameters: `query` (required), `limit` (optional). |
| `{name}_list_mailboxes` | List all IMAP mailbox folders. |
| `{name}_archive_conversation` | Archive a conversation (move to Archive or Trash). Parameter: `conversation_id` (required). |
| `{name}_delete_conversation` | Delete a conversation (move to Trash, not permanent). Parameter: `conversation_id` (required). |
| `{name}_create_reply_draft` | Create a reply draft for the latest message. Saved to Drafts, does NOT send. Parameters: `conversation_id`, `body` (required). |
| `{name}_create_forward_draft` | Create a forward draft. Saved to Drafts, does NOT send. Parameters: `conversation_id`, `to` (required), `body` (optional). |

### Meilisearch

Connects to a Meilisearch instance and exposes hybrid search (keyword + semantic) over an index. Useful for knowledge bases with vector embeddings configured.

**Required fields**: `name`, `type`, `host`, `database` (index name)

**Optional fields**: `port` (default: 7700), `secure` (default: false), `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (API key, optional for unauthenticated instances)

**Index requirements**: The `path` attribute must be configured as filterable in the Meilisearch index for the `read` tool to work. Hybrid search requires an embedder named `openai` configured on the index.

```toml
[[connections]]
name = "documentation"
type = "meilisearch"
host = "localhost"
port = 7711
database = "docs"
instructions = "Semantic search over documentation. Use when grep is insufficient."
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_search` | Hybrid search (keyword + semantic) over the index. Parameters: `query` (required), `limit` (optional, default 5, max 20). Uses `semanticRatio: 0.7` for balanced keyword/semantic weighting. |
| `{name}_read` | Read a full document by its path. Parameter: `path` (required, as returned by search results). Uses a filter-based exact match. |

## Proxy Connections

### MCP Proxy

Connects to an upstream MCP server, discovers its tools, and re-exposes them with a `{name}_` prefix. Supports Bearer token and OAuth 2.0 + PKCE authentication.

**Required fields**: `name`, `type`, `url`

**Optional fields**: `oauth` (default: false), `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (Bearer token) or `{name}-oauth-token` (OAuth)

```toml
# Bearer token proxy
[[connections]]
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"

# OAuth proxy
[[connections]]
name = "sentry"
type = "sentry"
url = "https://mcp.sentry.dev/mcp"
```

**MCP tools exposed**: All tools from the upstream server, prefixed with `{name}_`.

URLs ending in `/sse` use SSE transport; everything else uses Streamable HTTP.

Proxy-type aliases (`youtrack`, `sentry`, `netdata`, `notion`, `asana-mcp`) behave identically to `proxy` but provide semantic clarity. `sentry` and `notion` additionally default `oauth = true`. `asana-mcp` defaults `oauth = true` and URL to `https://mcp.asana.com/v2/mcp`.

### Google Workspace

Proxies tools from a local [google_workspace_mcp](https://github.com/taylorwilsdon/google_workspace_mcp) server. Provides access to Gmail, Google Drive, Docs, Sheets, Calendar, and Tasks. The upstream server handles Google OAuth 2.0 — no token is needed in mux.

**Required fields**: `name`, `type`, `url`

**Optional fields**: `instructions`

```toml
[[connections]]
name = "google-workspace"
type = "google-workspace"
url = "http://localhost:8000/mcp"
```

**MCP tools exposed**: All tools from the upstream google_workspace_mcp server, prefixed with `{name}_`. Typical tools include Gmail search/send/draft, Drive file management, Docs read/write, Sheets operations, Calendar event management, and Tasks CRUD.

**Prerequisites**: The google_workspace_mcp server must be running locally with valid Google OAuth credentials before mux can connect. See [`google-workspace-setup.md`](google-workspace-setup.md) for the full setup guide.

### HTTP

Generic HTTP client for any REST API. Useful for internal APIs or services without a dedicated connection type.

**Required fields**: `name`, `type`, `url`

**Optional fields**: `read_only` (default: false), `token_header`, `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (optional auth token)

By default, the token is sent as `Authorization: Bearer {token}`. Set `token_header` to use a custom header name instead (e.g. `x-goog-api-key` for Google APIs).

```toml
[[connections]]
name = "internal-api"
type = "http"
url = "https://api.internal.example.com"
token_header = "x-api-key"
instructions = "Internal product API. Use /api/v1/products to list products."
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request. Parameters: `path` (required), `output_file` (optional — save response to file). |
| `{name}_request` | HTTP request with body (POST, PUT, PATCH, DELETE). Parameters: `path`, `method`, `body`, `output_file`. Available unless `read_only = true`. |

Both tools support an optional `output_file` parameter. When set, the response body is streamed to the specified file path (no size limit) and only metadata (status, content-type, path, size) is returned. Useful for large responses like images or binary data.

### Google Gemini

Dedicated connector for the Google Gemini API with automatic image handling. Text responses are returned inline; generated images are decoded from base64 and saved to disk (`~/.mux/output/` by default).

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://generativelanguage.googleapis.com/v1beta`), `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (API key, sent as `x-goog-api-key`)

```toml
[[connections]]
name = "gemini"
type = "gemini"
# url defaults to https://generativelanguage.googleapis.com/v1beta
# Token stored as secret (key: "gemini-token")
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_list_models` | List available models with capabilities. Use to discover model IDs before generating. |
| `{name}_generate` | Generate content (text/images). Parameters: `model` (required), `prompt` (required), `images` (optional, for editing), `response_modalities`, `aspect_ratio`, `image_size`, `output_dir`. |

### fal.ai

Async inference queue connector for [fal.ai](https://fal.ai) — video, image and other generative models (Seedance, Kling, Flux, etc.). Long-running jobs are submitted to a queue; callers poll for status and fetch the final result separately, so that a single MCP call never blocks on minute-long generations.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://queue.fal.run`), `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (API key, sent as `Authorization: Key <token>`)

```toml
[[connections]]
name = "fal"
type = "fal-ai"
# url defaults to https://queue.fal.run
# Token stored as secret (key: "fal-token")
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_submit` | Submit an async job to a fal.ai model. Parameters: `model` (required, e.g. `bytedance/seedance-2.0/text-to-video`), `input` (required, JSON string with model parameters). Returns `request_id`, `status_url`, `response_url`. |
| `{name}_status` | Check queue status. Parameters: `status_url` (required). Returns status (`IN_QUEUE`, `IN_PROGRESS`, `COMPLETED`), queue position, logs. |
| `{name}_result` | Fetch the result of a completed job. Parameters: `response_url` (required). Call only after status reports `COMPLETED`. |

Status and response URLs are validated against the configured host — unexpected hosts are rejected to prevent SSRF via manipulated API responses.

## Passive Connections

Passive connections don't expose MCP tools. They provide credentials for external processes that communicate with the mux daemon through internal channels (not MCP).

### Git Credential

Provides HTTPS authentication for git operations (clone, pull, push). The `mux git-credential` subcommand implements the [git credential helper protocol](https://git-scm.com/docs/gitcredentials) and fetches tokens from the vault via a Unix domain socket (`~/.mux/credential.sock`).

**Required fields**: `name`, `type`, `host`, `user`

**Secret**: `{name}-token` in secret store (Personal Access Token)

```toml
[[connections]]
name = "gitlab"
type = "git"
host = "gitlab.com"
user = "oauth2"

[[connections]]
name = "github"
type = "git"
host = "github.com"
user = "x-access-token"
```

**Git configuration** (required on the host):

```ini
# ~/.gitconfig
[url "https://gitlab.com/"]
    insteadOf = git@gitlab.com:

[url "https://github.com/"]
    insteadOf = git@github.com:

[credential]
    helper = /path/to/mux git-credential
```

The `url.insteadOf` rules transparently rewrite SSH URLs to HTTPS. The credential helper is invoked by git when authentication is needed — it connects to the running mux daemon via Unix socket, retrieves the token from the vault, and outputs it in the git credential protocol format. Git consumes the token internally; it is not visible to calling processes.

**MCP tools exposed**: None. Git connections are visible in `connection_list` for discoverability but do not register tools. Agents should use git via the `Bash` tool, not through MCP.

**Security**: The Unix socket is restricted to the file owner (chmod 0600). The token is never exposed via HTTP endpoints or MCP tools. See the security architecture documentation for the full threat model.

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
| `secret_set` | Store a secret (vault or keychain, depending on configuration). Write-only. Parameters: `key`, `value` (required). |
| `secret_check` | Check which secrets are set (true/false per key, never reveals values). Parameter: `connection` (optional filter). |
| `tunnel_add` | Add a new tunnel (WireGuard or SSH). Parameters: `name`, `type` (required), plus type-specific fields. |
| `tunnel_delete` | Delete a local tunnel. Provisioned tunnels cannot be deleted. Parameter: `name` (required). |
| `provisioning_set` | Set the remote provisioning endpoint. Parameter: `endpoint` (required). Token stored separately via `secret_set` with key `provisioning-token`. |

### Vault Tools

Available when the vault is enabled in configuration.

| Tool | Description |
|------|-------------|
| `vault_status` | Show vault state (uninitialized, sealed, unlocked), secret count, and inactivity timer. |
| `vault_init` | Initialize a new vault with a passphrase. Parameter: `passphrase` (required). |
| `vault_unlock` | Unlock the vault with a passphrase. Parameter: `passphrase` (required). |
| `vault_lock` | Lock the vault immediately. Wipes encryption key from memory. |
| `vault_migrate` | Migrate secrets from legacy stores (keychain, secrets.toml) into the vault. |
| `vault_ssh_status` | Show SSH key status (stored in vault, loaded in ssh-agent, fingerprint). |
| `vault_ssh_load` | Load SSH private key from vault into ssh-agent with a temporary lifetime. Parameter: `lifetime_secs` (optional, default: 120). |
