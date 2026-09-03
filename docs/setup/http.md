# HTTP API

## Prerequisites

- A reachable HTTP API with a known base URL.
- Optional: an API token for authentication.

## Fields

| Field | Description |
|------|-------------|
| Base URL | Base URL of the API (e.g. `https://api.example.com`). |
| API Token (optional) | Bearer token for authentication. Stored in the Keychain. |
| Token Header (optional) | Name of the authorization header. Default: `Authorization: Bearer`. Change if the API expects a different header. |
| Headers (optional) | Extra headers sent with every request, one `Name: Value` per line — e.g. API version headers like `Notion-Version: 2022-06-28`. The token header wins over a custom header of the same name. |

## After creating

Click **Test** to verify reachability. mux sends a GET request to the base URL.

## Notes

- The HTTP type provides generic API tools (GET, POST, etc.) — the API does not need MCP support.
- For MCP servers, use the **MCP Proxy (generic)** type instead.
- The `request` tool can upload a local file as multipart/form-data (`file_path` + `form_fields`). mux reads the file, not the agent — the path must be visible to the mux process.
