# MCP Proxy (generic)

## Prerequisites

- A running MCP server that supports the Streamable HTTP protocol.
- The URL of the MCP endpoint.
- If the server requires authentication: a Bearer token or OAuth support.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | Full URL of the MCP endpoint (e.g. `https://example.com/mcp`). |
| Token | Bearer token for authentication. Stored in the Keychain. Leave empty when using OAuth. |

## After creating

1. For OAuth: enable OAuth on the connection and click **Authorize**.
2. Click **Test** to verify the connection. mux displays the server name and the number of available tools.

## Notes

- The generic proxy type is suitable for any MCP server not covered by a dedicated type.
- Tools from the proxied server are registered with the connection name as a prefix (e.g. `myproxy_tool_name`).
