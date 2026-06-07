# Higgsfield

Cinematic image and video generation (Soul, Kling, Seedance, Veo, and more) via the hosted Higgsfield MCP server.

## Prerequisites

- A Higgsfield account with available credits.
- Authentication uses OAuth — no manual token required. Dynamic client registration means there is nothing to pre-register.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the Higgsfield MCP endpoint. Default: `https://mcp.higgsfield.ai/mcp`. |

## After creating

1. Click **Authorize** to start the OAuth flow.
2. You will be redirected to Higgsfield — sign in and grant the requested permissions.
3. After successful authorization, the browser returns to mux automatically.
4. Click **Test** to verify the connection.

## Notes

- Default scopes are `openid email offline_access`. The `offline_access` scope obtains a refresh token, so the connection keeps working without re-authorization — important for headless and long-running sessions.
- OAuth tokens are stored in the system Keychain and refreshed automatically.
- Generations are billed against your Higgsfield account credits, based on the model and resolution.
- If tools do not appear: make sure mux reloaded the connection after authorization.
