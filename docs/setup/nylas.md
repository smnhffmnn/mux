# Nylas

## Prerequisites

- A Nylas account with an application in the region where the mailbox data should live (US or EU — the two regions are separate; a key from one does not open grants in the other).
- At least one connected mailbox (a **grant**) in that application: Gmail and Microsoft 365 over OAuth, other providers over IMAP (Dashboard → Grants, or the hosted auth page). IMAP grants hand Nylas the mailbox password; Nylas keeps a rolling 90-day cache of such mailboxes.
- An API key. Which one depends on the mode:
  - **One mailbox** (grant mode): an **IAM key** bound to the grant, with an expiry — see [Nylas IAM](https://developer.nylas.com/docs/v3/auth/nylas-iam/). The key cannot reach any other mailbox.
  - **Every mailbox of the application** (application mode): the application's key, or an IAM key bound to the application. One key, all grants, one sending switch — for the owner of all the mailboxes, not for a connection that serves someone else's.

## Fields

| Field | Description |
|------|-------------|
| Region | `us` (default) or `eu` — the data-residency region of the Nylas application. |
| Grant ID (optional) | The id of the connected mailbox (UUID shown on the grant in the Dashboard). Leave it empty together with Mailbox Address for application mode. |
| Mailbox Address (optional) | The mailbox's address; when Grant ID is empty mux looks the grant up by it (needs a key that may list the application's grants). Also the account's own address for the reply logic — otherwise read from the grant. Both empty: application mode. |
| API Key | The Nylas key for the mode (see above), sent as Bearer token. Stored in the Keychain. |
| Sender Name | Optional. Display name in the From header of drafts and sent mail — in application mode for every mailbox. |
| Allow sending | Registers the send tools (`send_draft`, `reply_send`, `forward_send`). Nylas sends through the provider; nothing else is needed. In application mode it applies to every mailbox. |

`read_only = true` (TOML or provisioning) keeps the reading tools only: no archive, delete or move, no drafts, no sending, no `delete_grant`, no `api_request`. It cannot be combined with `allow_send`.

## After creating

Click **Test**. Grant mode: mux reads the grant (it must be `valid`) and lists the folders — the two calls every tool starts with. Application mode: mux lists the application's grants and reports how many there are, how many are valid, and the first few addresses. A key for the wrong region, an expired key, a revoked grant or a grant-bound key on an application-mode connection shows up here.

## Notes

- Conversations are Nylas threads and include the account's own replies, so each conversation carries `ball`: `"me"` when the other side wrote last, `"them"` when this account wrote last.
- **Application mode:** the mail tools take `mailbox` (address or grant id); `list_mailboxes` without it lists the grants. While the application has exactly one valid grant, `mailbox` may be left out. Each mailbox has its own address, ball position and folders; ids belong to the mailbox they came from. The administration tools `get_grant`, `delete_grant`, `list_connectors`, `list_webhooks` and the pass-through `api_get`/`api_request` (any `/v3/…` path; sending is refused there) exist only in this mode. Grants are created by OAuth or in the Dashboard, not by mux.
- Gmail: archiving removes the INBOX label (the mail stays in All Mail), moving swaps the source label for the target; other labels stay.
- IMAP-backed accounts: Nylas syncs a rolling 90-day window. Conversations outside it are not listed; `search_messages` with `live=true` asks the IMAP server directly and reaches older mail (slow, one folder at a time).
- Nylas also runs a hosted MCP server. mux talks to the REST API instead: the API is versioned and documented (OpenAPI), and it has what the MCP server lacks — archive, delete and move.
- Without **Allow sending**, the connection only creates drafts; nothing leaves the mailbox.
- Revoking a key in the Nylas Dashboard disables the connection at once. Deleting a grant — in the Dashboard or with `delete_grant` — removes the mailbox from Nylas (and, for IMAP grants, its password).
