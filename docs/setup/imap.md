# IMAP

## Prerequisites

- An email account with IMAP access.
- IMAP must be enabled at the provider (disabled by default on some providers).
- For accounts with two-factor authentication: generate an app-specific password.

## Fields

| Field | Description |
|------|-------------|
| IMAP Host | Hostname of the IMAP server (e.g. `imap.gmail.com`, `outlook.office365.com`). |
| Port | Default: `993` (IMAPS/TLS). |
| Email / Username | Email address or username. |
| Password | Password or app-specific password. Stored in the Keychain. |

## After creating

Click **Test** to verify the TLS connection to the server.

## Notes

- mux connects exclusively over TLS (port 993).
- Gmail: generate an app password under Google Account → Security.
- Microsoft 365: IMAP must be enabled in the admin center; alternatively, use the **Microsoft Graph** type.
