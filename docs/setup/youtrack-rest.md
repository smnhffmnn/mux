# YouTrack REST

Issues, comments, change history, attachments, agile boards and the administration API of a YouTrack instance, spoken directly over its REST API and shaped for a model: readable IDs, ISO timestamps in your profile time zone, custom fields as a name → value map, comments paged to completion, images as image content.

This type replaces the MCP proxy (type **YouTrack (MCP proxy)**) for working with issue data and the **YouTrack Agile** type for sprint queries. Both keep working; new connections should use this one.

## Prerequisites

- A YouTrack instance (Cloud or Server, 2022.x or later).
- A Permanent Token of the account the connection should act as. Everything the tools do happens with that account's rights and under its name.

### Creating a token

1. Open YouTrack → **Profile → Account Security → Tokens**.
2. Click **New Token**, grant the **YouTrack** scope (and **YouTrack Administration** if the admin tools should reach `/api/admin` and Hub).
3. Copy the token — it is shown only once.

## Fields

| Field | Description |
|------|-------------|
| Base URL | The YouTrack URL as you open it in the browser: `https://instance.youtrack.cloud` or `https://host.myjetbrains.com/youtrack`. Without `/api` and without `/mcp` (the `/mcp` endpoint belongs to the MCP proxy type and is refused here). |
| Permanent Token | The token from above. Stored in the Keychain. |
| Protected fields (optional) | Fields this connection never writes, one per line as `Project:Field` or `Field` for every project — e.g. `P:Sprint priority`. Enforced in code for create_issue, update_issue and run_command. |
| Set-once fields (optional) | Fields this connection writes only while they are still empty — e.g. `P:Department`. |

`read_only = true` (TOML or provisioning) exposes the reading tools only: no create/update/comment/attach/watch/link/log_work/run_command and no admin tools.

## After creating

Click **Test**. The handler calls `/api/users/me` and reports the login and the profile time zone the connection will render timestamps in.

## Notes

- **Time zone.** Timestamps come back as ISO 8601 in the token owner's YouTrack profile time zone (Profile → General → Time zone). Date-only fields are `YYYY-MM-DD` and are written the way YouTrack stores them (12:00 UTC of that day), so a date set through mux shows the same day in the UI for everyone.
- **Fields.** `get_project` lists a project's fields with type, whether a value is required, and the allowed values. `create_issue`/`update_issue` take `fields` as `{"State": "In Progress", "Assignee": "jane.doe", "Due date": "2026-10-15", "Spent time": "1h 30m", "Priority": null}`.
- **Guards** hold for every client of the connection, not only for agents that read the instructions. A refused write names the guard and the field.
- **Commands.** `run_command` is the universal write path (`State Open Assignee me`, `add tag reviewed`, `Fix versions 2026.21`). YouTrack parses the command first; a command it does not understand or whose parsed form touches a guarded field — also as a bare value such as `High` — is refused. `dry_run` returns the parse and whether the guards would refuse it.
- **Attachments** are read with `get_attachment` (images ≤ 5 MB as image content, otherwise a file) and written with `attach` (several files in one call, streamed from disk). Reference them in Markdown as `![](file name)`.
- **Admin.** `admin_get`/`admin_request` pass raw requests to `/api/admin/…` and `/hub/api/rest/…`; paths are normalized first and the issue subtree `/api/admin/projects/{id}/issues` is refused, so the guards cannot be bypassed. Mind the reach of the token: a project's field is an instance of a global prototype with a per-project bundle, a workflow *usage* is not the workflow, a project role shown in a project view may be a global assignment.
- **Instructions** attached to the connection are appended to `search_issues` and `get_issue` — put the meaning of your projects and fields there (what the project keys stand for, what a state means), not the mechanics.
- **API drift.** The endpoints and field selectors this type uses are checked against YouTrack's published OpenAPI document in the test suite; a weekly workflow repeats the check against the live document and reports new API surface.
