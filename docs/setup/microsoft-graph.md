# Microsoft Graph

## Prerequisites

- A Microsoft 365 account (personal or organizational).
- No app registration required — mux uses a device code flow with a preconfigured app.

## Fields

| Field | Description |
|------|-------------|
| Scopes (optional) | OAuth scopes, separated by spaces. Default: `Mail.ReadWrite Mail.Send offline_access`. Extend the scopes if you need access to Calendar, OneDrive, etc. |

## After creating

1. Click **Authenticate**.
2. mux displays a **user code** and a link to `microsoft.com/devicelogin`.
3. Open the link in your browser, enter the code, and sign in with your Microsoft account.
4. Grant the requested permissions.
5. Return to mux — authentication is detected automatically.
6. Click **Test** to verify the connection.

## Notes

- The device code flow is particularly suited to scenarios where a browser redirect is not possible.
- Tokens are stored in the system Keychain and refreshed automatically.
- If your organization's admin restricts third-party apps, you will need admin approval for the requested scopes.
- Commonly used scopes: `Mail.Read`, `Mail.ReadWrite`, `Mail.Send`, `Calendars.ReadWrite`, `Files.ReadWrite`, `offline_access`.
