# YouTrack Agile

Read-only access to a single YouTrack Agile Board for sprint queries (`get_current_sprint`, `list_sprints`).

## Prerequisites

- A YouTrack instance and the ID of an Agile Board you want to query.
- A Permanent Token with read access to issues and agile boards.

### Finding the Board ID

1. Open the Agile Board in YouTrack.
2. The URL contains the board ID after `/agiles/` — e.g. `https://instance.myjetbrains.com/agiles/97-63/...` → `97-63`.

### Creating a token

1. Open YouTrack → **Profile → Account Security → Tokens**.
2. Click **New Token**.
3. Grant at least read access to issues and agile boards.
4. Copy the token — it is shown only once.

## Fields

| Field | Description |
|------|-------------|
| Base URL | YouTrack base URL, e.g. `https://instance.myjetbrains.com/youtrack`. No trailing `/api`. |
| Permanent Token | Permanent Token (`perm:...`). Stored in the Keychain. |
| Board ID | ID of the Agile Board to query (e.g. `97-63`). |

## After creating

Click **Test** to verify the connection. The handler resolves the board and confirms the token has read access.

## Notes

- One connection serves a single board. Add another connection for a second board.
- Tools wrap the YouTrack REST API (`/api/agiles/{boardId}/sprints[/current]`); rate limits and timeouts behave like a normal YouTrack request.
- Connection-level instructions (configured per-connection, not per-type) are appended to tool descriptions — useful for documenting board-specific conventions like sprint naming schemes.
