package tools

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Tools returns the MCP tools of a YouTrack REST connection. Read tools are
// always present; write tools and the admin pass-through only when the
// connection is not read-only. Connection instructions (the meaning of a
// project's fields, conventions) are appended to search_issues and get_issue,
// the two tools every session starts with.
func (y *YouTrackREST) Tools() []ToolDef {
	withInstructions := func(desc string) string {
		if y.instructions != "" {
			return desc + "\n\n" + y.instructions
		}
		return desc
	}
	const dateHint = "Timestamps are ISO 8601 in the token owner's YouTrack time zone; date-only fields are YYYY-MM-DD."
	const fieldsHint = "fields is an object of field name → value: strings for enum/state/version/build/owned/group values and user logins, " +
		"arrays of strings for multi-value fields, YYYY-MM-DD for date fields, RFC 3339 (or \"YYYY-MM-DD HH:MM\" in the profile time zone) for date-and-time fields, " +
		"numbers for integer/float, minutes or \"1h 30m\" for period fields, null to clear a field. Field names and allowed values: get_project."

	tools := []ToolDef{
		{
			Tool: mcp.NewTool("me",
				mcp.WithDescription("Who the connection acts as: login, name, email and the profile time zone that all timestamps are rendered in. Also reports whether the connection is read-only."),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleMe,
		},
		{
			Tool: mcp.NewTool("search_issues",
				mcp.WithDescription(withInstructions("Search issues with YouTrack query syntax (e.g. \"project: P #Unresolved Assignee: me sort by: updated desc\"). "+
					"Returns a compact view per issue — ID, summary, project, timestamps and all custom fields as a name → value map. Page with skip; the result says when a page is full. "+dateHint)),
				mcp.WithString("query", mcp.Required(), mcp.Description("YouTrack search query")),
				mcp.WithNumber("top", mcp.Description("Issues per call (default 50, max 200)")),
				mcp.WithNumber("skip", mcp.Description("Issues to skip for paging (default 0)")),
				mcp.WithBoolean("include_description", mcp.Description("Include each issue's description (default false — descriptions are long)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleSearchIssues,
		},
		{
			Tool: mcp.NewTool("get_issue",
				mcp.WithDescription(withInstructions("Read one issue in full: summary, description, every custom field (name → value), tags, links grouped by relation, attachments, watchers and — by default — all comments, paged to completion. "+dateHint)),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID, e.g. PROJ-123")),
				mcp.WithBoolean("include_comments", mcp.Description("Include all comments (default true)")),
				mcp.WithBoolean("include_description", mcp.Description("Include the description (default true)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleGetIssue,
		},
		{
			Tool: mcp.NewTool("get_comments",
				mcp.WithDescription("Comments of an issue with author, timestamps, visibility and attachments. Without top/skip the whole thread is returned (paged internally); with them, one page."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithNumber("top", mcp.Description("Comments per page (optional; default is the whole thread)")),
				mcp.WithNumber("skip", mcp.Description("Comments to skip (optional)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleGetComments,
		},
		{
			Tool: mcp.NewTool("get_activities",
				mcp.WithDescription("Change history: who changed which field, comment, link, attachment or tag, and when — for one issue or for every issue matching a query. "+
					"Each entry carries removed and added values. "+dateHint),
				mcp.WithString("issue", mcp.Description("Readable issue ID — history of this issue")),
				mcp.WithString("query", mcp.Description("YouTrack search query — history of all matching issues (alternative to issue)")),
				mcp.WithString("categories", mcp.Description("Comma-separated activity categories (default: created, fields, comments, attachments, links, resolved, summary, description, tags, sprint, work items, project, visibility). E.g. CustomFieldCategory,CommentsCategory")),
				mcp.WithString("author", mcp.Description("Only changes by this login (query mode)")),
				mcp.WithString("start", mcp.Description("Only changes at or after this time (RFC 3339 or YYYY-MM-DD)")),
				mcp.WithString("end", mcp.Description("Only changes at or before this time (RFC 3339 or YYYY-MM-DD)")),
				mcp.WithBoolean("newest_first", mcp.Description("Newest first (default: oldest first)")),
				mcp.WithNumber("top", mcp.Description("Entries per call (default 100, max 500)")),
				mcp.WithNumber("skip", mcp.Description("Entries to skip for paging")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleGetActivities,
		},
		{
			Tool: mcp.NewTool("get_attachment",
				mcp.WithDescription("Fetch an attachment's content. Images up to 5 MB come back as image content you can look at; small text files are inlined; everything else is saved to a file and the path returned. "+
					"Pass output_file to choose the destination (absolute path, never overwritten)."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithString("attachment", mcp.Required(), mcp.Description("Attachment id or file name as listed by get_issue")),
				mcp.WithString("output_file", mcp.Description("Save to this absolute path instead of returning the content (supports ~)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleGetAttachment,
		},
		{
			Tool: mcp.NewTool("list_projects",
				mcp.WithDescription("Projects the token can see: id, short name (the issue prefix), name, leader."),
				mcp.WithBoolean("include_archived", mcp.Description("Include archived projects (default false)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleListProjects,
		},
		{
			Tool: mcp.NewTool("get_project",
				mcp.WithDescription("One project with its custom fields: name, type, whether a value is required, and the allowed values (enum, state — resolved states listed separately — version, …). Consult it before writing fields."),
				mcp.WithString("project", mcp.Required(), mcp.Description("Project short name (e.g. P) or id")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleGetProject,
		},
		{
			Tool: mcp.NewTool("find_users",
				mcp.WithDescription("Search users by login, name or email fragment. Returns id, login, name, email and whether the account is banned. Field values take the login."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Login, name or email fragment")),
				mcp.WithNumber("top", mcp.Description("Max results (default 20)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleFindUsers,
		},
		{
			Tool: mcp.NewTool("list_link_types",
				mcp.WithDescription("Issue link types with the relation phrases link_issues accepts (e.g. \"parent for\" / \"subtask of\", \"depends on\" / \"is required for\", \"relates to\")."),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleListLinkTypes,
		},
		{
			Tool: mcp.NewTool("list_boards",
				mcp.WithDescription("Agile boards: id, name, projects, current sprint."),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleListBoards,
		},
		{
			Tool: mcp.NewTool("get_current_sprint",
				mcp.WithDescription("The current sprint of an agile board: name, start, finish (ISO 8601), goal."),
				mcp.WithString("board", mcp.Required(), mcp.Description("Board id (e.g. 97-63) or name")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleGetCurrentSprint,
		},
		{
			Tool: mcp.NewTool("list_sprints",
				mcp.WithDescription("Sprints of an agile board, newest first, with start and finish as ISO 8601."),
				mcp.WithString("board", mcp.Required(), mcp.Description("Board id (e.g. 97-63) or name")),
				mcp.WithNumber("top", mcp.Description("Max sprints (default 50)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleListSprints,
		},
	}

	if y.readOnly {
		return tools
	}

	guardHint := describeGuards(y.protected, y.setOnce)

	tools = append(tools,
		ToolDef{
			Tool: mcp.NewTool("create_issue",
				mcp.WithDescription("Create an issue. "+fieldsHint+guardHint),
				mcp.WithString("project", mcp.Required(), mcp.Description("Project short name (e.g. P) or id")),
				mcp.WithString("summary", mcp.Required(), mcp.Description("Issue summary")),
				mcp.WithString("description", mcp.Description("Description (YouTrack Markdown; attachments are referenced as ![](file name))")),
				mcp.WithObject("fields", mcp.Description("Custom field values by name, e.g. {\"Priority\": \"High\", \"Assignee\": \"jane.doe\", \"Due date\": \"2026-10-15\"}"), mcp.AdditionalProperties(true)),
			),
			Handler: y.handleCreateIssue,
		},
		ToolDef{
			Tool: mcp.NewTool("update_issue",
				mcp.WithDescription("Update an issue's summary, description and/or custom fields (State, Assignee, Priority, dates, …). "+fieldsHint+guardHint),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithString("summary", mcp.Description("New summary")),
				mcp.WithString("description", mcp.Description("New description (replaces the whole text)")),
				mcp.WithObject("fields", mcp.Description("Custom field values by name, e.g. {\"State\": \"In Progress\", \"Assignee\": \"jane.doe\", \"Spent time\": \"1h 30m\", \"Due date\": null}"), mcp.AdditionalProperties(true)),
			),
			Handler: y.handleUpdateIssue,
		},
		ToolDef{
			Tool: mcp.NewTool("add_comment",
				mcp.WithDescription("Add a comment (YouTrack Markdown). Optionally limit its visibility to groups and/or users; without visibility the comment is visible to everyone who sees the issue."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithString("text", mcp.Required(), mcp.Description("Comment text")),
				mcp.WithArray("visibility_groups", mcp.Description("Group names allowed to see the comment"), mcp.WithStringItems()),
				mcp.WithArray("visibility_users", mcp.Description("User logins allowed to see the comment"), mcp.WithStringItems()),
			),
			Handler: y.handleAddComment,
		},
		ToolDef{
			Tool: mcp.NewTool("update_comment",
				mcp.WithDescription("Edit an existing comment: replace its text and/or change its visibility (at least one of the two). Visibility given as empty lists makes the comment visible to everyone who sees the issue; visibility left out stays as it is. "+
					"Whether the account may edit a comment is YouTrack's decision (its own comments, or any comment with the matching permission) — a refused edit comes back as the API's error."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithString("comment", mcp.Required(), mcp.Description("Comment id as listed by get_comments or get_issue (e.g. 4-123)")),
				mcp.WithString("text", mcp.Description("New text (YouTrack Markdown; replaces the whole comment)")),
				mcp.WithArray("visibility_groups", mcp.Description("Group names allowed to see the comment"), mcp.WithStringItems()),
				mcp.WithArray("visibility_users", mcp.Description("User logins allowed to see the comment"), mcp.WithStringItems()),
			),
			Handler: y.handleUpdateComment,
		},
		ToolDef{
			Tool: mcp.NewTool("delete_comment",
				mcp.WithDescription("Delete a comment. The result confirms the deletion by re-reading the comment: YouTrack no longer returns it afterwards (it disappears from the thread and from the issue's history), so the result carries its id with deleted = true. "+
					"Whether the account may delete a comment is YouTrack's decision (its own comments, or any comment with the matching permission) — a refused deletion comes back as the API's error."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithString("comment", mcp.Required(), mcp.Description("Comment id as listed by get_comments or get_issue (e.g. 4-123)")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: y.handleDeleteComment,
		},
		ToolDef{
			Tool: mcp.NewTool("attach",
				mcp.WithDescription("Upload one or more local files as attachments of an issue, streamed from disk. Returns the issue's complete attachment list afterwards with the uploaded ones marked. "+
					"Reference an attachment in a description or comment as ![](file name). File names must be unique within an issue — list existing attachments with get_issue first."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithArray("file_paths", mcp.Required(), mcp.Description("Absolute paths of the files to upload (supports ~). mux reads them, so they must be visible to the mux process."), mcp.WithStringItems()),
			),
			Handler: y.handleAttach,
		},
		ToolDef{
			Tool: mcp.NewTool("watch",
				mcp.WithDescription("Add users as watchers of an issue (they get notified about changes). Returns the watcher list afterwards."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithArray("users", mcp.Required(), mcp.Description("Logins to add as watchers"), mcp.WithStringItems()),
			),
			Handler: y.handleWatch,
		},
		ToolDef{
			Tool: mcp.NewTool("link_issues",
				mcp.WithDescription("Link two issues. relation is the phrase as seen from issue: \"parent for\" makes target a subtask of issue, \"subtask of\" the other way round; \"depends on\", \"is required for\", \"relates to\", \"duplicates\". See list_link_types."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable ID of the issue the relation is seen from")),
				mcp.WithString("relation", mcp.Required(), mcp.Description("Relation phrase, e.g. \"parent for\"")),
				mcp.WithString("target", mcp.Required(), mcp.Description("Readable ID of the other issue")),
			),
			Handler: y.handleLinkIssues,
		},
		ToolDef{
			Tool: mcp.NewTool("log_work",
				mcp.WithDescription("Log spent time on an issue as a work item."),
				mcp.WithString("issue", mcp.Required(), mcp.Description("Readable issue ID")),
				mcp.WithString("duration", mcp.Required(), mcp.Description("Minutes (e.g. \"90\") or hours and minutes (\"1h 30m\"); days and weeks are not accepted")),
				mcp.WithString("date", mcp.Description("Work date as YYYY-MM-DD (default: today in the profile time zone)")),
				mcp.WithString("text", mcp.Description("Description of the work")),
				mcp.WithString("type", mcp.Description("Work item type by name (project-specific, e.g. Development)")),
			),
			Handler: y.handleLogWork,
		},
		ToolDef{
			Tool: mcp.NewTool("run_command",
				mcp.WithDescription("Apply a YouTrack command to one or more issues — the universal write path for everything the dedicated tools do not cover (tags, sprint/board moves, bulk changes, workflow commands). "+
					"Same syntax as the command dialog, e.g. \"State In Progress Assignee me\", \"add tag reviewed\", \"Fix versions 2026.21\". "+
					"YouTrack parses the command first; a command it does not understand, or one that touches a guarded field, is refused and nothing is applied. "+
					"dry_run returns the parse (and whether the guards would refuse it) without applying anything."+guardHint),
				mcp.WithArray("issues", mcp.Required(), mcp.Description("Readable issue IDs"), mcp.WithStringItems()),
				mcp.WithString("command", mcp.Required(), mcp.Description("Command text")),
				mcp.WithString("comment", mcp.Description("Comment to add alongside the command")),
				mcp.WithBoolean("silent", mcp.Description("Do not send notifications (default false)")),
				mcp.WithBoolean("dry_run", mcp.Description("Parse only, apply nothing (default false)")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: y.handleRunCommand,
		},
		ToolDef{
			Tool: mcp.NewTool("admin_get",
				mcp.WithDescription("Raw GET against the administration API — /api/admin/… (projects, custom fields, bundles, workflows, link types) or /hub/api/rest/… (Hub: users, groups, roles). "+
					"Pass fields= yourself; the JSON comes back as is. Paths must be normalized (no . or .. segments); the issue subtree /api/admin/projects/{id}/issues is refused — issues have their own tools. Reach: a project's workflow *usage* (/admin/projects/{id}/workflows) is not the workflow itself; a project's custom field is an instance of a global field prototype with its own bundle per project; a project role is not a global role."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /api/admin/ or /hub/api/rest/, including the query string")),
				mcp.WithString("output_file", mcp.Description("Save the response to this absolute path (supports ~)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: y.handleAdminGet,
		},
		ToolDef{
			Tool: mcp.NewTool("admin_request",
				mcp.WithDescription("Raw POST/PUT/PATCH/DELETE against /api/admin/… or /hub/api/rest/… with a JSON body — creates and changes projects, fields, bundles, workflow usages, roles. "+
					"Paths must be normalized; the issue subtree /api/admin/projects/{id}/issues is refused so the field guards cannot be bypassed. "+
					"Writes with the token owner's rights, which may be instance-wide: a DELETE of a role in a project view can remove a global assignment, and a bundle deletion affects every project sharing it. Read with admin_get first."),
				mcp.WithString("method", mcp.Required(), mcp.Description("POST, PUT, PATCH or DELETE"), mcp.Enum("POST", "PUT", "PATCH", "DELETE")),
				mcp.WithString("path", mcp.Required(), mcp.Description("Path starting with /api/admin/ or /hub/api/rest/, including the query string (add fields= to see the result)")),
				mcp.WithString("body", mcp.Description("JSON body")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: y.handleAdminRequest,
		},
	)
	return tools
}

// describeGuards renders the guard lists as one sentence for the write tools'
// descriptions; "" when the connection has no guards.
func describeGuards(protected, setOnce []config.FieldGuard) string {
	var clauses []string
	if len(protected) > 0 {
		clauses = append(clauses, "refuses writes to the protected fields "+joinAnd(guardNames(protected)))
	}
	if len(setOnce) > 0 {
		clauses = append(clauses, "writes the set-once fields "+joinAnd(guardNames(setOnce))+" only while they are still empty")
	}
	if len(clauses) == 0 {
		return ""
	}
	return " This connection " + strings.Join(clauses, " and ") + "."
}

func guardNames(guards []config.FieldGuard) []string {
	out := make([]string, 0, len(guards))
	for _, g := range guards {
		if g.Project == "" {
			out = append(out, "\""+g.Field+"\" (every project)")
		} else {
			out = append(out, "\""+g.Field+"\" in "+g.Project)
		}
	}
	return out
}

func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
