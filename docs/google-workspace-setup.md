# Google Workspace Setup Guide

Complete setup for connecting mux to Google Workspace (Gmail, Drive, Docs, Sheets, Calendar, Tasks) via [google_workspace_mcp](https://github.com/taylorwilsdon/google_workspace_mcp).

## Architecture

```
Claude ↔ mux ↔ google_workspace_mcp (local) ↔ Google APIs
```

mux connects to the google_workspace_mcp server running locally. That server handles all Google OAuth 2.0 authentication. No Google credentials are stored in mux — only the URL to the local MCP server.

## 1. Google Cloud Console Setup

### Create OAuth Credentials

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Create a new project (or select an existing one)
3. Enable the following APIs under **APIs & Services → Library**:
   - Gmail API
   - Google Drive API
   - Google Calendar API
   - Google Docs API
   - Google Sheets API
   - Google Tasks API
4. Go to **APIs & Services → Credentials**
5. Click **Create Credentials → OAuth client ID**
6. Application type: **Desktop app** (not Web application)
7. Name it (e.g. "mux Google Workspace")
8. Copy the **Client ID** and **Client Secret**

### Configure OAuth Consent Screen

If prompted to configure the consent screen:

1. Choose **External** (or Internal if using Google Workspace org)
2. Fill in the required fields (app name, support email)
3. Add scopes — select the APIs you enabled above
4. Add your Google account as a **test user** (required while app is in "Testing" status)
5. Save

> **Note:** While the app is in "Testing" status, only test users can authenticate. This is fine for personal use — no need to publish the app.

## 2. Install & Run google_workspace_mcp

### Install

```bash
git clone https://github.com/taylorwilsdon/google_workspace_mcp.git
cd google_workspace_mcp
uv sync
```

Requires [uv](https://docs.astral.sh/uv/) (Python package manager). Install with `brew install uv` or `curl -LsSf https://astral.sh/uv/install.sh | sh`.

### Configure

Set the OAuth credentials as environment variables:

```bash
export GOOGLE_OAUTH_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export GOOGLE_OAUTH_CLIENT_SECRET="your-client-secret"
```

For persistence, add these to your shell profile (`~/.zshrc`, `~/.bashrc`, `~/.config/fish/config.fish`).

### Start the Server

```bash
uv run main.py --transport streamable-http --tool-tier extended
```

- `--transport streamable-http` — required for mux (exposes HTTP endpoint on port 8000)
- `--tool-tier extended` — enables all available tools (Gmail, Drive, Docs, Sheets, Calendar, Tasks)

### First-Time Authentication

On first start, the server opens a browser window for Google OAuth consent. Sign in with your Google account and grant the requested permissions. The token is saved locally by the server and refreshed automatically.

### Optional: Restrict Services

To limit which Google services are available, use the `--enabled-services` flag:

```bash
uv run main.py --transport streamable-http --tool-tier extended \
  --enabled-services gmail drive calendar
```

Available services: `gmail`, `drive`, `docs`, `sheets`, `calendar`, `tasks`.

### Keep It Running

The google_workspace_mcp server must be running whenever mux needs to access Google services. Options:

- Run in a terminal tab
- Use a process manager (e.g. `pm2`, `supervisord`)
- Create a launchd plist (macOS) or systemd service (Linux)

## 3. Configure mux

Add the connection to `~/.config/mux/config.toml`:

```toml
[[connections]]
name = "google-workspace"
type = "google-workspace"
url = "http://localhost:8000/mcp"
```

Or add it at runtime via MCP tool:

```
connection_add name=google-workspace type=google-workspace url=http://localhost:8000/mcp
```

Restart mux (or it picks up the new connection automatically if added via `connection_add`).

### Verify

After adding, the Google Workspace tools should appear with the prefix `google-workspace_`:

```
google-workspace_gmail_search_emails
google-workspace_gmail_send_email
google-workspace_drive_search_files
google-workspace_calendar_list_events
google-workspace_tasks_list_tasklists
...
```

The exact tool names depend on the google_workspace_mcp version and configuration.

## 4. Troubleshooting

### "Connection failed" or tools not appearing

The google_workspace_mcp server must be running and accessible at the configured URL before mux tries to connect. If mux started first:

1. Start google_workspace_mcp
2. Remove and re-add the connection in mux, or restart mux

### OAuth token expired

If tools return authentication errors:

1. Stop google_workspace_mcp
2. Delete the stored token (typically `~/.google_workspace_mcp/token.json` or similar)
3. Restart the server — it will re-prompt for OAuth consent

### Permission errors on specific tools

The available tools depend on the OAuth scopes granted during setup. If a tool returns a scope/permission error, re-authenticate with broader scopes or check which APIs are enabled in Google Cloud Console.

### Port conflict on 8000

If port 8000 is already in use, start google_workspace_mcp on a different port:

```bash
uv run main.py --transport streamable-http --tool-tier extended --port 8001
```

Then update the mux connection URL accordingly:

```toml
url = "http://localhost:8001/mcp"
```
