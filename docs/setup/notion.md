# Notion

## Prerequisites

- A Notion account.
- Authentication uses OAuth — no manual token required.
- The AI query tools (`query-data-sources`, `query-database-view`) require a
  **Notion Business plan or higher with Notion AI** — Notion gates them
  server-side. Reading, searching, and creating pages works on every plan.
  On lower plans, query databases via the Notion REST API instead: create an
  internal integration and add an **HTTP API** connection to
  `https://api.notion.com` with a `Notion-Version` header.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the Notion MCP endpoint. Default: `https://mcp.notion.com/mcp`. |

## After creating

1. Click **Authorize** to start the OAuth flow.
2. You will be redirected to Notion — select the workspace and the pages mux is allowed to access.
3. After successful authorization, the browser returns to mux automatically.
4. Click **Test** to verify the connection.

## Notes

- You can restrict access to specific pages and databases when Notion prompts you during authorization.
- OAuth tokens are stored in the system Keychain and refreshed automatically.
- If tools do not appear: make sure mux reloaded the connection after authorization.
