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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	// The token owner's profile time zone has to resolve on every host mux runs
	// on — the distroless container image and Windows ship no zoneinfo.
	_ "time/tzdata"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// YouTrack REST connection type.
//
// The type speaks the YouTrack REST API directly and shapes it for a model:
// readable IDs instead of database IDs, ISO timestamps in the token owner's
// time zone instead of epoch milliseconds, custom fields as a name → value
// map instead of polymorphic field objects, comments paged to completion,
// attachments as image content or files. Every REST call goes through the
// endpoint registry below (ytEndpoints), which is what the spec test in
// youtrack_rest_spec_test.go checks against YouTrack's published OpenAPI
// document — drift in the upstream API is caught by a test, not by a user.
//
// Write guards (protected and set-once fields) live here as well, so they
// hold for every client and every agent, not only for the ones that read a
// connection's instructions. The meaning of a field stays in the instructions;
// the mechanics stay in code.

const (
	youtrackMaxBody        = 8 * 1024 * 1024 // JSON responses — issue lists with full custom fields are large
	youtrackInlineImageMax = 5 * 1024 * 1024 // attachments up to this size are returned as MCP image content
	youtrackInlineTextMax  = 256 * 1024      // textual attachments up to this size are inlined
	youtrackAdminInlineMax = 1024 * 1024     // raw admin responses are inlined up to this size
	youtrackPageSize       = 100             // $top per page when paging a collection to completion
	youtrackMaxPages       = 50              // 5000 items — beyond that a caller should narrow the query
	youtrackSearchMaxTop   = 200             // search_issues cap per call; $skip pages further
	youtrackListTop        = 1000            // lookups that are listed in full (projects, groups, boards)
	youtrackDateOnlyHour   = 12              // YouTrack stores date-only field values at 12:00 UTC
	youtrackCommandRereads = 10              // issues re-read after run_command to show the effect
	youtrackRequestTimeout = 60 * time.Second
)

// DefaultYouTrackRESTInstructions are shown to AI clients when a connection
// has no instructions of its own. They describe mechanics only — what a
// project's fields mean belongs in the connection's instructions.
const DefaultYouTrackRESTInstructions = `YouTrack via its REST API. Address issues by their readable ID (e.g. PROJ-123).
Timestamps are ISO 8601 in the token owner's YouTrack time zone; date-only fields are YYYY-MM-DD.
Custom fields come back as a name → value map: users as login, enums and states by name, periods as presentation plus minutes.
get_project lists a project's fields with their allowed values — consult it before writing fields with create_issue or update_issue.
Write tools are absent on read-only connections. Protected and set-once fields are refused with a message that names the guard.`

// YouTrackREST wraps the YouTrack REST API as MCP tools.
type YouTrackREST struct {
	client       *http.Client
	baseURL      string // https://instance.youtrack.cloud or https://host/youtrack — no trailing slash, no /api
	apiURL       string // baseURL + "/api"
	rootURL      string // scheme://host — attachment URLs and Hub (/hub/api/rest) are host-relative
	token        string
	readOnly     bool
	instructions string
	protected    []ytGuard
	setOnce      []ytGuard
	outputDir    string

	tzMu sync.Mutex
	tz   *time.Location // token owner's profile time zone, loaded lazily
}

