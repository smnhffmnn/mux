package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Read handlers. Every handler returns a tool error (not a Go error) for API
// failures so the model sees the message.

func (y *YouTrackREST) handleMe(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var me struct {
		ID       string `json:"id"`
		Login    string `json:"login"`
		Name     string `json:"fullName"`
		Email    string `json:"email"`
		Profiles struct {
			General struct {
				Timezone struct {
					ID     string `json:"id"`
					Offset int    `json:"offset"`
				} `json:"timezone"`
			} `json:"general"`
		} `json:"profiles"`
	}
	if err := y.callJSON(ctx, "me", nil, nil, nil, &me); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return ytResultJSON(map[string]any{
		"id":        me.ID,
		"login":     me.Login,
		"name":      me.Name,
		"email":     me.Email,
		"time_zone": me.Profiles.General.Timezone.ID,
		"base_url":  y.baseURL,
		"read_only": y.readOnly,
	})
}

func (y *YouTrackREST) handleSearchIssues(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if strings.TrimSpace(query) == "" {
		return mcp.NewToolResultError("query is required (YouTrack search syntax, e.g. \"project: P #Unresolved sort by: updated desc\")"), nil
	}
	top := ytClamp(req.GetInt("top", 50), 50, youtrackSearchMaxTop)
	skip := req.GetInt("skip", 0)
	if skip < 0 {
		skip = 0
	}
	key := "issues.compact"
	if req.GetBool("include_description", false) {
		key = "issues.search"
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("$top", strconv.Itoa(top))
	q.Set("$skip", strconv.Itoa(skip))
	var raw []ytRawIssue
	if err := y.callJSON(ctx, key, nil, q, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	loc := y.location(ctx)
	issues := make([]ytIssueOut, 0, len(raw))
	for _, i := range raw {
		out := y.renderIssue(i, loc, false)
		if key == "issues.compact" {
			out.Description = nil
		}
		issues = append(issues, out)
	}
	out := map[string]any{
		"query":  query,
		"count":  len(issues),
		"skip":   skip,
		"top":    top,
		"issues": issues,
	}
	if len(issues) == top {
		out["more"] = fmt.Sprintf("page is full — call again with skip=%d for the next %d", skip+top, top)
	}
	return ytResultJSON(out)
}

func (y *YouTrackREST) handleGetIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, _ := req.RequireString("issue")
	if id == "" {
		return mcp.NewToolResultError("issue is required (readable ID, e.g. PROJ-123)"), nil
	}
	var raw ytRawIssue
	if err := y.callJSON(ctx, "issue.get", map[string]string{"id": id}, nil, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	loc := y.location(ctx)
	out := y.renderIssue(raw, loc, true)
	if !req.GetBool("include_description", true) {
		out.Description = nil
	}
	if req.GetBool("include_comments", true) && raw.CommentsCount > 0 {
		comments, complete, err := y.fetchComments(ctx, raw.IDReadable)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		for _, c := range comments {
			out.Comments = append(out.Comments, renderComment(c, loc))
		}
		if !complete {
			return ytResultJSON(map[string]any{"issue": out, "note": fmt.Sprintf("comments truncated after %d — use get_comments with skip to read on", len(comments))})
		}
	}
	return ytResultJSON(out)
}

// fetchComments pages an issue's comments to completion.
func (y *YouTrackREST) fetchComments(ctx context.Context, issue string) ([]ytRawComment, bool, error) {
	pages, complete, err := y.callPages(ctx, "issue.comments", map[string]string{"id": issue}, nil)
	if err != nil {
		return nil, false, err
	}
	out := make([]ytRawComment, 0, len(pages))
	for _, p := range pages {
		var c ytRawComment
		if err := json.Unmarshal(p, &c); err != nil {
			return nil, false, fmt.Errorf("decode comment: %w", err)
		}
		out = append(out, c)
	}
	return out, complete, nil
}

func (y *YouTrackREST) handleGetComments(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, _ := req.RequireString("issue")
	if id == "" {
		return mcp.NewToolResultError("issue is required"), nil
	}
	loc := y.location(ctx)
	skip := req.GetInt("skip", 0)
	top := req.GetInt("top", 0)
	var raw []ytRawComment
	complete := true
	if top > 0 || skip > 0 {
		q := url.Values{}
		q.Set("$top", strconv.Itoa(ytClamp(top, youtrackPageSize, 500)))
		q.Set("$skip", strconv.Itoa(skip))
		if err := y.callJSON(ctx, "issue.comments", map[string]string{"id": id}, q, nil, &raw); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	} else {
		var err error
		raw, complete, err = y.fetchComments(ctx, id)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	comments := make([]ytCommentOut, 0, len(raw))
	for _, c := range raw {
		comments = append(comments, renderComment(c, loc))
	}
	out := map[string]any{"issue": id, "count": len(comments), "comments": comments}
	if !complete {
		out["note"] = fmt.Sprintf("stopped after %d comments — call again with skip=%d", len(comments), len(comments))
	}
	return ytResultJSON(out)
}

// ytDefaultActivityCategories is what get_activities asks for when the caller
// names none: everything a person reading an issue's history expects.
const ytDefaultActivityCategories = "IssueCreatedCategory,CustomFieldCategory,CommentsCategory,AttachmentsCategory,LinksCategory,IssueResolvedCategory,SummaryCategory,DescriptionCategory,TagsCategory,SprintCategory,WorkItemCategory,ProjectCategory,VisibilityCategory"

func (y *YouTrackREST) handleGetActivities(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue := req.GetString("issue", "")
	query := req.GetString("query", "")
	if issue == "" && query == "" {
		return mcp.NewToolResultError("give issue (one issue's history) or query (a YouTrack search whose issues' activities are listed)"), nil
	}
	loc := y.location(ctx)
	q := url.Values{}
	cats := strings.TrimSpace(req.GetString("categories", ""))
	if cats == "" {
		cats = ytDefaultActivityCategories
	}
	q.Set("categories", strings.ReplaceAll(cats, " ", ""))
	q.Set("$top", strconv.Itoa(ytClamp(req.GetInt("top", 100), 100, 500)))
	if skip := req.GetInt("skip", 0); skip > 0 {
		q.Set("$skip", strconv.Itoa(skip))
	}
	if req.GetBool("newest_first", false) {
		q.Set("reverse", "true")
	}
	for _, p := range []string{"start", "end"} {
		if v, ok := req.GetArguments()[p]; ok && v != nil {
			ms, err := ytToMillis(v, loc, false)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s: %v", p, err)), nil
			}
			q.Set(p, strconv.FormatInt(ms, 10))
		}
	}
	var raw []ytRawActivity
	var err error
	if issue != "" {
		err = y.callJSON(ctx, "issue.activities", map[string]string{"id": issue}, q, nil, &raw)
	} else {
		q.Set("issueQuery", query)
		if author := req.GetString("author", ""); author != "" {
			q.Set("author", author)
		}
		err = y.callJSON(ctx, "activities", nil, q, nil, &raw)
	}
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	items := make([]ytActivityOut, 0, len(raw))
	for _, a := range raw {
		items = append(items, renderActivity(a, loc))
	}
	out := map[string]any{"count": len(items), "activities": items}
	if issue != "" {
		out["issue"] = issue
	} else {
		out["query"] = query
	}
	return ytResultJSON(out)
}

// ytSafeFileName keeps an attachment name usable as part of a local file
// name: only letters, digits, dot, dash and underscore survive.
var ytUnsafeFileRune = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func ytSafeFileName(name string) string {
	s := ytUnsafeFileRune.ReplaceAllString(name, "_")
	s = strings.Trim(s, "._")
	if s == "" {
		return "attachment"
	}
	return s
}

func (y *YouTrackREST) handleGetAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	want := strings.TrimSpace(req.GetString("attachment", ""))
	if issue == "" || want == "" {
		return mcp.NewToolResultError("issue and attachment (id or file name, as listed by get_issue) are required"), nil
	}
	outputFile := req.GetString("output_file", "")
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	var list []ytRawAttachment
	if err := y.callJSON(ctx, "issue.attachments", map[string]string{"id": issue}, nil, nil, &list); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var found *ytRawAttachment
	var names []string
	for i := range list {
		a := &list[i]
		names = append(names, a.Name)
		if a.ID == want || strings.EqualFold(a.Name, want) {
			if found != nil {
				return mcp.NewToolResultError(fmt.Sprintf("attachment name %q is ambiguous on %s — use the id (%s or %s)", want, issue, found.ID, a.ID)), nil
			}
			found = a
		}
	}
	if found == nil {
		return mcp.NewToolResultError(fmt.Sprintf("attachment %q not found on %s; available: %s", want, issue, strings.Join(names, ", "))), nil
	}
	if found.URL == "" {
		return mcp.NewToolResultError(fmt.Sprintf("attachment %s (%s) carries no download URL", found.ID, found.Name)), nil
	}

	resp, err := y.download(ctx, found.URL)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	defer resp.Body.Close()
	contentType := found.MimeType
	if contentType == "" {
		contentType = sniffContentType(resp)
	}
	meta := fmt.Sprintf("%s — %s (%s, %d bytes, attachment %s)", issue, found.Name, contentType, found.Size, found.ID)

	if outputFile != "" {
		return saveResponseToFile(resp, contentType, outputFile)
	}
	isImage := strings.HasPrefix(contentType, "image/") && !strings.Contains(contentType, "svg")
	if isImage && found.Size <= youtrackInlineImageMax {
		data, err := io.ReadAll(io.LimitReader(resp.Body, youtrackInlineImageMax+1))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read attachment: %v", err)), nil
		}
		if int64(len(data)) > youtrackInlineImageMax {
			return mcp.NewToolResultError(fmt.Sprintf("attachment %s is larger than reported — pass output_file to save it", found.Name)), nil
		}
		return mcp.NewToolResultImage(meta, base64.StdEncoding.EncodeToString(data), contentType), nil
	}
	if _, binary := binaryResponseExt(contentType); !binary && found.Size <= youtrackInlineTextMax {
		data, err := io.ReadAll(io.LimitReader(resp.Body, youtrackInlineTextMax+1))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read attachment: %v", err)), nil
		}
		return mcp.NewToolResultText(meta + "\n\n" + string(data)), nil
	}
	pattern := "youtrack-*-" + ytSafeFileName(found.Name)
	return saveResponseToGeneratedFile(resp, contentType, y.outputDir, pattern)
}

