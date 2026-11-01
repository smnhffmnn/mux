package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// The supabase connection type puts one Supabase project behind the
// Management API (https://api.supabase.com) as MCP tools: Edge Functions
// (list, read, deploy, delete), function secrets, SQL against the project's
// database and the unified log stream.
//
// The deploy is the reason the type exists. The API takes a function as a
// multipart request with one part per source file — the server bundles — and
// the set of files is what the supabase CLI computes in its API mode: the
// import graph from the entrypoint (relative imports only), the local targets
// of the import map, and the static_files patterns from config.toml, every
// name anchored at the project root. supabase_deploy.go reproduces that walk,
// so a function deployed through mux carries the same files as one deployed
// with the CLI; nothing passes through the model but the paths.
//
// The contract is the Management API's published OpenAPI document: the
// endpoints, query parameters and response fields the type relies on are
// listed in sbEndpoints and sbSchemaFields below and checked against it by
// supabase_spec_test.go (vendored snapshot under testdata, live document in
// the drift workflow).

const (
	sbMaxJSONBody     = 8 << 20          // largest JSON answer read (query results, logs)
	sbMaxBundleBody   = 64 << 20         // largest function body read (the source files of one function)
	sbInlineFileMax   = 256 * 1024       // a single source file is inlined up to this size
	sbRequestTimeout  = 90 * time.Second // JSON calls; deploys run under uploadTimeout
	sbRetryAttempts   = 4                // tries on HTTP 429, the API's documented limit is 60 requests per minute
	sbRetryMaxDelay   = 30 * time.Second
	sbLogsDefaultSpan = time.Hour
	sbLogsMaxSpan     = 24 * time.Hour // the API refuses longer windows
	sbLogsDefaultRows = 100
	sbLogsMaxRows     = 1000
	sbSecretMaxBatch  = 100 // the API's maxItems on the secrets body
)

// DefaultSupabaseInstructions are shown to AI clients when a connection has
// no instructions of its own. Mechanics only — what a project's functions and
// tables mean belongs in the connection's instructions.
const DefaultSupabaseInstructions = `One Supabase project over the Management API; every tool acts on that project.
functions_list shows the deployed Edge Functions with version and update time; function_get reads one back — its metadata and the source files the server holds (list, one file inline, or all files into a directory).
migrations_pending compares the checkout's supabase/migrations/ with the project's history; migrations_apply runs the pending files through the API in file order (dry_run first) — the database is production.
function_deploy uploads a function from a local checkout: pass the project root (the directory that contains supabase/) and the slug; mux collects the files the way the supabase CLI does (entrypoint, relative imports, import map targets, static_files from config.toml) and the server bundles. Use dry_run first: it shows the file list and compares it with the files of the deployed version (comparison.sameFiles). verify_jwt is taken from config.toml, else kept as deployed; pass it only to change it.
secrets_list never returns values. secrets_set reads them from an env_file on this machine (preferred) or from the call; names must not start with SUPABASE_.
sql_query runs SQL on the project's Postgres through the API; on a read-only connection only the read-only endpoint is used. logs queries the unified log stream (ClickHouse SQL) for a window of at most 24 hours — the default is the last hour of function_logs; filter by function slug.
Write tools are absent on read-only connections.`

// Supabase wraps one project of the Supabase Management API as MCP tools.
type Supabase struct {
	name         string
	baseURL      string // https://api.supabase.com — no path
	token        string
	projectRef   string
	readOnly     bool
	instructions string
	client       *http.Client
	// longClient serves calls the server works on for minutes — the deploy
	// (bundling) and a migration (its SQL): no header timeout, the upload
	// budget as the overall one.
	longClient *http.Client

	// now and sleep are swappable for tests (log windows, 429 back-off).
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// NewSupabase creates a Supabase connection from config.
func NewSupabase(conn config.Connection, dialer Dialer) (*Supabase, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("supabase: token (access token) is required")
	}
	if err := config.ValidateSupabase(conn); err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}
	baseURL, err := config.SupabaseBaseURL(conn.URL)
	if err != nil {
		return nil, fmt.Errorf("supabase: %w", err)
	}
	transport := &http.Transport{
		ResponseHeaderTimeout: 60 * time.Second,
	}
	longTransport := &http.Transport{}
	if dialer != nil {
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
		transport.DialContext = dial
		longTransport.DialContext = dial
	}
	apiHost := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"))
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if strings.ToLower(req.URL.Host) != apiHost {
			return fmt.Errorf("refusing redirect to %s", req.URL.Host)
		}
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		return nil
	}
	return &Supabase{
		name:         conn.Name,
		baseURL:      baseURL,
		token:        conn.Token,
		projectRef:   strings.TrimSpace(conn.ProjectRef),
		readOnly:     conn.ReadOnly,
		instructions: conn.Instructions,
		// A redirect to another host would replay the request body — secret
		// values, a function's source — there. The API has no reason to
		// redirect at all; refuse anything off-host.
		client:     &http.Client{Transport: transport, Timeout: sbRequestTimeout, CheckRedirect: checkRedirect},
		longClient: &http.Client{Transport: longTransport, Timeout: uploadTimeout, CheckRedirect: checkRedirect},
		now:        time.Now,
		sleep:      sbSleep,
	}, nil
}

func sbSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- Endpoint registry ---

// sbEndpoint is one Management API call the type makes. Path is the OpenAPI
// template; Params are the query parameters the call may send. The spec test
// resolves every entry against the published OpenAPI document, so a renamed
// path, a dropped parameter or a removed field fails a test instead of a
// user's call.
type sbEndpoint struct {
	Method  string
	Path    string
	Params  []string
	Headers []string // request headers beyond auth and content type the call may send
}

// sbEndpoints is the complete list of API calls the type can make through
// call. Handlers address entries by key; call refuses anything that is not
// listed, so the spec test sees every path the type can send.
var sbEndpoints = map[string]sbEndpoint{
	"project":          {Method: http.MethodGet, Path: "/v1/projects/{ref}"},
	"functions":        {Method: http.MethodGet, Path: "/v1/projects/{ref}/functions"},
	"function":         {Method: http.MethodGet, Path: "/v1/projects/{ref}/functions/{function_slug}"},
	"function.body":    {Method: http.MethodGet, Path: "/v1/projects/{ref}/functions/{function_slug}/body"},
	"function.deploy":  {Method: http.MethodPost, Path: "/v1/projects/{ref}/functions/deploy", Params: []string{"slug"}},
	"function.delete":  {Method: http.MethodDelete, Path: "/v1/projects/{ref}/functions/{function_slug}"},
	"secrets":          {Method: http.MethodGet, Path: "/v1/projects/{ref}/secrets"},
	"secrets.create":   {Method: http.MethodPost, Path: "/v1/projects/{ref}/secrets"},
	"secrets.delete":   {Method: http.MethodDelete, Path: "/v1/projects/{ref}/secrets"},
	"query":            {Method: http.MethodPost, Path: "/v1/projects/{ref}/database/query"},
	"query.readonly":   {Method: http.MethodPost, Path: "/v1/projects/{ref}/database/query/read-only"},
	"logs":             {Method: http.MethodGet, Path: "/v1/projects/{ref}/analytics/endpoints/logs", Params: []string{"sql", "iso_timestamp_start", "iso_timestamp_end"}},
	"migrations":       {Method: http.MethodGet, Path: "/v1/projects/{ref}/database/migrations"},
	"migrations.apply": {Method: http.MethodPost, Path: "/v1/projects/{ref}/database/migrations", Headers: []string{"Idempotency-Key"}},
}

// sbSchemaFields lists, per OpenAPI component schema, the properties the type
// reads or writes. The spec test checks every name against the schema, so a
// property Supabase removes or renames fails a test before it fails a user.
var sbSchemaFields = map[string][]string{
	"V1ProjectWithDatabaseResponse_Output": {"ref", "name", "region", "status", "organization_slug", "created_at", "database"},
	"FunctionResponse_Output":              {"id", "slug", "name", "status", "version", "created_at", "updated_at", "verify_jwt", "import_map", "entrypoint_path", "import_map_path", "ezbr_sha256"},
	"FunctionSlugResponse_Output":          {"id", "slug", "name", "status", "version", "created_at", "updated_at", "verify_jwt", "import_map", "entrypoint_path", "import_map_path", "ezbr_sha256"},
	"DeployFunctionResponse_Output":        {"id", "slug", "name", "status", "version", "created_at", "updated_at", "verify_jwt", "import_map", "entrypoint_path", "import_map_path", "ezbr_sha256"},
	"FunctionDeployBody":                   {"file", "metadata"},
	"SecretResponse_Output":                {"name", "value", "updated_at"},
	"V1RunQueryBody":                       {"query", "parameters", "read_only"},
	"V1ReadOnlyQueryBody":                  {"query", "parameters"},
	"AnalyticsResponse_Output":             {"result", "error"},
	"V1CreateMigrationBody":                {"query", "name"},
}

