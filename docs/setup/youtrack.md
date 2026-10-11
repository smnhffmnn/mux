# YouTrack (MCP proxy)

Proxies JetBrains' built-in YouTrack MCP server. For issue work — attachments, change history, full comment threads, time-zone-correct dates, write guards — use the **YouTrack REST** type instead; this type stays for the tools only the MCP server offers.

## Prerequisites

- A YouTrack instance with MCP support enabled (YouTrack Cloud or Server with the MCP plugin).
- A Permanent Token with the required permissions.

### Creating a token

1. Open YouTrack → **Profile → Account Security → Tokens**.
2. Click **New Token**.
3. Select the required scopes (at least read access to issues).
4. Copy the token — it is shown only once.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the YouTrack MCP endpoint (e.g. `https://instance.myjetbrains.com/mcp`). |
| Token | Permanent Token. Stored in the Keychain. |

## After creating

Click **Test** to verify the connection. mux displays the server name and the available tools.

## Notes

- The MCP URL typically has the format `https://<instance>.myjetbrains.com/mcp`.
- For self-hosted instances the path may differ.