func (y *YouTrackREST) handleListProjects(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type rawProject struct {
		ID        string    `json:"id"`
		ShortName string    `json:"shortName"`
		Name      string    `json:"name"`
		Archived  bool      `json:"archived"`
		Leader    *ytRawRef `json:"leader"`
	}
	q := url.Values{}
	q.Set("$top", strconv.Itoa(youtrackListTop))
	var raw []rawProject
	if err := y.callJSON(ctx, "projects", nil, q, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	includeArchived := req.GetBool("include_archived", false)
	type projectOut struct {
		ID        string `json:"id"`
		ShortName string `json:"short_name"`
		Name      string `json:"name"`
		Archived  bool   `json:"archived,omitempty"`
		Leader    string `json:"leader,omitempty"`
	}
	var out []projectOut
	for _, p := range raw {
		if p.Archived && !includeArchived {
			continue
		}
		out = append(out, projectOut{ID: p.ID, ShortName: p.ShortName, Name: p.Name, Archived: p.Archived, Leader: ytRefName(p.Leader)})
	}
	if out == nil {
		out = []projectOut{}
	}
	return ytResultJSON(out)
}

// resolveProject turns a short name, id or name into the project id and
// short name.
func (y *YouTrackREST) resolveProject(ctx context.Context, ref string) (id, shortName string, err error) {
	type rawProject struct {
		ID        string `json:"id"`
		ShortName string `json:"shortName"`
		Name      string `json:"name"`
	}
	q := url.Values{}
	q.Set("$top", strconv.Itoa(youtrackListTop))
	var raw []rawProject
	if err := y.callJSON(ctx, "projects", nil, q, nil, &raw); err != nil {
		return "", "", err
	}
	ref = strings.TrimSpace(ref)
	for _, p := range raw {
		if p.ID == ref || strings.EqualFold(p.ShortName, ref) {
			return p.ID, p.ShortName, nil
		}
	}
	for _, p := range raw {
		if strings.EqualFold(p.Name, ref) {
			return p.ID, p.ShortName, nil
		}
	}
	names := make([]string, 0, len(raw))
	for _, p := range raw {
		names = append(names, p.ShortName)
	}
	return "", "", fmt.Errorf("project %q not found; known: %s", ref, strings.Join(names, ", "))
}

// projectFields lists a project's custom fields with their types and bundle
// values.
func (y *YouTrackREST) projectFields(ctx context.Context, projectID string) ([]ytRawProjectField, error) {
	q := url.Values{}
	q.Set("$top", strconv.Itoa(youtrackListTop))
	var raw []ytRawProjectField
	if err := y.callJSON(ctx, "project.fields", map[string]string{"id": projectID}, q, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (y *YouTrackREST) handleGetProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ref, _ := req.RequireString("project")
	if ref == "" {
		return mcp.NewToolResultError("project is required (short name or id)"), nil
	}
	id, _, err := y.resolveProject(ctx, ref)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var raw struct {
		ID          string    `json:"id"`
		ShortName   string    `json:"shortName"`
		Name        string    `json:"name"`
		Description string    `json:"description"`
		Archived    bool      `json:"archived"`
		Leader      *ytRawRef `json:"leader"`
		CreatedBy   *ytRawRef `json:"createdBy"`
	}
	if err := y.callJSON(ctx, "project.get", map[string]string{"id": id}, nil, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	fields, err := y.projectFields(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	fieldOut := make([]ytProjectFieldOut, 0, len(fields))
	for _, f := range fields {
		fieldOut = append(fieldOut, renderProjectField(f))
	}
	return ytResultJSON(map[string]any{
		"id":          raw.ID,
		"short_name":  raw.ShortName,
		"name":        raw.Name,
		"description": raw.Description,
		"archived":    raw.Archived,
		"leader":      ytRefName(raw.Leader),
		"created_by":  ytRefName(raw.CreatedBy),
		"fields":      fieldOut,
	})
}

func (y *YouTrackREST) handleFindUsers(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if strings.TrimSpace(query) == "" {
		return mcp.NewToolResultError("query is required (login, name or email fragment)"), nil
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("$top", strconv.Itoa(ytClamp(req.GetInt("top", 20), 20, 200)))
	var raw []struct {
		ID     string `json:"id"`
		Login  string `json:"login"`
		Name   string `json:"fullName"`
		Email  string `json:"email"`
		Banned bool   `json:"banned"`
	}
	if err := y.callJSON(ctx, "users", nil, q, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	type userOut struct {
		ID     string `json:"id"`
		Login  string `json:"login"`
		Name   string `json:"name"`
		Email  string `json:"email,omitempty"`
		Banned bool   `json:"banned,omitempty"`
	}
	out := make([]userOut, 0, len(raw))
	for _, u := range raw {
		out = append(out, userOut{ID: u.ID, Login: u.Login, Name: u.Name, Email: u.Email, Banned: u.Banned})
	}
	return ytResultJSON(out)
}

// resolveUser finds a user by login (exact, case-insensitive). The search
// endpoint matches loosely, so the result is filtered.
func (y *YouTrackREST) resolveUser(ctx context.Context, login string) (id string, err error) {
	q := url.Values{}
	q.Set("query", login)
	q.Set("$top", "50")
	var raw []struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	}
	if err := y.callJSON(ctx, "users", nil, q, nil, &raw); err != nil {
		return "", err
	}
	for _, u := range raw {
		if strings.EqualFold(u.Login, login) {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("user %q not found (login must match exactly; find_users searches loosely)", login)
}

// resolveGroup finds a user group by name (exact, case-insensitive).
func (y *YouTrackREST) resolveGroup(ctx context.Context, name string) (id string, err error) {
	pages, _, err := y.callPages(ctx, "groups", nil, nil)
	if err != nil {
		return "", err
	}
	var names []string
	for _, p := range pages {
		var g struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(p, &g) != nil {
			continue
		}
		if strings.EqualFold(g.Name, name) {
			return g.ID, nil
		}
		names = append(names, g.Name)
	}
	return "", fmt.Errorf("group %q not found; known: %s", name, strings.Join(names, ", "))
}

func (y *YouTrackREST) handleListLinkTypes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var raw []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		SourceToTarget string `json:"sourceToTarget"`
		TargetToSource string `json:"targetToSource"`
		Directed       bool   `json:"directed"`
		Aggregation    bool   `json:"aggregation"`
	}
	if err := y.callJSON(ctx, "linktypes", nil, nil, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	type out struct {
		Name      string   `json:"name"`
		Relations []string `json:"relations"`
		Directed  bool     `json:"directed"`
	}
	list := make([]out, 0, len(raw))
	for _, t := range raw {
		o := out{Name: t.Name, Directed: t.Directed}
		if t.SourceToTarget != "" {
			o.Relations = append(o.Relations, t.SourceToTarget)
		}
		if t.Directed && t.TargetToSource != "" {
			o.Relations = append(o.Relations, t.TargetToSource)
		}
		list = append(list, o)
	}
	return ytResultJSON(map[string]any{
		"link_types": list,
		"usage":      "link_issues takes one of the relation phrases as seen from the first issue, e.g. relation=\"parent for\" makes the target a subtask",
	})
}

type ytRawAgile struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Projects []ytRawRef `json:"projects"`
	Current  *ytRawRef  `json:"currentSprint"`
	Settings *struct {
		DisableSprints bool `json:"disableSprints"`
	} `json:"sprintsSettings"`
}

func (y *YouTrackREST) listBoards(ctx context.Context) ([]ytRawAgile, error) {
	q := url.Values{}
	q.Set("$top", strconv.Itoa(youtrackListTop))
	var raw []ytRawAgile
	if err := y.callJSON(ctx, "agiles", nil, q, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

var ytEntityID = regexp.MustCompile(`^\d+-\d+$`)

// resolveBoard accepts a board id (97-63) or a board name.
func (y *YouTrackREST) resolveBoard(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ytEntityID.MatchString(ref) {
		return ref, nil
	}
	boards, err := y.listBoards(ctx)
	if err != nil {
		return "", err
	}
	var names []string
	for _, b := range boards {
		if strings.EqualFold(b.Name, ref) {
			return b.ID, nil
		}
		names = append(names, b.Name)
	}
	return "", fmt.Errorf("board %q not found; known: %s", ref, strings.Join(names, ", "))
}

func (y *YouTrackREST) handleListBoards(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	boards, err := y.listBoards(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	type out struct {
		ID             string   `json:"id"`
		Name           string   `json:"name"`
		Projects       []string `json:"projects"`
		CurrentSprint  string   `json:"current_sprint,omitempty"`
		SprintsEnabled bool     `json:"sprints_enabled"`
	}
	list := make([]out, 0, len(boards))
	for _, b := range boards {
		o := out{ID: b.ID, Name: b.Name, SprintsEnabled: b.Settings == nil || !b.Settings.DisableSprints}
		for _, p := range b.Projects {
			o.Projects = append(o.Projects, p.ShortName)
		}
		if b.Current != nil {
			o.CurrentSprint = b.Current.Name
		}
		list = append(list, o)
	}
	return ytResultJSON(list)
}

type ytRawSprint struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Start      *int64  `json:"start"`
	Finish     *int64  `json:"finish"`
	Goal       *string `json:"goal"`
	Archived   bool    `json:"archived"`
	IsDefault  bool    `json:"isDefault"`
	Unresolved *int    `json:"unresolvedIssuesCount"`
}

type ytSprintOut struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Start      *string `json:"start"`
	Finish     *string `json:"finish"`
	Goal       *string `json:"goal,omitempty"`
	Archived   bool    `json:"archived,omitempty"`
	Default    bool    `json:"default,omitempty"`
	Unresolved *int    `json:"unresolved_issues,omitempty"`
}

// renderSprint renders sprint bounds in UTC: YouTrack stores them as
// 00:00:00 and 23:59:59.999 UTC of the first and last day (observed on a
// production board), so UTC names the right days for every reader while a
// zone west of UTC would shift the start to the previous evening.
func renderSprint(s ytRawSprint) ytSprintOut {
	return ytSprintOut{
		ID:         s.ID,
		Name:       s.Name,
		Start:      ytTimePtr(s.Start, time.UTC),
		Finish:     ytTimePtr(s.Finish, time.UTC),
		Goal:       s.Goal,
		Archived:   s.Archived,
		Default:    s.IsDefault,
		Unresolved: s.Unresolved,
	}
}

func (y *YouTrackREST) handleGetCurrentSprint(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	board, _ := req.RequireString("board")
	if board == "" {
		return mcp.NewToolResultError("board is required (id or name, see list_boards)"), nil
	}
	id, err := y.resolveBoard(ctx, board)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var raw ytRawSprint
	if err := y.callJSON(ctx, "agile.sprint", map[string]string{"id": id, "sprintId": "current"}, nil, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return ytResultJSON(renderSprint(raw))
}

func (y *YouTrackREST) handleListSprints(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	board, _ := req.RequireString("board")
	if board == "" {
		return mcp.NewToolResultError("board is required (id or name, see list_boards)"), nil
	}
	id, err := y.resolveBoard(ctx, board)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	top := ytClamp(req.GetInt("top", 50), 50, 1000)
	// YouTrack lists sprints oldest first and offers no ordering parameter;
	// the whole list is fetched and reversed so the newest come first.
	q := url.Values{}
	q.Set("$top", "10000")
	var raw []ytRawSprint
	if err := y.callJSON(ctx, "agile.sprints", map[string]string{"id": id}, q, nil, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}
	if len(raw) > top {
		raw = raw[:top]
	}
	out := make([]ytSprintOut, 0, len(raw))
	for _, s := range raw {
		out = append(out, renderSprint(s))
	}
	return ytResultJSON(out)
}
