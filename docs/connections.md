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

Accesses Microsoft 365 mail, calendar, and SharePoint via the Microsoft Graph REST API. Authentication uses the OAuth 2.0 device code flow -- no client secret needed, just sign in with your browser when prompted.

**Required fields**: `name`, `type`

**Optional fields**: `scopes` (override default scopes), `tunnel`, `instructions`

**Secret**: `{name}-oauth-refresh-token` in secret store (stored automatically after first login)

Default scopes: `User.Read Mail.Read Mail.ReadWrite Mail.Send Calendars.ReadWrite Calendars.Read.Shared offline_access`

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
| `{name}_list_conversations` | List conversations grouped by thread. Reads a window of the newest messages (`limit`×3, at most 250) — page backwards with `offset` (messages to skip: 250, 500, …) until the folder's `totalItemCount` from `list_mail_folders` is reached. Parameters: `limit` (default: 20), `offset` (default: 0), `folder_id` (default: Inbox). |
| `{name}_get_conversation` | Get all messages in a conversation, with full body. Scans recent messages across all folders by default; pass `folder_id` to scope to one folder. For a specific older/archived message, use `get_message`. Parameters: `conversation_id` (required), `folder_id` (optional). |
| `{name}_get_message` | Get a single message by ID with full body, regardless of folder (incl. archived). Use to fetch the full body of a message found via `search_messages` (which returns only `bodyPreview`). Parameter: `message_id` (required). |
| `{name}_archive_conversation` | Move all inbox messages of a conversation to Archive. Parameter: `conversation_id` (required). |
| `{name}_delete_conversation` | Delete all inbox messages of a conversation. Parameter: `conversation_id` (required). |
| `{name}_search_messages` | Search messages across all mail folders using KQL. Parameters: `query` (required), `limit` (optional). |
| `{name}_create_reply_draft` | Create a reply draft (does NOT send). Parameters: `conversation_id`, `body` (required). |
| `{name}_create_draft` | Create a new email draft from scratch (does NOT send). Parameters: `to`, `subject`, `body` (required), `cc`, `bcc` (optional). |
| `{name}_create_forward_draft` | Create a forward draft (does NOT send). Parameters: `conversation_id`, `to` (required), `body`, `cc`, `bcc` (optional). |
| `{name}_send_draft` | Send an existing draft (created by `create_draft` / `create_reply_draft` / `create_forward_draft`). Dispatches immediately and moves it to Sent Items. Parameter: `draft_id` (required). |
| `{name}_list_attachments` | List attachments of a message. Parameter: `message_id` (required). |
| `{name}_get_attachment` | Get attachment content (base64-encoded). Parameters: `message_id`, `attachment_id` (required). |
| `{name}_sp_list_sites` | Search SharePoint sites. Parameter: `query` (default: `*`). |
| `{name}_sp_get_site` | Get SharePoint site details. Parameter: `site_id` (required). |
| `{name}_sp_list_drives` | List document libraries for a site with quota info. Parameter: `site_id` (required). |
| `{name}_sp_list_items` | List files/folders in a drive. Parameters: `drive_id` (required), `item_id`, `order_by`, `top`, `page_token`. |
| `{name}_sp_get_item_versions` | Get file version history. Parameters: `drive_id`, `item_id` (required). |
| `{name}_sp_storage_report` | SharePoint storage usage report (admin only). Parameter: `period` (D7/D30/D90/D180). |
| `{name}_sp_search` | Search files/folders in a drive. Parameters: `drive_id`, `query` (required), `top`. |
| `{name}_cal_calendar_view` | List events in a time range. Recurring series are expanded into individual occurrences. Parameters: `start`, `end` (required), `top` (optional). |
| `{name}_cal_get_event` | Get a single event by ID, including body, attendees, and online meeting info. Parameter: `event_id` (required). |
| `{name}_cal_create_event` | Create a calendar event. Parameters: `subject`, `start`, `end` (required), plus `time_zone`, `body`, `location`, `attendees`, `online_meeting`, `all_day` (optional). |
| `{name}_cal_update_event` | Update fields on an existing event. Parameter: `event_id` (required), plus any of `subject`, `start`, `end`, `time_zone`, `body`, `location`, `attendees`. |
| `{name}_cal_delete_event` | Delete an event. Parameter: `event_id` (required). For meetings the user organized, this cancels and notifies attendees. |
| `{name}_cal_get_schedule` | Get free/busy availability for one or more mailboxes. Parameters: `schedules`, `start`, `end` (required), `time_zone`, `interval` (optional, 5–1440 minutes). |

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
| `{name}_get` | HTTP GET to the OpenAI API. Params: `path` (required, e.g. `/v1/models`), `output_file` (optional, stream binary/large responses to disk). |
| `{name}_request` | HTTP POST/PUT/PATCH/DELETE with a JSON body, or a multipart file upload. Params: `method`, `path`, `body` (JSON string), `output_file`, `file_path`, `file_field` (default `file`), `form_fields`. Use for `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/images/generations`; multipart endpoints such as `/v1/audio/transcriptions` or `/v1/files` take `file_path` plus `form_fields` (e.g. `{"model": "whisper-1"}`). See [Uploading files](#uploading-files). |

Default instructions describe both tools and list common POST endpoints.

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
| `{name}_get` | HTTP GET request to the ElevenLabs API. Params: `path` (required, e.g. `/v1/voices`), `output_file` (optional, absolute path). |
| `{name}_post` | HTTP POST with a JSON body. Params: `path` (required, e.g. `/v1/text-to-speech/{voice_id}`), `body` (required, JSON string), `output_file` (optional, absolute path). Use for text-to-speech, streaming TTS and sound generation. |

**Binary responses are never inlined.** The text-to-speech endpoints answer with raw audio bytes; the connector streams them to a file and returns the path plus metadata (status, content-type, size). With `output_file` the destination is yours — an unusable path (relative, or a file that already exists) is rejected *before* the request is sent, so a paid generation is not thrown away. Without `output_file` the audio lands in `~/.mux/output/` (same directory as the Gemini connector uses); mux does not delete those files.

Whether a response counts as binary is decided by its `Content-Type`: textual types (`application/json`, `application/xml`, `text/*`, `+json`/`+xml`, and the common `application/` text formats) are returned inline as text, truncated at 512 KB with an explicit marker; everything else is written to disk — audio types under their real extension, other binary payload as `.bin`. If the server declares no type at all, mux sniffs the first bytes, so header-less audio still becomes a file. Non-2xx responses are always inlined, so an API error stays readable and writes no file.

Endpoints that wrap audio in JSON (e.g. `…/with-timestamps`) count as textual — fetch those with `output_file` to avoid the 512 KB cut.

Default instructions describe available endpoints (TTS, streaming TTS, sound generation, voices, models, subscription) and the file handling.

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
| `{name}_post` | HTTP POST with a JSON body or a multipart image upload. Parameters: `path` (required, e.g. `/images/generations`), `body` (JSON string, required unless `file_path` is given), `file_path`, `file_field` (default `file`), `form_fields`. See [Uploading files](#uploading-files). |

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

The multipart endpoints take the image via `file_path`; text values such as `prompt` or `strength` go into `form_fields`. One file per request — `/images/inpaint` (file + mask) and a multi-file `/styles` call are out of reach for now.

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
| `{name}_post` | HTTP POST with a JSON body or a multipart image upload. Parameters: `path` (required, e.g. `/v1/ideogram-v3/generate`), `body` (JSON string, required unless `file_path` is given), `file_path`, `file_field` (default `image`), `form_fields`. See [Uploading files](#uploading-files). |

Default instructions describe available endpoints:

| Endpoint | Description |
|----------|-------------|
| `POST /v1/ideogram-v3/generate` | Generate image from text prompt. Body: `prompt`, `rendering_speed` (`FLASH`, `TURBO`, `DEFAULT`, `QUALITY`), `resolution`, `aspect_ratio`, `style_type` (`AUTO`, `GENERAL`, `REALISTIC`, `DESIGN`, `FICTION`), `magic_prompt`, `num_images`. |
| `POST /v1/ideogram-v3/remix` | Remix an existing image (multipart: image, prompt). |
| `POST /v1/ideogram-v3/edit` | Edit an image (multipart: image, mask, prompt). |
| `POST /v1/ideogram-v3/reframe` | Reframe/extend an image (multipart: image, resolution). |
| `POST /v1/ideogram-v3/replace-background` | Replace image background (multipart: image, prompt). |
| `POST /v1/ideogram-v3/describe` | Describe an image (multipart: image). |

The multipart endpoints take the image via `file_path` (sent as form field `image`); `prompt`, `resolution` and the like go into `form_fields`. One file per request — `/v1/ideogram-v3/edit` needs `image` and `mask` together and cannot be served yet.

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

### Hyperbrowser

Stealth headless Chrome with residential proxy rotation and CAPTCHA solving via
the Hyperbrowser API. Complements Firecrawl: use it when Firecrawl fails on
Anti-Bot-protected sites (Imperva, Cloudflare, DataDome) or JS-heavy SPAs that
need a real browser.

**Required fields**: `name`, `type`

**Optional fields**: `url` (default: `https://api.hyperbrowser.ai`), `instructions`

**Secret**: `{name}-token` in secret store (API key — sent as `x-api-key` header)

```toml
[[connections]]
name = "hyperbrowser"
type = "hyperbrowser"
```

**MCP tools exposed**:

Web-scraping (stateless):

| Tool | Description |
|------|-------------|
| `{name}_scrape` | Scrape a single URL. Blocks until done (max ~90s). Params: `url` (required), `formats` (csv: markdown/html/links/screenshot), `only_main_content`, `session_options`, `proxy_country`, `session_id`. |
| `{name}_extract` | Extract structured data with AI from one or more URLs. Blocks (max ~180s). Params: `urls` (csv, required), `prompt` and/or `schema` (JSON), `system_prompt`, `session_options`, `proxy_country`, `session_id`. |
| `{name}_crawl` | Start a multi-page crawl. Async — returns `jobId`. Params: `url` (required), `max_pages` (default 10, max 100), `follow_links`, `ignore_sitemap`, `formats`, `session_options`, `proxy_country`, `session_id`. |
| `{name}_crawl_status` | Poll a crawl job and retrieve results when completed. Param: `job_id` (required). |

Session lifecycle:

| Tool | Description |
|------|-------------|
| `{name}_session_create` | Start a persistent browser session. Returns `id` + `liveUrl` (VNC). Params: `session_options`, `proxy_country`, `timeout_minutes`, `profile_id`. Sessions are billed while active. |
| `{name}_session_stop` | Stop a running session. Param: `session_id` (required). |
| `{name}_session_list` | List all active sessions. No params. |

Browser-Use agent (LLM-driven navigation, for sites where direct scrape is blocked):

| Tool | Description |
|------|-------------|
| `{name}_browser_use_agent_start` | Start an agent task. Returns `jobId` + `liveUrl`. Params: `task` (required), `llm` (default gemini-2.0-flash), `session_id`, `keep_browser_open`, `max_steps`, `session_options`, `proxy_country`. Costs ~$0.10/run + LLM tokens. |
| `{name}_browser_use_agent_status` | Poll an agent task. Returns status, steps with model decisions/actions/page-state, and `data.finalResult`. **Screenshots are stripped server-side** to keep response sizes manageable. |

`session_options` is a comma-separated list of toggles: `use_stealth`,
`use_proxy`, `solve_captchas`, `accept_cookies`. Setting `proxy_country`
implicitly enables `use_proxy`. Proxy usage and CAPTCHA solving require a paid
Hyperbrowser plan and noticeably slow down requests.

`session_id` lets stateless tools (`scrape`, `extract`, `crawl`) reuse a
session previously started with `session_create` so cookies and storage
carry forward. Note: some sites (e.g. Imperva on ImmoScout24) profile
behaviour in real time and still block direct scrapes even with a warm
session — in those cases use the Browser-Use agent.

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

### YouTrack REST

Issues, comments, change history, attachments, agile boards and the administration API of a YouTrack instance over its REST API, shaped for a model: readable IDs, ISO 8601 timestamps in the token owner's profile time zone, custom fields as a name → value map, comments paged to completion, images as image content. Replaces the `youtrack` MCP-proxy alias for issue work and the `youtrack-agile` type for sprint queries (both keep working).

**Required fields**: `name`, `type`, `url` (the YouTrack base URL — `https://instance.youtrack.cloud` or `https://host/youtrack`; no `/api`, no `/mcp`)

**Optional fields**: `read_only` (default: false — reading tools only), `protected_fields`, `set_once_fields`, `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (Permanent Token; the tools act as that account)

Write guards are enforced in code for every client: `protected_fields` lists fields the connection never writes, `set_once_fields` fields it writes only while they are still empty. Entries read `Project:Field` or `Field` (every project).

```toml
[[connections]]
name = "youtrack-rest"
type = "youtrack-rest"
url = "https://instance.youtrack.cloud"
protected_fields = ["P:Sprint priority"]
set_once_fields = ["P:Department"]
instructions = "P = projects, T = tickets. State Review means waiting for feedback."
```

**MCP tools exposed** (reading):

| Tool | Description |
|------|-------------|
| `{name}_me` | Login, name, email and profile time zone of the account; whether the connection is read-only. |
| `{name}_search_issues` | Search with YouTrack query syntax. Parameters: `query` (required), `top` (≤ 200), `skip`, `include_description`. Compact issues with all custom fields. |
| `{name}_get_issue` | One issue in full: fields, tags, links by relation, attachments, watchers, all comments. Parameters: `issue` (required), `include_comments`, `include_description`. |
| `{name}_get_comments` | Comments with author, timestamps, visibility, attachments. Parameters: `issue` (required), `top`, `skip` (whole thread without them). |
| `{name}_get_activities` | Change history for one issue or a query. Parameters: `issue` or `query`, `categories`, `author`, `start`, `end`, `newest_first`, `top`, `skip`. |
| `{name}_get_attachment` | Attachment content: images ≤ 5 MB as image content, small text inline, otherwise saved to a file. Parameters: `issue`, `attachment` (id or name), `output_file`. |
| `{name}_list_projects` | Projects with id, short name, name, leader. Parameter: `include_archived`. |
| `{name}_get_project` | A project with its fields: type, required, allowed values (resolved states separately). Parameter: `project`. |
| `{name}_find_users` | Users by login, name or email fragment. Parameters: `query` (required), `top`. |
| `{name}_list_link_types` | Link types and the relation phrases `link_issues` accepts. |
| `{name}_list_boards` | Agile boards with projects and current sprint. |
| `{name}_get_current_sprint` | Current sprint of a board (ISO dates). Parameter: `board` (id or name). |
| `{name}_list_sprints` | Sprints of a board, newest first. Parameters: `board`, `top`. |

**MCP tools exposed** (when `read_only = false`):

| Tool | Description |
|------|-------------|
| `{name}_create_issue` | Parameters: `project`, `summary` (required), `description`, `fields` (name → value). |
| `{name}_update_issue` | Parameters: `issue` (required), `summary`, `description`, `fields` (name → value; `null` clears). |
| `{name}_add_comment` | Parameters: `issue`, `text` (required), `visibility_groups`, `visibility_users`. |
| `{name}_update_comment` | Edit a comment's text and/or visibility. Parameters: `issue`, `comment` (required), `text`, `visibility_groups`, `visibility_users` (at least one; a list left out keeps its entries, both lists empty lift the restriction). |
| `{name}_delete_comment` | Delete a comment; the result re-reads it and reports `deleted = true` once YouTrack no longer returns it. Parameters: `issue`, `comment` (required). |
| `{name}_attach` | Upload files streamed from disk, several per call. Parameters: `issue`, `file_paths` (required). Returns the full attachment list. |
| `{name}_watch` | Add watchers. Parameters: `issue`, `users` (required). |
| `{name}_link_issues` | Parameters: `issue`, `relation` (phrase as seen from issue, e.g. `parent for`), `target` (required). |
| `{name}_log_work` | Parameters: `issue`, `duration` (`90` or `1h 30m`; required), `date` (YYYY-MM-DD), `text`, `type`. |
| `{name}_run_command` | YouTrack command on one or more issues, the universal write path. Parameters: `issues`, `command` (required), `comment`, `silent`, `dry_run`. YouTrack parses the command first (`/commands/assist`); a command it does not understand, or one whose parsed form touches a guarded field — also as a bare value, an alias, or under the localized field name of a non-English instance — is refused. |
| `{name}_admin_get` | Raw GET on `/api/admin/…` or `/hub/api/rest/…`. Parameters: `path` (required), `output_file`. Paths are normalized before the prefix check; the issue subtree `/api/admin/projects/{id}/issues` is refused. |
| `{name}_admin_request` | Raw POST/PUT/PATCH/DELETE on `/api/admin/…` or `/hub/api/rest/…`. Parameters: `method`, `path` (required), `body`. Same path rules as `admin_get`, so the field guards cannot be bypassed through the administration API. |

Field values: strings for enum/state/version/build/owned/group values and user logins, arrays for multi-value fields, `YYYY-MM-DD` for date fields (stored as YouTrack does, 12:00 UTC), RFC 3339 or `YYYY-MM-DD HH:MM` in the profile time zone for date-and-time fields, numbers for integer/float, minutes or `1h 30m` for period fields, `null` to clear a field. Work item dates and sprint bounds are rendered in UTC, the zone YouTrack stores them in.

The endpoints and `fields=` selectors the type sends are listed in a registry and checked against YouTrack's published OpenAPI document (`/api/openapi.json`) by the test suite — vendored snapshot in `internal/tools/testdata/`, refreshed with `scripts/youtrack-openapi-snapshot.sh`; the `YouTrack API drift` workflow repeats the check weekly against the live document.

### IMAP

Connects to an IMAP mailbox with conversation threading (subject-based grouping). Supports MIME parsing, RFC 2047 encoded headers, and common mailbox operations. With an SMTP server configured and sending allowed, the same connection also sends mail.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default: 993), `from_name`, `smtp_host`, `smtp_port` (default: 587), `allow_send` (default: false), `smtp_saves_sent` (default: false), `tunnel`, `instructions`

**Secret**: `{name}-password` in secret store — used for IMAP and SMTP

```toml
[[connections]]
name = "mail"
type = "imap"
host = "imap.example.com"
port = 993
user = "user@example.com"
from_name = "Jane Doe"
smtp_host = "smtp.example.com"
allow_send = true
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_list_conversations` | List inbox conversations grouped by thread. Reads a window of the newest 200 messages — page backwards with `offset` (messages to skip: 200, 400, …) until the count from `list_mailboxes` is reached. Returns latest message, participants, and message count. Parameters: `limit` (optional), `offset` (optional). |
| `{name}_get_conversation` | Get all messages in a conversation thread with full body text. Parameter: `conversation_id` (required). |
| `{name}_search_messages` | Search messages by text, grouped by conversation. Parameters: `query` (required), `limit` (optional). |
| `{name}_list_mailboxes` | List all IMAP mailbox folders with `messages` and `unseen` counts. |
| `{name}_archive_conversation` | Archive a conversation (move to Archive or Trash). Parameter: `conversation_id` (required). |
| `{name}_delete_conversation` | Delete a conversation (move to Trash, not permanent). Parameter: `conversation_id` (required). |
| `{name}_create_reply_draft` | Create a reply draft for the latest message. Saved to Drafts, does NOT send. The reply goes to the message's `Reply-To` (its sender when it has none). Returns `draft_id`. Parameters: `conversation_id`, `body` (required), `reply_all` (optional: also everyone else on To and Cc). |
| `{name}_create_forward_draft` | Create a forward draft. Saved to Drafts, does NOT send. Returns `draft_id`. Parameters: `conversation_id`, `to` (required, comma-separated for several), `body` (optional). |
| `{name}_send_draft` | *Only with `allow_send`.* Send a draft over SMTP. Recipients are the draft's To, Cc and Bcc; Bcc is not transmitted. The draft is removed, a copy stored in Sent. Parameter: `draft_id` (required). |
| `{name}_reply_send` | *Only with `allow_send`.* Reply and send in one step — no draft. Parameters as `create_reply_draft`. |
| `{name}_forward_send` | *Only with `allow_send`.* Forward and send in one step — no draft. Parameters as `create_forward_draft`. |

**Drafts and Sent.** mux finds both folders by their special-use attribute (`\Drafts`, `\Sent`, RFC 6154) and falls back to common names (`Drafts`, `INBOX.Drafts`, `Entwürfe` … and `Sent`, `INBOX.Sent`, `Sent Items`, `Gesendet` …) when the server marks none. `draft_id` is the Message-ID mux gives the draft; `send_draft` finds the draft by it, so it also finds a draft another client has edited as long as that client keeps the Message-ID. Two drafts with the same Message-ID are refused rather than guessed between.

**Sending.** `allow_send` is the switch, `smtp_host` its precondition: `smtp_host` alone registers no send tool, and `allow_send` without `smtp_host` fails the connection with a message saying so. mux signs in to the SMTP server with the IMAP user and password — implicit TLS on port 465, STARTTLS on any other port (a server without STARTTLS is refused; the password never travels unencrypted), AUTH PLAIN or LOGIN. The **Test** button signs in to both servers. After the server has accepted a message, mux stores a copy in Sent (skip it with `smtp_saves_sent = true` for providers such as Gmail that do this themselves) and, for `send_draft`, removes the draft; a failure there is reported as a warning next to `sent: true`, never as an error, so an agent does not send twice. An error that leaves open whether the server accepted the message says so (`unclear whether sent`).

**Encoding.** mux writes plain-text mail with CRLF line endings, RFC 2047 encoded headers and the display name from `from_name`. A body with non-ASCII text is quoted-printable, so it passes servers without 8BITMIME; a draft written by another client with 8-bit content is refused by such a server before anything is sent.

**Removing messages.** Archive, delete and the draft cleanup after sending flag the messages `\Deleted` and remove exactly those with `UID EXPUNGE` where the server supports UIDPLUS. Without it, `EXPUNGE` also removes other messages in that folder that are already flagged `\Deleted` — the only means plain IMAP has.

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

Connects to an upstream MCP server, discovers its tools, and re-exposes them with a `{name}_` prefix. Supports Bearer token, custom-header, HTTP Basic, and OAuth 2.0 + PKCE authentication.

**Required fields**: `name`, `type`, `url`

**Optional fields**: `oauth` (default: false), `token_scheme`, `basic_suffix`, `token_header`, `headers`, `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (token auth) or `{name}-oauth-token` (OAuth)

Auth precedence for the `{name}-token` secret:

- **Bearer** (default) — `Authorization: Bearer {token}`.
- **`token_scheme = "basic"`** — `Authorization: Basic base64({token}:{basic_suffix})`. The token is the Basic *username* and `basic_suffix` a fixed constant, which is the common API-token-as-Basic idiom (Graylog uses `token`, GitHub `x-oauth-basic`, Stripe an empty value). This lets a user supply just their **raw token** — mux does the base64 encoding — so it is the right choice for a per-user token on a provisioned connection.
- **`token_header = "X"`** — sends the token verbatim under header `X` (e.g. `x-goog-api-key`).

`headers` adds extra static headers on every request; the token header wins over a static header of the same name.

When `tunnel` is set, the upstream connection is dialed through that tunnel (WireGuard or SSH) — required for MCP servers that are only routable on an internal network. This is fail-closed: if the tunnel is unavailable the proxy is skipped rather than connected directly.

```toml
# Bearer token proxy
[[connections]]
name = "youtrack"
type = "proxy"
url = "https://instance.myjetbrains.com/mcp"

# Basic-auth proxy over an internal tunnel — each user sets only their raw token
# via: secret_set {name}-token = "<raw access token>"
[[connections]]
name = "internal-logs"
type = "proxy"
url = "https://logs.internal.example.com/api/mcp"
token_scheme = "basic"
basic_suffix = "token"
tunnel = "office-vpn"

# OAuth proxy
[[connections]]
name = "sentry"
type = "sentry"
url = "https://mcp.sentry.dev/mcp"
```

**MCP tools exposed**: All tools from the upstream server, prefixed with `{name}_`.

URLs ending in `/sse` use SSE transport; everything else uses Streamable HTTP.

OAuth proxies need a one-time login in a browser. The desktop app starts it with **Authorize** on the connection card; a headless instance has no UI, so open `http://localhost:7700/oauth/start?connection={name}` yourself (adjust the port). Either way the provider redirects back to `/oauth/callback` on the local port, the token is stored as `{name}-oauth-token`, and the connection mounts without a restart.

Proxy-type aliases (`youtrack`, `sentry`, `netdata`, `notion`, `asana-mcp`, `higgsfield`) behave identically to `proxy` but provide semantic clarity. `youtrack` proxies JetBrains' built-in YouTrack MCP server; for issue work prefer the native [YouTrack REST](#youtrack-rest) type. `sentry` and `notion` additionally default `oauth = true`. `asana-mcp` defaults `oauth = true` and URL to `https://mcp.asana.com/v2/mcp`. `higgsfield` defaults `oauth = true`, URL to `https://mcp.higgsfield.ai/mcp`, and scopes to `openid email offline_access` (the `offline_access` scope obtains a refresh token, so the proxy keeps working without re-authentication).

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

**Optional fields**: `read_only` (default: false), `token_scheme`, `basic_suffix`, `token_header`, `headers`, `tunnel`, `instructions`

**Secret**: `{name}-token` in secret store (optional auth token)

By default, the token is sent as `Authorization: Bearer {token}`. Set `token_header` to use a custom header name instead (e.g. `x-goog-api-key` for Google APIs). Set `token_scheme = "basic"` to send `Authorization: Basic base64({token}:{basic_suffix})` — the token is the Basic username (see the MCP Proxy section for the idiom).

Use `headers` for extra headers the API requires on every request, e.g. API version headers. The token header always wins over a custom header of the same name.

```toml
[[connections]]
name = "internal-api"
type = "http"
url = "https://api.internal.example.com"
token_header = "x-api-key"
instructions = "Internal product API. Use /api/v1/products to list products."

[connections.headers]
"Notion-Version" = "2022-06-28"
```

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_get` | HTTP GET request. Parameters: `path` (required), `output_file` (optional — save response to file). |
| `{name}_request` | HTTP request with body (POST, PUT, PATCH, DELETE). Parameters: `path`, `method`, `body` (JSON), `output_file`, and for uploads `file_path`, `file_field` (default `file`), `form_fields`. Available unless `read_only = true`. |

Both tools support an optional `output_file` parameter. When set, the response body is streamed to the specified file path (no size limit) and only metadata (status, content-type, path, size) is returned. Useful for large responses like images or binary data.

#### Uploading files

`{name}_request` — and the `request`/`post` tools of the OpenAI, Recraft and Ideogram connectors — can send a local file as `multipart/form-data`. This is the counterpart of `output_file`: the model names a path, mux moves the bytes, and the file never passes through the conversation.

- `file_path` — absolute path of the file (supports `~`). Setting it switches the request to multipart; `body` is not allowed in this mode.
- `file_field` — form field the file is sent under (default `file`; Ideogram uses `image`).
- `form_fields` — the other text parts as a JSON object of scalars, e.g. `{"title": "Banner", "public": true}`. Numbers and booleans are sent as their JSON text; `null` and nested values are rejected.

The file is streamed from disk with an exact `Content-Length`; the part's `Content-Type` comes from the file extension, or from the first bytes when the extension is unknown. The result reports what went out (`Uploaded banner.png as "file": 70123 bytes, image/png`). Auth headers and redirect handling are the same as for JSON requests, and a 307/308 replays the body from disk; the one difference is `Content-Type`, where a header configured on the connection gives way to the multipart boundary.

Uploads run on their own time budget of 30 minutes instead of the 30–60 seconds a JSON round trip gets, so a large file on a slow link is not cut off (100 MB at 1 MB/s take under two minutes). The budget exists for one case: a server that accepts the connection and then stops reading. No other timer covers that — the transport's response-header timeout (30 s) starts only once the body has been written in full, and the stdio transport has no per-request deadline that would otherwise end the call. When it fires, the tool result says so in plain words (`upload aborted after 30m0s: … did not finish taking N bytes of …`) rather than Go's "awaiting headers", and mux logs the abort under `[upload]`. There is no size limit on the mux side; the target API enforces its own.

The path is resolved by the mux process, not by the caller: in a container the file has to be inside a mounted directory (see [container.md](container.md#configuration)). One file per request; `form_fields` without `file_path` is an error rather than a form-encoded body.

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

## Host Connections

### SSH

Commands and file copies on a remote host, with the credential held by mux. The agent gets exit codes, output and copied files; it never sees the private key — the same arrangement as a database connection, where the agent sees query results and not the password. Every operation is logged.

**Required fields**: `name`, `type`, `host`, `user`

**Optional fields**: `port` (default 22), `auth` (`key` default, or `agent`), `key_file`, `host_key`, `read_only`, `allow_commands`, `deny_commands`, `tunnel`, `instructions`

**Secrets**: `{name}-private-key` (the private key in PEM form, may be passphrase-protected), `{name}-private-key-passphrase` (when it is)

```toml
[[connections]]
name = "web01"
type = "ssh"
host = "web01.internal.example.com"
user = "deploy"
read_only = true
host_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI… web01"
deny_commands = ["journalctl --vacuum*"]
instructions = "Production web server. Logs live in /var/log/nginx; the app runs as the 'app' systemd unit."
# Private key stored as secret (key: "web01-private-key"), its passphrase as "web01-private-key-passphrase"
```

**Key sources** (`auth`):

- `key` (default) — mux holds the private key: the secret `{name}-private-key`, or the file named by `key_file`. An encrypted key stays encrypted at rest; mux decrypts it in memory with `{name}-private-key-passphrase`. Where the key and the passphrase sit in the same file store (a container without keychain or vault), the encryption buys little; it buys something when the passphrase sits in the keychain or vault and the key in a file.
- `agent` — mux signs through the ssh-agent of the user it runs as (`SSH_AUTH_SOCK`, a Unix socket). No key material enters mux; on a desktop this is the unlocked agent of the logged-in user. Not available in a container, and not on Windows, where the agent is a named pipe.

**Host keys** are verified on every connection; there is no insecure switch on this type. `host_key` pins one public key (authorized_keys or known_hosts line form) — the form a provisioning server uses when it knows its hosts. Without it, mux consults `known_hosts` in its config directory (`~/.config/mux/known_hosts`) and then `~/.ssh/known_hosts`, re-reading them at every connection attempt; lines the parser cannot read are skipped with a log line, as OpenSSH does. An unknown host fails with the exact line to add, and the next attempt sees it; a changed key fails as a mismatch. The key types a file already knows for the host are offered first in the handshake, as OpenSSH does — otherwise a host known by its Ed25519 key would present its ECDSA key and fail as a mismatch.

**Command policy.** `allow_commands` and `deny_commands` are lists of leading tokens: `cat` covers `cat /etc/hosts`; `systemctl status` covers `systemctl status nginx` and not `systemctl restart nginx`. Tokens take the wildcards `*` and `?` (`journalctl --vacuum*`). The program is matched as written — `cat` does not cover `/bin/cat`, and not `/tmp/x/cat` either; a connection that may run a full path lists it. Deny entries always win, and `shell=true` is refused as soon as either list exists. A read-only connection without `allow_commands` runs the built-in list — `ls cat head tail grep stat wc file df du free uptime ps uname id whoami date`, `systemctl status|is-active|list-units`, `journalctl`, `docker ps|logs|inspect`, `podman ps|logs|inspect` — minus `journalctl --vacuum*`, `--rotate`, `--flush`, `--relinquish-var` and `date -s*`, `date --set*`. Programs that run other programs (`env`, `nice`, `timeout`, `xargs`) are deliberately not on it. Setting `allow_commands` replaces the built-in list. A connection that is neither read-only nor has lists runs anything. The lists express the operator's intent and hold for every client; they are not a sandbox — `cat` as root reads every file, `tail -f` runs until the timeout, and on a connection that also has `upload`, an allow list is a convenience, not a confinement.

**MCP tools exposed**:

| Tool | Description |
|------|-------------|
| `{name}_exec` | Run one program. Parameters: `args` (required, the argument vector — program first), `cwd`, `stdin`, `timeout` (seconds; default 60, at most 1800), `output_file` (stdout to a local file), `shell` (pass `args[0]` to the remote shell verbatim; only on a connection without `allow_commands`, without `deny_commands` and not read-only), `dry_run` (show the command line and the policy verdict, run nothing). Returns exit code, duration and sizes, then stdout and stderr — inline up to 64 KB / 16 KB with an explicit truncation note, or stdout in the file. A non-zero exit is a result, not an error. On a timeout mux sends SIGKILL, closes the session and reports the partial output; OpenSSH 7.9+ without a forced command kills the program on that signal, older servers and forced-command sessions ignore it and may leave the program running. |
| `{name}_download` | Copy a remote file to this machine over SFTP. Parameters: `remote_path`, `output_file` (both required). Streamed to disk; an existing local file is never overwritten. |
| `{name}_upload` | Copy a local file to the host over SFTP. Parameters: `file_path`, `remote_path` (required), `mode` (octal, default `0644`), `overwrite` (default false — an existing remote file is an error). Streamed from disk; a failed copy removes what it created, and says so when it could not (connection gone). Absent when `read_only = true`. |

Transfers run under the same 30-minute budget as HTTP uploads. The connection is probed with a keepalive every 30 seconds while idle; a probe that goes unanswered while no data arrived for that long marks the connection dead, so a transfer slower than about 1 KB/s can be cut by it.

Arguments are sent as literals: an SSH exec request carries one string, so mux quotes every element of `args` for a POSIX shell and the remote shell splits it back into exactly that list. `cwd` becomes a `cd` in front of the command. `stdin` is written to the program and closed.

**Audit.** Each command and copy — and each refusal — produces one line in the mux log (`[ssh] <connection> exec "<command>" exit=0 dur=120ms bytes=512`) and an entry in `GET /audit/ssh` on the loopback listener, which answers the newest entries first (default 100, `?limit=` up to the ring's 500; `?connection=` filters). The endpoint is not on the TLS listener. The command line is logged as sent — a secret passed as an argument lands in the audit trail.

**Through a tunnel.** `tunnel = "bastion"` routes the SSH connection over a WireGuard or SSH tunnel like any other connection type.

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
