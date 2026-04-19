# Sentry

## Prerequisites

- A Sentry account (SaaS or self-hosted).
- Authentication uses OAuth — no manual token required.

## Fields

| Field | Description |
|------|-------------|
| MCP URL | URL of the Sentry MCP endpoint. Default: `https://mcp.sentry.dev/mcp`. |

## After creating

1. Click **Authorize** to start the OAuth flow.
2. You will be redirected to Sentry — sign in and grant the requested permissions.
3. After successful authorization, the browser returns to mux automatically.
4. Click **Test** to verify the connection.

## Notes

- OAuth tokens are stored in the system Keychain and refreshed automatically.
- For self-hosted Sentry instances, adjust the MCP URL accordingly.
- If authorization fails: check whether third-party apps are allowed in your Sentry organization.