// NewYouTrackREST creates a YouTrack REST connection from config.
func NewYouTrackREST(conn config.Connection, dialer Dialer) (*YouTrackREST, error) {
	if conn.URL == "" {
		return nil, fmt.Errorf("youtrack-rest: base URL is required")
	}
	if conn.Token == "" {
		return nil, fmt.Errorf("youtrack-rest: permanent token is required")
	}
	baseURL, rootURL, err := YouTrackBaseURL(conn.URL)
	if err != nil {
		return nil, err
	}
	protected, err := parseYouTrackGuards(conn.ProtectedFields)
	if err != nil {
		return nil, fmt.Errorf("youtrack-rest: protected_fields: %w", err)
	}
	setOnce, err := parseYouTrackGuards(conn.SetOnceFields)
	if err != nil {
		return nil, fmt.Errorf("youtrack-rest: set_once_fields: %w", err)
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &YouTrackREST{
		client: &http.Client{
			Transport: transport,
			Timeout:   youtrackRequestTimeout,
		},
		baseURL:      baseURL,
		apiURL:       baseURL + "/api",
		rootURL:      rootURL,
		token:        conn.Token,
		readOnly:     conn.ReadOnly,
		instructions: conn.Instructions,
		protected:    protected,
		setOnce:      setOnce,
		outputDir:    filepath.Join(config.Dir(), "output"),
	}, nil
}

// YouTrackBaseURL normalizes the configured URL. It accepts the forms users
// copy from the browser (with or without a trailing slash or /api) and
// rejects the MCP endpoint, which belongs to the proxy type — a base URL
// ending in /mcp would make every call 404 with a misleading message.
func YouTrackBaseURL(raw string) (base, root string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("youtrack-rest: url must be an absolute http(s) URL, got %q", raw)
	}
	p := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(p, "/mcp") {
		return "", "", fmt.Errorf("youtrack-rest: %q is the MCP endpoint — configure the YouTrack base URL without /mcp (the MCP proxy is type %q)", raw, config.TypeYouTrack)
	}
	p = strings.TrimSuffix(p, "/api")
	root = u.Scheme + "://" + u.Host
	return root + p, root, nil
}

// --- Endpoint registry ---

// ytEndpoint is one REST call the type makes. Path is the OpenAPI template
// under /api; Fields is the fields= selector sent with the call; Params are
// the other query parameters used. The spec test resolves every entry
// against YouTrack's OpenAPI document, so a renamed path, a dropped parameter
// or a removed entity field fails a test instead of a user's call.
type ytEndpoint struct {
	Method string
	Path   string
	Fields string
	Params []string
	// Undocumented marks an endpoint that works but is absent from
	// openapi.json. The spec test tolerates it; the drift workflow reports
	// when it appears (or when a documented one disappears).
	Undocumented bool
	// UndocumentedParams are query parameters the API accepts although the
	// OpenAPI document does not list them for the operation (/users?query=).
	// The spec test checks that they are still undocumented, so the entry
	// moves to Params the day JetBrains documents them.
	UndocumentedParams []string
	// Raw marks the admin pass-through: the caller supplies the path, so
	// there is nothing to resolve against the spec.
	Raw bool
}

// Field selectors. Nested selectors on polymorphic properties (custom field
// values, activity added/removed) list the union of what the subtypes carry;
// YouTrack returns what exists on each instance.
const (
	ytMeFields           = "id,login,fullName,email,profiles(general(timezone(id,offset)))"
	ytUserFields         = "id,login,fullName,email,banned"
	ytGroupFields        = "id,name"
	ytCustomFieldFields  = "id,name,$type,value(id,name,login,fullName,presentation,minutes,text,isResolved,archived,$type),projectCustomField(field(name,fieldType(id)))"
	ytAttachmentFields   = "id,name,size,mimeType,extension,created,author(login),url,comment(id)"
	ytLinkFields         = "id,direction,linkType(id,name,sourceToTarget,targetToSource,directed),issues(id,idReadable,summary)"
	ytCommentFields      = "id,text,created,updated,author(login,fullName),deleted,visibility($type,permittedGroups(name),permittedUsers(login)),attachments(id,name,size,mimeType)"
	ytIssueFields        = "id,idReadable,summary,description,created,updated,resolved,project(id,shortName,name),reporter(login,fullName),updater(login),tags(name),visibility($type,permittedGroups(name),permittedUsers(login)),watchers(hasStar,issueWatchers(user(login))),links(" + ytLinkFields + "),attachments(" + ytAttachmentFields + "),commentsCount,customFields(" + ytCustomFieldFields + ")"
	ytIssueCompactFields = "id,idReadable,summary,created,updated,resolved,project(shortName),reporter(login),customFields(" + ytCustomFieldFields + ")"
	ytIssueSearchFields  = ytIssueCompactFields + ",description"
	ytIssueGuardFields   = "id,idReadable,project(id,shortName),customFields(name,$type,value(id,name,login))"
	ytIssueWatchFields   = "id,idReadable,watchers(hasStar,issueWatchers(user(login)))"
	ytActivityFields     = "id,$type,timestamp,author(login,fullName),category(id),target($type,id,idReadable),field(presentation,id,$type),added(id,$type,name,login,presentation,text,idReadable,summary),removed(id,$type,name,login,presentation,text,idReadable,summary)"
	ytWorkItemFields     = "id,date,duration(minutes,presentation),text,type(id,name),author(login),creator(login),created"
	ytWorkItemTypeFields = "id,name"
	ytProjectFields      = "id,shortName,name,archived,leader(login)"
	ytProjectFullFields  = "id,shortName,name,description,archived,leader(login,fullName),createdBy(login)"
	ytProjectFieldFields = "id,$type,canBeEmpty,emptyFieldText,isPublic,field(id,name,fieldType(id)),bundle(id,$type,values(id,name,isResolved,archived))"
	ytLinkTypeFields     = "id,name,sourceToTarget,targetToSource,directed,aggregation"
	ytAgileFields        = "id,name,projects(shortName),currentSprint(id,name),sprintsSettings(disableSprints)"
	ytSprintFields       = "id,name,start,finish,goal,archived,isDefault,unresolvedIssuesCount"
	ytCommandFields      = "issues(idReadable)"
	ytAssistFields       = "commands(description,error),suggestions(option,description,prefix,suffix)"
	ytIDFields           = "id,idReadable"
)

