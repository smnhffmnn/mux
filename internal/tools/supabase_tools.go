package tools

import (
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Tools returns the MCP tools of a Supabase connection. Reading tools are
// always present; the deploy, delete and secret-writing tools only when the
// connection is not read-only. Connection instructions (what the project's
// functions and tables are for) are appended to project and functions_list,
// the two tools a session starts with.
func (s *Supabase) Tools() []ToolDef {
	withInstructions := func(desc string) string {
		if s.instructions != "" {
			return desc + "\n\n" + s.instructions
		}
		return desc
	}
	sources := strings.Join(sbLogSourceNames(), ", ")

	tools := []ToolDef{
		{
			Tool: mcp.NewTool("project",
				mcp.WithDescription(withInstructions("The project this connection acts on: ref, name, region, status, organization and database host. Also reports whether the connection is read-only.")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: s.handleProject,
		},
		{
			Tool: mcp.NewTool("functions_list",
				mcp.WithDescription(withInstructions("The project's Edge Functions: slug, status, version, verify_jwt, entrypoint and import map paths as deployed, created and updated times (UTC). The version advances with every deploy, from the CLI as well as from here.")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: s.handleFunctionsList,
		},
		{
			Tool: mcp.NewTool("function_get",
				mcp.WithDescription("One deployed function: its metadata and the source files the server holds for it (paths as uploaded, relative to the project root, with sizes). "+
					"With file, the content of that one file is returned inline (text up to 256 KB); with output_dir, all files are written under that directory (never overwriting). "+
					"A function deployed as a pre-built bundle has no source files — the result says so."),
				mcp.WithString("slug", mcp.Required(), mcp.Description("Function slug, as listed by functions_list")),
				mcp.WithString("file", mcp.Description("Path of one file to return inline — as listed in files, or its unique suffix (index.ts)")),
				mcp.WithString("output_dir", mcp.Description("Absolute directory to write all files into, keeping their paths (created if missing; existing files are not overwritten)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: s.handleFunctionGet,
		},
		{
			Tool: mcp.NewTool("secrets_list",
				mcp.WithDescription("Names of the project's function secrets with their update time. Values are never returned."),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: s.handleSecretsList,
		},
		{
			Tool: mcp.NewTool("sql_query",
				mcp.WithDescription("Run SQL on the project's Postgres through the Management API and return the rows as JSON. "+
					"On a read-only connection every query runs on the read-only endpoint (as supabase_read_only_user); otherwise pass read_only=true for the same guarantee, or leave it off to write. "+
					"Schema-qualify tables on the read-only endpoint (public.jobs). Results are not paged — add LIMIT."),
				mcp.WithString("query", mcp.Required(), mcp.Description("SQL statement(s)")),
				mcp.WithArray("parameters", mcp.Description("Positional parameters for $1, $2, … (optional)"), mcp.WithStringItems()),
				mcp.WithBoolean("read_only", mcp.Description("Run as the read-only database user (default: the connection's read_only setting)")),
			),
			Handler: s.handleSQLQuery,
		},
		{
			Tool: mcp.NewTool("logs",
				mcp.WithDescription("Query the project's unified log stream for a window of at most 24 hours (default: the last hour). "+
					"source picks the log source — function_logs is the console output of Edge Functions, function_edge_logs the requests that reached them, edge_logs the API gateway, postgres_logs the database; others: "+sources+". "+
					"function narrows the two function sources to one slug, search to rows whose message contains the text. "+
					"sql replaces the built query with your own ClickHouse SQL over the table logs (columns timestamp, event_message, log_attributes map; filter "+sbLogsSourceColumn+" yourself; source, function, search and limit do not apply then) — the result shows the SQL that ran."),
				mcp.WithString("source", mcp.Description("Log source (default function_logs): "+sources)),
				mcp.WithString("function", mcp.Description("Function slug — only rows of this function (function_logs, function_edge_logs)")),
				mcp.WithString("search", mcp.Description("Only rows whose event_message contains this text (case-insensitive)")),
				mcp.WithString("start", mcp.Description("Window start, RFC 3339 or YYYY-MM-DD HH:MM in UTC (default: end minus one hour)")),
				mcp.WithString("end", mcp.Description("Window end (default: now)")),
				mcp.WithNumber("limit", mcp.Description("Rows (default 100, max 1000), newest first")),
				mcp.WithString("sql", mcp.Description("Your own ClickHouse SQL instead of the built query (not with function or search)")),
				mcp.WithReadOnlyHintAnnotation(true),
			),
			Handler: s.handleLogs,
		},
	}
	if s.readOnly {
		return tools
	}
	tools = append(tools,
		ToolDef{
			Tool: mcp.NewTool("function_deploy",
				mcp.WithDescription("Deploy an Edge Function from a local checkout: pass the project root (the directory that contains supabase/) and the slug. "+
					"mux collects the files the supabase CLI would upload — the entrypoint (supabase/functions/<slug>/index.ts unless config.toml says otherwise), its relative imports (../_shared/… included), the import map (deno.json next to the entrypoint, or as configured) and its local targets, and config.toml's static_files — and the server bundles. "+
					"Files outside the git root are skipped with a warning; files above the project root cannot be carried. "+
					"verify_jwt comes from config.toml, else stays as deployed; pass it only to change it. dry_run shows the file list and metadata without deploying. "+
					"The result carries the function before and after, so the version shows the deploy took."),
				mcp.WithString("path", mcp.Required(), mcp.Description("Absolute path of the project root on the machine running mux (contains supabase/functions)")),
				mcp.WithString("slug", mcp.Required(), mcp.Description("Function slug (the directory name under supabase/functions)")),
				mcp.WithBoolean("verify_jwt", mcp.Description("Require a valid JWT on invocation — overrides config.toml and the deployed setting")),
				mcp.WithBoolean("dry_run", mcp.Description("Only compute and show what would be uploaded (default false)")),
			),
			Handler: s.handleFunctionDeploy,
		},
		ToolDef{
			Tool: mcp.NewTool("function_delete",
				mcp.WithDescription("Delete a deployed Edge Function. Its URL stops answering at once; the local source is untouched. The result shows what was deleted."),
				mcp.WithString("slug", mcp.Required(), mcp.Description("Function slug")),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: s.handleFunctionDelete,
		},
		ToolDef{
			Tool: mcp.NewTool("secrets_set",
				mcp.WithDescription("Create or update function secrets. Prefer env_file: an absolute path to a KEY=VALUE file on the machine running mux, so the values never pass through this conversation; secrets (name → value) is the direct form. "+
					"Names must not start with SUPABASE_. Functions see new values on their next invocation."),
				mcp.WithString("env_file", mcp.Description("Absolute path of a dotenv file on the machine running mux — named .env, .env.<name> or <name>.env; KEY=VALUE per line, # comments, quotes. SUPABASE_* entries are skipped.")),
				mcp.WithObject("secrets", mcp.Description("Secrets as name → value, e.g. {\"OPENAI_API_KEY\": \"sk-…\"} (merged with env_file; the call wins on a duplicate)")),
			),
			Handler: s.handleSecretsSet,
		},
		ToolDef{
			Tool: mcp.NewTool("secrets_delete",
				mcp.WithDescription("Delete function secrets by name. Functions lose the values on their next invocation."),
				mcp.WithArray("names", mcp.Required(), mcp.Description("Secret names"), mcp.WithStringItems()),
				mcp.WithDestructiveHintAnnotation(true),
			),
			Handler: s.handleSecretsDelete,
		},
	)
	return tools
}
