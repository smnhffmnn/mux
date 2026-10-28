# IMAP

## Prerequisites

- An email account with IMAP access.
- IMAP must be enabled at the provider (disabled by default on some providers).
- For accounts with two-factor authentication: generate an app-specific password.
- For sending: the provider's SMTP submission server (often `smtp.<domain>`; the provider's mail client settings list it).

## Fields

| Field | Description |
|------|-------------|
| IMAP Host | Hostname of the IMAP server (e.g. `imap.gmail.com`, `outlook.office365.com`). |
| Port | Default: `993` (IMAPS/TLS). |
| Email / Username | Email address or username. |
| Password | Password or app-specific password. Stored in the Keychain. Used for IMAP and SMTP. |
| Sender Name | Optional. Display name in the From header of drafts and sent mail. |
| SMTP Host | Optional. Submission server for sending (e.g. `smtp.gmail.com`). Signs in with the same user and password. |
| SMTP Port | Default: `587` (STARTTLS). `465` is implicit TLS. |
| Allow sending | Registers the send tools (`send_draft`, `reply_send`, `forward_send`). Needs SMTP Host and an email address as Email / Username (it is the sender); SMTP Host alone does not enable sending. |
| Server files sent mail itself | Tick for providers that put mail sent over SMTP into the Sent folder themselves (Gmail), so mux does not store a second copy. |

## After creating

Click **Test** to verify the TLS connection and the login. With sending allowed, the test signs in to the SMTP server as well.

## Notes

- mux connects to IMAP exclusively over TLS (port 993). SMTP runs over TLS too: implicit on port 465, STARTTLS on any other port — a server without STARTTLS is refused.
- Without **Allow sending**, the connection only creates drafts; nothing leaves the mailbox.
- Gmail: generate an app password under Google Account → Security. SMTP: `smtp.gmail.com`, tick **Server files sent mail itself**.
- Microsoft 365: IMAP must be enabled in the admin center; alternatively, use the **Microsoft Graph** type.