// ytEndpoints is the complete list of REST calls the type can make. Handlers
// address entries by key; call refuses anything that is not listed.
var ytEndpoints = map[string]ytEndpoint{
	"me":                    {Method: http.MethodGet, Path: "/users/me", Fields: ytMeFields},
	"users":                 {Method: http.MethodGet, Path: "/users", Fields: ytUserFields, Params: []string{"$top"}, UndocumentedParams: []string{"query"}},
	"groups":                {Method: http.MethodGet, Path: "/groups", Fields: ytGroupFields, Params: []string{"$top", "$skip"}},
	"issues.search":         {Method: http.MethodGet, Path: "/issues", Fields: ytIssueSearchFields, Params: []string{"query", "$top", "$skip"}},
	"issues.compact":        {Method: http.MethodGet, Path: "/issues", Fields: ytIssueCompactFields, Params: []string{"query", "$top", "$skip"}},
	"issue.get":             {Method: http.MethodGet, Path: "/issues/{id}", Fields: ytIssueFields},
	"issue.compact":         {Method: http.MethodGet, Path: "/issues/{id}", Fields: ytIssueCompactFields},
	"issue.guard":           {Method: http.MethodGet, Path: "/issues/{id}", Fields: ytIssueGuardFields},
	"issue.id":              {Method: http.MethodGet, Path: "/issues/{id}", Fields: ytIDFields},
	"issue.watchers":        {Method: http.MethodGet, Path: "/issues/{id}", Fields: ytIssueWatchFields},
	"issue.create":          {Method: http.MethodPost, Path: "/issues", Fields: ytIssueFields},
	"issue.update":          {Method: http.MethodPost, Path: "/issues/{id}", Fields: ytIssueFields},
	"issue.comments":        {Method: http.MethodGet, Path: "/issues/{id}/comments", Fields: ytCommentFields, Params: []string{"$top", "$skip"}},
	"issue.comment.add":     {Method: http.MethodPost, Path: "/issues/{id}/comments", Fields: ytCommentFields},
	"issue.activities":      {Method: http.MethodGet, Path: "/issues/{id}/activities", Fields: ytActivityFields, Params: []string{"categories", "$top", "$skip", "reverse", "start", "end"}},
	"activities":            {Method: http.MethodGet, Path: "/activities", Fields: ytActivityFields, Params: []string{"issueQuery", "categories", "$top", "$skip", "reverse", "start", "end", "author"}},
	"issue.attachments":     {Method: http.MethodGet, Path: "/issues/{id}/attachments", Fields: ytAttachmentFields},
	"issue.attachment.add":  {Method: http.MethodPost, Path: "/issues/{id}/attachments", Fields: ytAttachmentFields},
	"issue.links":           {Method: http.MethodGet, Path: "/issues/{id}/links", Fields: ytLinkFields},
	"issue.link.add":        {Method: http.MethodPost, Path: "/issues/{id}/links/{issueLinkId}/issues", Fields: ytIDFields},
	"issue.workitems":       {Method: http.MethodGet, Path: "/issues/{id}/timeTracking/workItems", Fields: ytWorkItemFields, Params: []string{"$top", "$skip"}},
	"issue.workitem.add":    {Method: http.MethodPost, Path: "/issues/{id}/timeTracking/workItems", Fields: ytWorkItemFields},
	"issue.watcher.add":     {Method: http.MethodPost, Path: "/issues/{id}/watchers/issueWatchers", Fields: "user(login),isStarred", Undocumented: true},
	"projects":              {Method: http.MethodGet, Path: "/admin/projects", Fields: ytProjectFields, Params: []string{"$top", "$skip"}},
	"project.get":           {Method: http.MethodGet, Path: "/admin/projects/{id}", Fields: ytProjectFullFields},
	"project.fields":        {Method: http.MethodGet, Path: "/admin/projects/{id}/customFields", Fields: ytProjectFieldFields, Params: []string{"$top"}},
	"project.workitemtypes": {Method: http.MethodGet, Path: "/admin/projects/{id}/timeTrackingSettings/workItemTypes", Fields: ytWorkItemTypeFields},
	"linktypes":             {Method: http.MethodGet, Path: "/issueLinkTypes", Fields: ytLinkTypeFields},
	"agiles":                {Method: http.MethodGet, Path: "/agiles", Fields: ytAgileFields, Params: []string{"$top"}},
	"agile.sprint":          {Method: http.MethodGet, Path: "/agiles/{id}/sprints/{sprintId}", Fields: ytSprintFields},
	"agile.sprints":         {Method: http.MethodGet, Path: "/agiles/{id}/sprints", Fields: ytSprintFields, Params: []string{"$top"}},
	"commands":              {Method: http.MethodPost, Path: "/commands", Fields: ytCommandFields},
	"commands.assist":       {Method: http.MethodPost, Path: "/commands/assist", Fields: ytAssistFields},
	"admin.raw":             {Raw: true},
}

