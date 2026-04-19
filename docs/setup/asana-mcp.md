# Asana MCP

## Prerequisites

- An Asana account.
- Authentication uses OAuth — no manual token required.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the Asana MCP endpoint. Default: `https://mcp.asana.com/v2/mcp`. |

## After creating

1. Click **Authorize** to start the OAuth flow.
2. You will be redirected to Asana — sign in and grant the requested permissions.
3. After successful authorization, the browser returns to mux automatically.
4. Click **Test** to verify the connection.

## Notes

- Asana MCP exposes the full Asana feature set over the MCP protocol.
- For plain REST API access with a PAT, use the **Asana** type instead.
- OAuth tokens are stored in the system Keychain and refreshed automatically.