// sbMigrationListFields are the properties of one entry of
// V1ListMigrationsResponse_Output (an array schema). Checked by the spec test.
var sbMigrationListFields = []string{"version", "name"}

// sbDeployMetadataFields are the properties of FunctionDeployBody.metadata
// the deploy sends. Checked by the spec test as a nested schema.
var sbDeployMetadataFields = []string{"entrypoint_path", "import_map_path", "static_patterns", "verify_jwt", "name"}

// --- HTTP ---

// sbHTTPError is a non-2xx answer, with what the API said about it.
type sbHTTPError struct {
	Status  int
	Message string
}

func (e *sbHTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Supabase API error (HTTP %d)", e.Status)
	}
	return fmt.Sprintf("Supabase API error (HTTP %d): %s", e.Status, e.Message)
}

// sbErrorMessage reads the message of an error body. The Management API
// answers {"message": "...", "statusCode": n}; some endpoints say
// {"error": "..."}; anything else is quoted as is, truncated.
func sbErrorMessage(status int, body []byte) *sbHTTPError {
	var e struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		switch m := e.Message.(type) {
		case string:
			if m != "" {
				return &sbHTTPError{Status: status, Message: m}
			}
		case []any:
			parts := make([]string, 0, len(m))
			for _, p := range m {
				parts = append(parts, fmt.Sprint(p))
			}
			if len(parts) > 0 {
				return &sbHTTPError{Status: status, Message: strings.Join(parts, "; ")}
			}
		}
		if e.Error != "" {
			return &sbHTTPError{Status: status, Message: e.Error}
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 2048 {
		text = text[:2048] + "…"
	}
	return &sbHTTPError{Status: status, Message: text}
}

// sbBody is a request body the type can open more than once: once per try
// on a 429 and again for a redirect.
type sbBody struct {
	open        func() (io.ReadCloser, error)
	length      int64
	contentType string
}

func sbJSONBody(v any) (*sbBody, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return &sbBody{
		open:        func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil },
		length:      int64(len(b)),
		contentType: "application/json",
	}, nil
}

// call performs a registered call and returns the open response; the caller
// closes the body. pathParams fill the {placeholders} of the endpoint's path
// template ({ref} is filled from the connection); query carries the
// parameters listed in Params. accept overrides the Accept header. HTTP 429
// is retried with the server's Retry-After, bounded.
func (s *Supabase) call(ctx context.Context, key string, pathParams map[string]string, query url.Values, body *sbBody, accept string, client *http.Client) (*http.Response, error) {
	return s.callHeaders(ctx, key, pathParams, query, nil, body, accept, client)
}