// --- HTTP ---

// call performs a registered call. pathParams fill the {placeholders} of the
// endpoint's path template; query carries the parameters listed in Params.
// The registry is the only way to reach the API, so the spec test sees every
// path and selector the type can send.
func (y *YouTrackREST) call(ctx context.Context, key string, pathParams map[string]string, query url.Values, body any) ([]byte, int, error) {
	ep, ok := ytEndpoints[key]
	if !ok || ep.Raw {
		return nil, 0, fmt.Errorf("youtrack-rest: endpoint %q is not registered", key)
	}
	path := ep.Path
	for k, v := range pathParams {
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
	}
	if strings.Contains(path, "{") {
		return nil, 0, fmt.Errorf("youtrack-rest: endpoint %q has an unfilled path parameter: %s", key, path)
	}
	q := url.Values{}
	for k, vs := range query {
		q[k] = vs
	}
	if ep.Fields != "" {
		q.Set("fields", ep.Fields)
	}
	u := y.apiURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return y.do(ctx, ep.Method, u, body)
}

// callJSON performs a registered call, requires a 2xx status and decodes the
// body into out (nil out discards the body).
func (y *YouTrackREST) callJSON(ctx context.Context, key string, pathParams map[string]string, query url.Values, body, out any) error {
	data, status, err := y.call(ctx, key, pathParams, query, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return ytAPIError(status, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode YouTrack response: %w", err)
	}
	return nil
}

// callPages pages a collection endpoint to completion with $top/$skip and
// returns the raw elements. It stops after youtrackMaxPages pages so a huge
// collection cannot run away.
func (y *YouTrackREST) callPages(ctx context.Context, key string, pathParams map[string]string, query url.Values) ([]json.RawMessage, bool, error) {
	var all []json.RawMessage
	for page := 0; page < youtrackMaxPages; page++ {
		q := url.Values{}
		for k, vs := range query {
			q[k] = vs
		}
		q.Set("$top", strconv.Itoa(youtrackPageSize))
		q.Set("$skip", strconv.Itoa(page*youtrackPageSize))
		var items []json.RawMessage
		if err := y.callJSON(ctx, key, pathParams, q, nil, &items); err != nil {
			return nil, false, err
		}
		all = append(all, items...)
		if len(items) < youtrackPageSize {
			return all, true, nil
		}
	}
	return all, false, nil
}

// do sends one request with the connection's token and reads the body up to
// youtrackMaxBody. The status is returned alongside so callers can shape
// their own error.
func (y *YouTrackREST) do(ctx context.Context, method, fullURL string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+y.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := y.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, youtrackMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return data, resp.StatusCode, nil
}

// download fetches a host-relative file URL (attachments) with the token and
// returns the open response for streaming. The caller closes the body.
func (y *YouTrackREST) download(ctx context.Context, relURL string) (*http.Response, error) {
	full := relURL
	if strings.HasPrefix(relURL, "/") {
		full = y.rootURL + relURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+y.token)
	resp, err := y.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, ytAPIError(resp.StatusCode, data)
	}
	return resp, nil
}

