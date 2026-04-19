# Netdata

## Prerequisites

- A Netdata Cloud account.
- An API token for access.

### Creating a token

1. Open [Netdata Cloud](https://app.netdata.cloud/).
2. Go to **User Settings → API Tokens**.
3. Create a new token and copy it.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the Netdata MCP endpoint. Default: `https://app.netdata.cloud/api/v1/mcp`. |
| Token | API token. Stored in the Keychain. |

## After creating

Click **Test** to verify the connection.

## Notes

- The token has the format `ndc.xxx`.
- Netdata Cloud is required — local Netdata agents are not directly supported.
