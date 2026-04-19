# Google Workspace

## Prerequisites

- A local [google_workspace_mcp](https://github.com/taylorwilsdon/google_workspace_mcp) server.
- OAuth credentials from the Google Cloud Console.
- Python with [uv](https://docs.astral.sh/uv/) installed.

### Google Cloud setup

1. Open the [Google Cloud Console](https://console.cloud.google.com/).
2. Create a project or select an existing one.
3. Enable the required APIs (Gmail, Drive, Calendar, Docs, Sheets, Tasks) under **APIs & Services → Library**.
4. Create OAuth credentials under **APIs & Services → Credentials → Create Credentials → OAuth client ID** (type: Desktop App).
5. Copy the **Client ID** and **Client Secret**.
6. Configure the OAuth consent screen and add your Google account as a test user.

### Install and run the MCP server

```bash
git clone https://github.com/taylorwilsdon/google_workspace_mcp.git
cd google_workspace_mcp
uv sync

export GOOGLE_OAUTH_CLIENT_ID="your-client-id"
export GOOGLE_OAUTH_CLIENT_SECRET="your-client-secret"
uv run main.py --transport streamable-http --tool-tier extended
```

On first start, a browser window opens for Google sign-in.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the local MCP server. Default: `http://localhost:8000/mcp`. |

## After creating

Click **Test** to verify the connection to the local server.

## Notes

- The google_workspace_mcp server must be running before mux can use the connection.
- On port conflicts, start the server on a different port (`--port 8001`) and adjust the URL in mux accordingly.
- For details see `docs/google-workspace-setup.md` in the mux repo.