// ytAPIError turns a YouTrack error body ({"error", "error_description"})
// into a readable message; anything else is quoted as is, truncated.
func ytAPIError(status int, body []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) == nil && (e.Error != "" || e.Description != "") {
		msg := e.Error
		if e.Description != "" && e.Description != e.Error {
			if msg != "" {
				msg += ": "
			}
			msg += e.Description
		}
		return fmt.Errorf("YouTrack API error (HTTP %d): %s", status, msg)
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 2048 {
		text = text[:2048] + "…"
	}
	return fmt.Errorf("YouTrack API error (HTTP %d): %s", status, text)
}

// --- Time zone and dates ---

// location returns the token owner's YouTrack profile time zone. It is
// loaded once from /users/me and cached; without a resolvable zone the type
// renders in UTC and says so in the log. The failed lookup is not cached, so
// a transient error does not pin the connection to UTC.
func (y *YouTrackREST) location(ctx context.Context) *time.Location {
	y.tzMu.Lock()
	defer y.tzMu.Unlock()
	if y.tz != nil {
		return y.tz
	}
	var me struct {
		Login    string `json:"login"`
		Profiles struct {
			General struct {
				Timezone struct {
					ID string `json:"id"`
				} `json:"timezone"`
			} `json:"general"`
		} `json:"profiles"`
	}
	if err := y.callJSON(ctx, "me", nil, nil, nil, &me); err != nil {
		log.Printf("[youtrack-rest] profile time zone unavailable (%v) — rendering in UTC", err)
		return time.UTC
	}
	id := me.Profiles.General.Timezone.ID
	if id == "" {
		log.Printf("[youtrack-rest] profile of %q carries no time zone — rendering in UTC", me.Login)
		y.tz = time.UTC
		return y.tz
	}
	loc, err := time.LoadLocation(id)
	if err != nil {
		log.Printf("[youtrack-rest] unknown profile time zone %q (%v) — rendering in UTC", id, err)
		y.tz = time.UTC
		return y.tz
	}
	y.tz = loc
	return y.tz
}

// ytTime renders an epoch-millisecond timestamp as ISO 8601 in loc.
func ytTime(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format(time.RFC3339)
}

// ytTimePtr is ytTime for optional timestamps (resolved).
func ytTimePtr(ms *int64, loc *time.Location) *string {
	if ms == nil || *ms == 0 {
		return nil
	}
	s := ytTime(*ms, loc)
	return &s
}

