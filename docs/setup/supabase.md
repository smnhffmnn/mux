# Supabase (Management API)

One Supabase project behind the Management API: Edge Functions (list, read back, deploy, delete), function secrets, SQL on the project's database, and the unified log stream. The deploy collects the files the way the supabase CLI does — entrypoint, relative imports, import map and its targets, `static_files` — and the server bundles; nothing but the paths passes through the agent.

## Prerequisites

- A Supabase project. Its **reference** is the 20-letter id in the dashboard URL (`supabase.com/dashboard/project/<ref>`). The `project_id` in `supabase/config.toml` names the local stack and is not necessarily the same.
- An **access token** for the Management API. Create a fine-grained token under Dashboard → Account → **Access Tokens**, scoped to this project, with the permissions the tools need: Edge Functions (read; write for deploy and delete), Secrets, Database, Logs/Analytics. An account-wide personal access token works as well but opens every project the account can see.

## Fields

| Field | Description |
|------|-------------|
| Project Ref | The project reference, 20 lowercase letters. |
| Access Token | The token from above, sent as Bearer token. Stored in the Keychain. |
| API URL | `https://api.supabase.com` unless left empty (the default). Only a proxy or a test server needs anything else. |

`read_only = true` (TOML, provisioning or `connection_add`) exposes the reading tools only: no deploy, delete or secret writes, and every `sql_query` runs on the read-only endpoint.

## After creating

Click **Test**. mux reads the project (`GET /v1/projects/{ref}`) — the call every tool depends on — and reports its name, region and status. A token without access to the project, a wrong reference or a paused project shows up here.

## Notes

- **Deploying.** `function_deploy` takes the project root (the directory that contains `supabase/`) and the slug. Run it with `dry_run` first: the result lists the files and the metadata that would go out. Compare the list with what `supabase functions deploy <slug> --debug` uploads, or with `function_get` after a CLI deploy — once they agree, deploy from mux.
- **verify_jwt** is taken from `[functions.<slug>]` in `supabase/config.toml`; without it the deployed setting is kept. Pass `verify_jwt` on the call only to change it.
- **Secrets.** Values are never returned. To set one, write it to a dotenv file (`.env`, `.env.<name>` or `<name>.env`) on the machine running mux and pass `env_file` — the value goes from the file to Supabase without appearing in the conversation. `SUPABASE_*` entries are skipped.
- **Logs.** A window of at most 24 hours, default the last hour; `function_logs` is the console output of Edge Functions, `function_edge_logs` the requests that reached them. The query runs as ClickHouse SQL over the unified `logs` table; the result shows the SQL that ran.
- **Rate limit.** 60 requests per minute per token; mux retries a 429 with the server's `Retry-After`.
