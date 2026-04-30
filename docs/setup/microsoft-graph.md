# Microsoft Graph

## Prerequisites

- A Microsoft 365 account (personal or organizational).
- An **Azure App Registration** in your own tenant (or a multi-tenant one you control).
  mux no longer ships a built-in Client ID — every connection points at the app
  registration *you* own so that tokens, audit logs, and consent are attributed
  to you rather than to the mux project.

## Creating the Azure App Registration

1. Open the [Azure Portal → App registrations](https://portal.azure.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade)
   and click **New registration**.
2. **Name:** anything you like (e.g. `mux`).
3. **Supported account types:** choose based on where the signed-in users live
   (single-tenant, multi-tenant, or `Personal Microsoft accounts only`).
4. **Redirect URI:** leave blank — the device code flow does not need one.
5. After creation, copy the **Application (client) ID** from the Overview page —
   this is the value that goes in the `Client ID` connection field.
6. Under **Authentication**, enable **Allow public client flows** (required for
   the device code flow).
7. Under **API permissions**, add the Microsoft Graph permissions you need.
   For mux's default tools you need `User.Read`, `Mail.Read`, `Mail.ReadWrite`,
   `Mail.Send`, `Calendars.ReadWrite`, `Calendars.Read.Shared`, and
   `offline_access`. Add `Files.ReadWrite` or others if you plan to override
   `Scopes` to use additional tools. Grant admin consent if your tenant
   requires it.

## Fields

| Field | Description |
|------|-------------|
| **Client ID** (required) | Application (client) ID of your Azure App Registration. |
| Scopes | Space-separated OAuth scopes. Default: `User.Read Mail.Read Mail.ReadWrite Mail.Send Calendars.ReadWrite Calendars.Read.Shared offline_access` (mail + calendar). Override this if your app grants additional permissions (Files, Teams, etc.) and you want them in the issued token. |

## After creating

1. Click **Authenticate**.
2. mux displays a **user code** and a link to `microsoft.com/devicelogin`.
3. Open the link in your browser, enter the code, and sign in with your Microsoft account.
4. Grant the requested permissions.
5. Return to mux — authentication is detected automatically.
6. Click **Test** to verify the connection.

## Notes

- The device code flow is suited to scenarios where a browser redirect is not
  possible.
- Tokens are stored in the system Keychain and refreshed automatically.
- **Changing the Client ID** on an existing connection drops all stored OAuth
  tokens — they were issued against the old app registration and would no
  longer work. You will need to re-authenticate afterwards.
- **Scopes are bounded by the app registration.** The OAuth `scope` parameter
  requests a subset of the permissions your Azure app grants; you cannot
  obtain tokens for permissions the app does not have, regardless of what you
  put in this field.
- Commonly used scopes: `Mail.Read`, `Mail.ReadWrite`, `Mail.Send`,
  `Calendars.ReadWrite`, `Files.ReadWrite`, `offline_access`.