// ytDate renders a date-only value (a timestamp YouTrack stores at 12:00
// UTC) as YYYY-MM-DD in loc. Noon UTC falls on the same calendar day in
// every zone between UTC-11 and UTC+11, so the profile zone only matters for
// values other clients wrote at some other hour.
func ytDate(ms int64, loc *time.Location) string {
	return time.UnixMilli(ms).In(loc).Format("2006-01-02")
}

// ytParseDateOnly converts YYYY-MM-DD into the millisecond value YouTrack
// uses for date-type custom fields: 12:00 UTC of that day (verified against
// the stored values of a production instance). Writing midnight in any zone
// instead is what produced the off-by-one day of the MCP proxy.
func ytParseDateOnly(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("date %q: expected YYYY-MM-DD", s)
	}
	return t.Add(youtrackDateOnlyHour * time.Hour).UnixMilli(), nil
}

// ytParseWorkDate converts YYYY-MM-DD into the value YouTrack stores for a
// work item's date: 00:00 UTC of that day (verified on a production instance
// — a different convention from date-type custom fields).
func ytParseWorkDate(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("date %q: expected YYYY-MM-DD", s)
	}
	return t.UnixMilli(), nil
}

// ytParseDateTime parses a timestamp for "date and time" fields and
// activity ranges: RFC 3339 with offset, or a local wall-clock time
// ("2006-01-02 15:04", "2006-01-02T15:04", with optional seconds) read in
// loc, or a bare date read as midnight in loc.
func ytParseDateTime(s string, loc *time.Location) (int64, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("timestamp %q: expected RFC 3339 (2026-10-15T14:30:00+02:00) or YYYY-MM-DD HH:MM in the profile time zone", s)
}

// ytToMillis accepts either a formatted value or a raw millisecond number for
// the date parameters of the read tools.
func ytToMillis(v any, loc *time.Location, dateOnly bool) (int64, error) {
	switch t := v.(type) {
	case float64:
		return int64(t), nil
	case json.Number:
		return t.Int64()
	case string:
		if dateOnly {
			return ytParseDateOnly(t)
		}
		return ytParseDateTime(t, loc)
	default:
		return 0, fmt.Errorf("expected a string or a millisecond number, got %T", v)
	}
}

var ytPeriodToken = regexp.MustCompile(`^(\d+)\s*([hm])$`)

// ytParsePeriod converts a duration into minutes. Accepted: a number of
// minutes, "90", "1h 30m", "1h30m", "2h", "45m". Days and weeks are refused
// because their length is an instance setting (hours per day, days per week)
// the type cannot know.
func ytParsePeriod(v any) (int, error) {
	switch t := v.(type) {
	case float64:
		if t <= 0 || t != float64(int(t)) {
			return 0, fmt.Errorf("duration must be a positive whole number of minutes, got %v", t)
		}
		return int(t), nil
	case json.Number:
		n, err := t.Int64()
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("duration must be a positive whole number of minutes, got %s", t)
		}
		return int(n), nil
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		if s == "" {
			return 0, errors.New("duration is empty")
		}
		if n, err := strconv.Atoi(s); err == nil {
			if n <= 0 {
				return 0, fmt.Errorf("duration must be positive, got %d", n)
			}
			return n, nil
		}
		// Split "1h30m" into tokens before the unit letters.
		s = strings.ReplaceAll(s, "h", "h ")
		s = strings.ReplaceAll(s, "m", "m ")
		total := 0
		for _, tok := range strings.Fields(s) {
			m := ytPeriodToken.FindStringSubmatch(tok)
			if m == nil {
				if strings.HasSuffix(tok, "d") || strings.HasSuffix(tok, "w") {
					return 0, fmt.Errorf("duration %q: days and weeks depend on the instance's time tracking settings — give hours and minutes", t)
				}
				return 0, fmt.Errorf("duration %q: expected minutes or a combination like \"1h 30m\"", t)
			}
			n, _ := strconv.Atoi(m[1])
			if m[2] == "h" {
				total += n * 60
			} else {
				total += n
			}
		}
		if total <= 0 {
			return 0, fmt.Errorf("duration %q is zero", t)
		}
		return total, nil
	default:
		return 0, fmt.Errorf("duration: expected a string or a number, got %T", v)
	}
}

