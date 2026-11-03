# Nylas

## Prerequisites

- A Nylas account with an application in the region where the mailbox data should live (US or EU — the two regions are separate; a key from one does not open grants in the other).
- A connected mailbox (a **grant**) in that application: Gmail and Microsoft 365 over OAuth, other providers over IMAP (Dashboard → Grants, or the hosted auth page). IMAP grants hand Nylas the mailbox password; Nylas keeps a rolling 90-day cache of such mailboxes.
- An API key. For production use, create an **IAM key** bound to the grant (or the application) with an expiry instead of the application-wide key — see [Nylas IAM](https://developer.nylas.com/docs/v3/auth/nylas-iam/).

## Fields

| Field | Description |
|------|-------------|
| Region | `us` (default) or `eu` — the data-residency region of the Nylas application. |
| Grant ID | The id of the connected mailbox (UUID shown on the grant in the Dashboard). |
| Mailbox Address (optional) | The mailbox's address; when Grant ID is empty mux looks the grant up by it (needs a key that may list the application's grants). Also the account's own address for the reply logic — otherwise read from the grant. |
| API Key | The Nylas API key or IAM key, sent as Bearer token. Stored in the Keychain. |
| Sender Name | Optional. Display name in the From header of drafts and sent mail. |
| Allow sending | Registers the send tools (`send_draft`, `reply_send`, `forward_send`). Nylas sends through the provider; nothing else is needed. |

## After creating

Click **Test**. mux reads the grant (it must be `valid`) and lists the folders — the two calls every tool starts with. A key for the wrong region, an expired key or a revoked grant shows up here.

## Notes

- Conversations are Nylas threads and include the account's own replies, so each conversation carries `ball`: `"me"` when the other side wrote last, `"them"` when this account wrote last.
- Gmail: archiving removes the INBOX label (the mail stays in All Mail), moving swaps the source label for the target; other labels stay.
- IMAP-backed accounts: Nylas syncs a rolling 90-day window. Conversations outside it are not listed; `search_messages` with `live=true` asks the IMAP server directly and reaches older mail (slow, one folder at a time).
- Nylas also runs a hosted MCP server. mux talks to the REST API instead: the API is versioned and documented (OpenAPI), and it has what the MCP server lacks — archive, delete and move.
- Without **Allow sending**, the connection only creates drafts; nothing leaves the mailbox.
- Revoking a key in the Nylas Dashboard disables the connection at once. Deleting the grant removes the mailbox from Nylas (and, for IMAP grants, its password).
