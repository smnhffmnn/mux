package tools

// DefaultGoogleWorkspaceInstructions are shown to AI clients for the google-workspace proxy type.
const DefaultGoogleWorkspaceInstructions = `Google Workspace — proxy to google_workspace_mcp (OAuth).

Proxies all tools from a local google_workspace_mcp server. Provides access to
Gmail, Google Drive, Docs, Sheets, Calendar, and Tasks.

Available services:
- Gmail — search, read, send, draft, reply, label management
- Drive — search, list, read, create files and folders
- Docs — read and create/update documents
- Sheets — read, create, update spreadsheets and cell ranges
- Calendar — list, create, update, delete events
- Tasks — list task lists, create, update, complete tasks

Notes:
- The upstream server handles Google OAuth 2.0 authentication directly.
  No bearer token is needed in the mux connection config.
- Available tools depend on the OAuth scopes granted during setup.
  If a tool returns a permission error, the scope may not be authorized.
- File contents from Drive/Docs may be large — prefer targeted reads over full exports.`