// callHeaders is call with extra request headers; each must be listed in the
// endpoint's Headers.
func (s *Supabase) callHeaders(ctx context.Context, key string, pathParams map[string]string, query url.Values, headers map[string]string, body *sbBody, accept string, client *http.Client) (*http.Response, error) {
	ep, ok := sbEndpoints[key]
	if !ok {
		return nil, fmt.Errorf("supabase: endpoint %q is not registered", key)
	}
	path := strings.ReplaceAll(ep.Path, "{ref}", url.PathEscape(s.projectRef))
	for k, v := range pathParams {
		// Slugs come from the API or from config, but they travel through an
		// agent before they come back: a value that would change the path's
		// shape is refused.
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "/?#") {
			return nil, fmt.Errorf("supabase: %s %q is not a valid value", k, v)
		}
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(path, "{") {
		return nil, fmt.Errorf("supabase: endpoint %q has an unfilled path parameter: %s", key, path)
	}
	allowed := map[string]bool{}
	for _, p := range ep.Params {
		allowed[p] = true
	}
	for k := range query {
		if !allowed[k] {
			return nil, fmt.Errorf("supabase: endpoint %q does not take query parameter %q", key, k)
		}
	}
	allowedHeaders := map[string]bool{}
	for _, h := range ep.Headers {
		allowedHeaders[strings.ToLower(h)] = true
	}
	for k := range headers {
		if !allowedHeaders[strings.ToLower(k)] {
			return nil, fmt.Errorf("supabase: endpoint %q does not take header %q", key, k)
		}
	}
	u := s.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	if client == nil {
		client = s.client
	}
	if accept == "" {
		accept = "application/json"
	}

	for attempt := 1; ; attempt++ {
		var rdr io.ReadCloser
		if body != nil {
			var err error
			rdr, err = body.open()
			if err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, ep.Method, u, rdr)
		if err != nil {
			if rdr != nil {
				rdr.Close()
			}
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("Accept", accept)
		if body != nil {
			req.ContentLength = body.length
			req.GetBody = body.open
			req.Header.Set("Content-Type", body.contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", ep.Method, path, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= sbRetryAttempts {
			return resp, nil
		}
		delay := sbRetryDelay(resp.Header.Get("Retry-After"), attempt)
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		log.Printf("[supabase] %s: rate limited on %s %s, retrying in %s (try %d of %d)", s.name, ep.Method, path, delay, attempt, sbRetryAttempts)
		if err := s.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// sbRetryDelay reads Retry-After (seconds) and falls back to a short
// back-off; both are capped so a wrong header cannot park a call.
func sbRetryDelay(retryAfter string, attempt int) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d > sbRetryMaxDelay {
			d = sbRetryMaxDelay
		}
		return d
	}
	d := time.Duration(attempt*attempt) * time.Second
	if d > sbRetryMaxDelay {
		d = sbRetryMaxDelay
	}
	return d
}

// callJSON performs a registered call, requires a 2xx status and decodes the
// body into out (nil out discards it). It returns the status code.
func (s *Supabase) callJSON(ctx context.Context, key string, pathParams map[string]string, query url.Values, body any, out any) (int, error) {
	return s.callJSONHeaders(ctx, key, pathParams, query, nil, body, out)
}

// callJSONHeaders is callJSON with extra request headers.
func (s *Supabase) callJSONHeaders(ctx context.Context, key string, pathParams map[string]string, query url.Values, headers map[string]string, body any, out any) (int, error) {
	return s.callJSONHeadersWith(ctx, nil, key, pathParams, query, headers, body, out)
}

// callJSONHeadersWith is callJSONHeaders on a given client (nil: the
// connection's default one).
func (s *Supabase) callJSONHeadersWith(ctx context.Context, client *http.Client, key string, pathParams map[string]string, query url.Values, headers map[string]string, body any, out any) (int, error) {
	var b *sbBody
	if body != nil {
		var err error
		if b, err = sbJSONBody(body); err != nil {
			return 0, err
		}
	}
	resp, err := s.callHeaders(ctx, key, pathParams, query, headers, b, "", client)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, sbMaxJSONBody+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, sbErrorMessage(resp.StatusCode, data)
	}
	if int64(len(data)) > sbMaxJSONBody {
		return resp.StatusCode, fmt.Errorf("response larger than %d MB — narrow the request", sbMaxJSONBody>>20)
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, fmt.Errorf("decode Supabase response (HTTP %d): %w", resp.StatusCode, err)
	}
	return resp.StatusCode, nil
}

// sbAnswered reports whether err carries an answer from the API (a status),
// as opposed to a request that died without one — after which the server
// may or may not have acted.
func sbAnswered(err error) bool {
	var he *sbHTTPError
	return errors.As(err, &he)
}

// sbNotFound reports whether err is the API's 404.
func sbNotFound(err error) bool {
	var he *sbHTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// --- Rendering ---

// sbResultJSON renders a value as an indented JSON tool result.
func sbResultJSON(v any) (*mcp.CallToolResult, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

// sbTime renders an API timestamp. The functions endpoints declare integers
// without a unit; values of the current era are milliseconds above 1e12 and
// seconds below, so both are read.
func sbTime(v int64) string {
	if v <= 0 {
		return ""
	}
	if v > 1e12 {
		return time.UnixMilli(v).UTC().Format(time.RFC3339)
	}
	return time.Unix(v, 0).UTC().Format(time.RFC3339)
}

// sbRawFunction is a function as the API returns it (list, get, deploy).
type sbRawFunction struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	Version       int    `json:"version"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	VerifyJWT     *bool  `json:"verify_jwt"`
	ImportMap     *bool  `json:"import_map"`
	Entrypoint    string `json:"entrypoint_path"`
	ImportMapPath string `json:"import_map_path"`
	EzbrSHA256    string `json:"ezbr_sha256"`
}

// sbFunction is the shape the tools return.
type sbFunction struct {
	Slug          string `json:"slug"`
	Name          string `json:"name,omitempty"`
	Status        string `json:"status"`
	Version       int    `json:"version"`
	VerifyJWT     *bool  `json:"verifyJwt,omitempty"`
	Entrypoint    string `json:"entrypointPath,omitempty"`
	ImportMapPath string `json:"importMapPath,omitempty"`
	BundleSHA256  string `json:"bundleSha256,omitempty"`
	Created       string `json:"created,omitempty"`
	Updated       string `json:"updated,omitempty"`
	ID            string `json:"id"`
}

func renderSBFunction(f sbRawFunction) sbFunction {
	out := sbFunction{
		Slug:          f.Slug,
		Name:          f.Name,
		Status:        f.Status,
		Version:       f.Version,
		VerifyJWT:     f.VerifyJWT,
		Entrypoint:    f.Entrypoint,
		ImportMapPath: f.ImportMapPath,
		BundleSHA256:  f.EzbrSHA256,
		Created:       sbTime(f.CreatedAt),
		Updated:       sbTime(f.UpdatedAt),
		ID:            f.ID,
	}
	if out.Name == out.Slug {
		out.Name = ""
	}
	return out
}

// --- Project ---

type sbRawProject struct {
	Ref          string `json:"ref"`
	Name         string `json:"name"`
	Region       string `json:"region"`
	Status       string `json:"status"`
	Organization string `json:"organization_slug"`
	CreatedAt    string `json:"created_at"`
	Database     struct {
		Host    string `json:"host"`
		Version string `json:"version"`
	} `json:"database"`
}

// Verify reads the project — the call every tool depends on — and returns
// what the desktop test and the live test report.
func (s *Supabase) Verify(ctx context.Context) (*sbRawProject, error) {
	var p sbRawProject
	if _, err := s.callJSON(ctx, "project", nil, nil, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Supabase) handleProject(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p, err := s.Verify(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return sbResultJSON(map[string]any{
		"ref":          p.Ref,
		"name":         p.Name,
		"region":       p.Region,
		"status":       p.Status,
		"organization": p.Organization,
		"created":      p.CreatedAt,
		"database":     map[string]any{"host": p.Database.Host, "version": p.Database.Version},
		"readOnly":     s.readOnly,
		"api":          s.baseURL,
	})
}

// --- Functions: list and read ---

func (s *Supabase) listFunctions(ctx context.Context) ([]sbRawFunction, error) {
	var raw []sbRawFunction
	if _, err := s.callJSON(ctx, "functions", nil, nil, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *Supabase) handleFunctionsList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	raw, err := s.listFunctions(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	out := make([]sbFunction, 0, len(raw))
	for _, f := range raw {
		out = append(out, renderSBFunction(f))
	}
	return sbResultJSON(map[string]any{"project": s.projectRef, "count": len(out), "functions": out})
}

// getFunction reads one function's metadata; a missing function is reported
// as (nil, nil) so callers can tell "new" from "failed".
func (s *Supabase) getFunction(ctx context.Context, slug string) (*sbRawFunction, error) {
	var f sbRawFunction
	if _, err := s.callJSON(ctx, "function", map[string]string{"function_slug": slug}, nil, nil, &f); err != nil {
		if sbNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &f, nil
}

// sbSlugArg reads and checks a function slug argument. The API's pattern is
// ^[A-Za-z][A-Za-z0-9_-]*$; anything else never reaches the wire.
func sbSlugArg(req mcp.CallToolRequest) (string, error) {
	slug := strings.TrimSpace(req.GetString("slug", ""))
	if slug == "" {
		return "", errors.New("slug is required")
	}
	if !sbValidSlug(slug) {
		return "", fmt.Errorf("slug %q is not a function slug (letters, digits, _ and -, starting with a letter)", slug)
	}
	return slug, nil
}

func sbValidSlug(slug string) bool {
	if slug == "" {
		return false
	}
	for i, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// --- Secrets ---

type sbRawSecret struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Supabase) handleSecretsList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var raw []sbRawSecret
	if _, err := s.callJSON(ctx, "secrets", nil, nil, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	type secret struct {
		Name    string `json:"name"`
		Updated string `json:"updated,omitempty"`
	}
	out := make([]secret, 0, len(raw))
	for _, r := range raw {
		// The API returns the values. They stay here: a secret that passes
		// through a model's context is no longer one.
		out = append(out, secret{Name: r.Name, Updated: r.UpdatedAt})
	}
	return sbResultJSON(map[string]any{"project": s.projectRef, "count": len(out), "secrets": out, "note": "values are never returned"})
}

func (s *Supabase) handleSecretsSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	values, err := objectArg(req, "secrets")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	envFile, err := stringArg(req, "env_file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	secrets := map[string]string{}
	var fromFile, skipped []string
	if envFile != "" {
		parsed, skip, err := sbReadEnvFile(envFile)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		skipped = skip
		for k, v := range parsed {
			secrets[k] = v
			fromFile = append(fromFile, k)
		}
	}
	for k, v := range values {
		str, ok := v.(string)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("secrets.%s must be a string, got %T", k, v)), nil
		}
		if _, dup := secrets[k]; dup {
			// The call wins; the name is no longer "from the file".
			for i, name := range fromFile {
				if name == k {
					fromFile = append(fromFile[:i], fromFile[i+1:]...)
					break
				}
			}
		}
		secrets[k] = str
	}
	if len(secrets) == 0 {
		return mcp.NewToolResultError("nothing to set: pass secrets (name → value) and/or env_file"), nil
	}
	if len(secrets) > sbSecretMaxBatch {
		return mcp.NewToolResultError(fmt.Sprintf("%d secrets in one call; the API takes at most %d — split the call", len(secrets), sbSecretMaxBatch)), nil
	}
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		if err := sbValidSecretName(name); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		names = append(names, name)
	}
	sortStrings(names)
	body := make([]map[string]string, 0, len(names))
	for _, name := range names {
		body = append(body, map[string]string{"name": name, "value": secrets[name]})
	}
	if _, err := s.callJSON(ctx, "secrets.create", nil, nil, body, nil); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{"project": s.projectRef, "set": names, "note": "functions see new values on their next invocation"}
	if len(fromFile) > 0 {
		sortStrings(fromFile)
		result["fromEnvFile"] = fromFile
	}
	if len(skipped) > 0 {
		result["skipped"] = skipped
		result["skippedReason"] = "names starting with SUPABASE_ are reserved by Supabase and not set"
	}
	return sbResultJSON(result)
}

func (s *Supabase) handleSecretsDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	names, err := stringListArg(req, "names")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(names) == 0 {
		return mcp.NewToolResultError("names is required (one or more secret names)"), nil
	}
	for _, name := range names {
		if err := sbValidSecretName(name); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	if _, err := s.callJSON(ctx, "secrets.delete", nil, nil, names, nil); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return sbResultJSON(map[string]any{"project": s.projectRef, "deleted": names})
}

// sbValidSecretName applies the API's rule (no SUPABASE_ prefix, at most 256
// characters) and the shape an environment variable has.
func sbValidSecretName(name string) error {
	if name == "" {
		return errors.New("a secret name is empty")
	}
	if len(name) > 256 {
		return fmt.Errorf("secret name %q is longer than 256 characters", name[:20]+"…")
	}
	if strings.HasPrefix(name, "SUPABASE_") {
		return fmt.Errorf("secret name %q: names starting with SUPABASE_ are reserved by Supabase", name)
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return fmt.Errorf("secret name %q is not an environment variable name (letters, digits, _; not starting with a digit)", name)
		}
	}
	return nil
}

// --- SQL ---

func (s *Supabase) handleSQLQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := stringArg(req, "query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(query) == "" {
		return mcp.NewToolResultError("query is required"), nil
	}
	var params []any
	if v, ok := req.GetArguments()["parameters"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("parameters must be an array, got %T", v)), nil
		}
		params = list
	}
	readOnly := s.readOnly
	if _, ok := req.GetArguments()["read_only"]; ok {
		if s.readOnly && !req.GetBool("read_only", true) {
			return mcp.NewToolResultError("this connection is read-only: read_only=false is not available"), nil
		}
		readOnly = readOnly || req.GetBool("read_only", false)
	}
	body := map[string]any{"query": query}
	if len(params) > 0 {
		body["parameters"] = params
	}
	key := "query"
	if readOnly {
		// The read-only endpoint runs as supabase_read_only_user — a
		// transaction-level guarantee, not a check of the statement text.
		key = "query.readonly"
	}
	var raw json.RawMessage
	if _, err := s.callJSON(ctx, key, nil, nil, body, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{"project": s.projectRef, "readOnly": readOnly}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) == nil {
		result["rowCount"] = len(rows)
		result["rows"] = rows
	} else {
		result["result"] = raw
	}
	return sbResultJSON(result)
}

// --- Logs ---

// sbLogSources are the sources of the unified log stream the logs tool
// offers, with the attributes worth selecting per source (the names the
// supabase MCP server selects, which is what the dashboard's Logs Explorer
// shows). function_logs is the console output of Edge Functions,
// function_edge_logs the requests that reached them.
var sbLogSources = map[string][]string{
	"function_logs":      {"level", "event_type", "function_id", "execution_id", "deployment_id", "version"},
	"function_edge_logs": {"response.status_code", "request.method", "request.url", "function_id", "execution_time_ms", "deployment_id", "version"},
	"edge_logs":          {"identifier", "request.method", "request.path", "response.status_code"},
	"postgres_logs":      {"identifier", "parsed.error_severity"},
	"auth_logs":          {"level", "status", "path", "msg", "error"},
	"storage_logs":       nil,
	"realtime_logs":      nil,
}

// sbLogsSourceColumn is the column of the unified logs table that names the
// source. The OpenAPI document and the supabase MCP server use source.
const sbLogsSourceColumn = "source"

// sbFunctionSources are the sources that carry function_id.
var sbFunctionSources = map[string]bool{"function_logs": true, "function_edge_logs": true}

func (s *Supabase) handleLogs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	source := strings.TrimSpace(req.GetString("source", "function_logs"))
	attrs, ok := sbLogSources[source]
	if !ok {
		return mcp.NewToolResultError(fmt.Sprintf("source %q is not offered; one of %s", source, strings.Join(sbLogSourceNames(), ", "))), nil
	}
	end := s.now().UTC()
	if v := strings.TrimSpace(req.GetString("end", "")); v != "" {
		t, err := sbParseTime(v)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("end: %v", err)), nil
		}
		end = t
	}
	start := end.Add(-sbLogsDefaultSpan)
	if v := strings.TrimSpace(req.GetString("start", "")); v != "" {
		t, err := sbParseTime(v)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("start: %v", err)), nil
		}
		start = t
	}
	if !start.Before(end) {
		return mcp.NewToolResultError("start must be before end"), nil
	}
	if end.Sub(start) > sbLogsMaxSpan {
		return mcp.NewToolResultError(fmt.Sprintf("the window %s – %s is longer than 24 hours, the API's limit — narrow it", start.Format(time.RFC3339), end.Format(time.RFC3339))), nil
	}
	limit := sbLogsDefaultRows
	if v, ok := args["limit"]; ok && v != nil {
		n, ok := v.(float64)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("limit must be a number, got %T", v)), nil
		}
		switch {
		case n >= sbLogsMaxRows:
			limit = sbLogsMaxRows
		case n >= 1:
			limit = int(n)
		default:
			return mcp.NewToolResultError("limit must be at least 1"), nil
		}
	}

	var sql string
	rawSQL, err := stringArg(req, "sql")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	ownSQL := strings.TrimSpace(rawSQL) != ""
	if ownSQL {
		for _, k := range []string{"function", "search"} {
			if v := strings.TrimSpace(req.GetString(k, "")); v != "" {
				return mcp.NewToolResultError(fmt.Sprintf("sql replaces the built query — %s cannot be combined with it", k)), nil
			}
		}
		sql = strings.TrimSpace(rawSQL)
	} else {
		where := []string{fmt.Sprintf("%s = '%s'", sbLogsSourceColumn, source)}
		if slug := strings.TrimSpace(req.GetString("function", "")); slug != "" {
			if !sbFunctionSources[source] {
				return mcp.NewToolResultError(fmt.Sprintf("function applies to function_logs and function_edge_logs, not to %s", source)), nil
			}
			if !sbValidSlug(slug) {
				return mcp.NewToolResultError(fmt.Sprintf("function %q is not a function slug", slug)), nil
			}
			fns, err := s.listFunctions(ctx)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("resolve function %s: %v", slug, err)), nil
			}
			id := ""
			for _, f := range fns {
				if f.Slug == slug {
					id = f.ID
					break
				}
			}
			if id == "" {
				return mcp.NewToolResultError(fmt.Sprintf("function %q is not deployed in project %s", slug, s.projectRef)), nil
			}
			where = append(where, fmt.Sprintf("log_attributes['function_id'] = '%s'", sbSQLString(id)))
		}
		if search := strings.TrimSpace(req.GetString("search", "")); search != "" {
			where = append(where, fmt.Sprintf("positionCaseInsensitive(event_message, '%s') > 0", sbSQLString(search)))
		}
		cols := []string{"timestamp", "event_message"}
		if source == "function_logs" {
			cols = append(cols, "severity_text")
		}
		for _, a := range attrs {
			cols = append(cols, fmt.Sprintf("log_attributes['%s'] AS \"%s\"", a, a))
		}
		sql = fmt.Sprintf("SELECT %s FROM logs WHERE %s ORDER BY timestamp DESC LIMIT %d", strings.Join(cols, ", "), strings.Join(where, " AND "), limit)
	}

	q := url.Values{}
	q.Set("sql", sql)
	q.Set("iso_timestamp_start", start.UTC().Format(time.RFC3339))
	q.Set("iso_timestamp_end", end.UTC().Format(time.RFC3339))
	var raw struct {
		Result []map[string]any `json:"result"`
		Error  any              `json:"error"`
	}
	if _, err := s.callJSON(ctx, "logs", nil, q, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if raw.Error != nil {
		msg, _ := json.Marshal(raw.Error)
		return mcp.NewToolResultError(fmt.Sprintf("logs query refused: %s (SQL was: %s)", string(msg), sql)), nil
	}
	// Function and edge-request rows carry the function's id; rows of the
	// unified table name the timestamp in microseconds since the epoch.
	for _, row := range raw.Result {
		switch ts := row["timestamp"].(type) {
		case float64:
			if ts > 1e15 {
				row["timestamp"] = time.UnixMicro(int64(ts)).UTC().Format(time.RFC3339Nano)
			}
		case string:
			// The stream answers "2026-11-01T13:04:01.230000" — UTC without
			// saying so (seen live 01.11.2026); say so.
			if sbNaiveTimestamp.MatchString(ts) {
				row["timestamp"] = ts + "Z"
			}
		}
	}
	result := map[string]any{
		"project":  s.projectRef,
		"start":    start.UTC().Format(time.RFC3339),
		"end":      end.UTC().Format(time.RFC3339),
		"rowCount": len(raw.Result),
		"rows":     raw.Result,
		"sql":      sql,
	}
	if !ownSQL {
		result["source"] = source
		if len(raw.Result) >= limit {
			result["note"] = fmt.Sprintf("the limit of %d rows is full — narrow the window or raise limit (max %d)", limit, sbLogsMaxRows)
		}
	}
	return sbResultJSON(result)
}

func sbLogSourceNames() []string {
	names := make([]string, 0, len(sbLogSources))
	for n := range sbLogSources {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// sbSQLString escapes a value for a single-quoted ClickHouse string literal.
func sbSQLString(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	return strings.ReplaceAll(v, `'`, `\'`)
}

// sbNaiveTimestamp is an ISO timestamp without zone, as the logs stream
// renders them.
var sbNaiveTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?$`)

// sbParseTime accepts RFC 3339 and "YYYY-MM-DD HH:MM" / "YYYY-MM-DD" in UTC.
func sbParseTime(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a timestamp (RFC 3339, or YYYY-MM-DD HH:MM in UTC)", v)
}

// --- Function delete ---

func (s *Supabase) handleFunctionDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	slug, err := sbSlugArg(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	before, err := s.getFunction(ctx, slug)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if before == nil {
		return mcp.NewToolResultError(fmt.Sprintf("function %q is not deployed in project %s — nothing to delete", slug, s.projectRef)), nil
	}
	if _, err := s.callJSON(ctx, "function.delete", map[string]string{"function_slug": slug}, nil, nil, nil); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return sbResultJSON(map[string]any{
		"project": s.projectRef,
		"deleted": renderSBFunction(*before),
		"note":    "the function's invocations fail from now on; redeploying creates version 1 again",
	})
}