// --- Write guards ---

// ytGuard names a field in a project (Project "" = every project).
type ytGuard struct {
	Project string
	Field   string
}

// parseYouTrackGuards reads "Project:Field" and "Field" entries. "*" stands
// for every project. Field names keep their spelling; matching is
// case-insensitive.
func parseYouTrackGuards(entries []string) ([]ytGuard, error) {
	var out []ytGuard
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		project, field, found := strings.Cut(e, ":")
		if !found {
			field, project = project, ""
		}
		project = strings.TrimSpace(project)
		field = strings.TrimSpace(field)
		if project == "*" {
			project = ""
		}
		if field == "" {
			return nil, fmt.Errorf("entry %q names no field (expected \"Project:Field\" or \"Field\")", e)
		}
		out = append(out, ytGuard{Project: project, Field: field})
	}
	return out, nil
}

func (g ytGuard) matches(project, field string) bool {
	if g.Project != "" && !strings.EqualFold(g.Project, project) {
		return false
	}
	return strings.EqualFold(g.Field, field)
}

// isProtected reports whether a field may never be written in a project.
func (y *YouTrackREST) isProtected(project, field string) bool {
	for _, g := range y.protected {
		if g.matches(project, field) {
			return true
		}
	}
	return false
}

// isSetOnce reports whether a field may only be written while empty.
func (y *YouTrackREST) isSetOnce(project, field string) bool {
	for _, g := range y.setOnce {
		if g.matches(project, field) {
			return true
		}
	}
	return false
}

func ytProtectedError(project, field string) error {
	return fmt.Errorf("field %q in project %s is protected on this connection (protected_fields) — not written; the value is decided outside this channel", field, project)
}

func ytSetOnceError(project, field string, issue string) error {
	return fmt.Errorf("field %q in project %s is set-once on this connection (set_once_fields) and already has a value on %s — not written", field, project, issue)
}

// ytCommandMentions reports whether a YouTrack command names a field: a
// case-insensitive match on word boundaries. Field names with spaces
// ("Sprint priority") match as a phrase. Conservative by design — a refused
// command names the guard, a missed one would write the field.
func ytCommandMentions(command, field string) bool {
	c := strings.ToLower(command)
	f := strings.ToLower(strings.TrimSpace(field))
	if f == "" {
		return false
	}
	for start := 0; ; {
		i := strings.Index(c[start:], f)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(f)
		before := i == 0 || !ytWordRune(rune(c[i-1]))
		after := end == len(c) || !ytWordRune(rune(c[end]))
		if before && after {
			return true
		}
		start = i + 1
	}
}

func ytWordRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127
}

// --- Tool argument helpers ---

// objectArg reads an argument declared as a JSON object. A client that sends
// the object as a JSON string (common with weaker tool-calling) is accepted
// as well; any other type is an error rather than silently empty.
func objectArg(req mcp.CallToolRequest, key string) (map[string]any, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case map[string]any:
		return t, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("%s must be a JSON object: %w", key, err)
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%s must be a JSON object, got %T", key, v)
	}
}

// stringListArg reads an argument declared as an array of strings. A JSON
// string holding an array, or a single plain string, is accepted as well.
func stringListArg(req mcp.CallToolRequest, key string) ([]string, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s[%d] must be a string, got %T", key, i, e)
			}
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	case []string:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil, nil
		}
		if strings.HasPrefix(s, "[") {
			var out []string
			if err := json.Unmarshal([]byte(s), &out); err != nil {
				return nil, fmt.Errorf("%s must be an array of strings: %w", key, err)
			}
			return out, nil
		}
		return []string{s}, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings, got %T", key, v)
	}
}

// ytResultJSON renders a value as an indented JSON tool result.
func ytResultJSON(v any) (*mcp.CallToolResult, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encode result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

// ytClamp bounds a caller-supplied page size.
func ytClamp(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
