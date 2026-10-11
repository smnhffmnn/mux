package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Write handlers. All of them are absent on read-only connections (see
// Tools) and all of them run the field guards before anything is sent.

// ytFieldWrite is one custom field entry of a create/update body.
type ytFieldWrite struct {
	Name  string `json:"name"`
	Type  string `json:"$type"`
	Value any    `json:"value"`
}

// sortedKeys gives deterministic order to field processing (error messages,
// request bodies, tests).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (y *YouTrackREST) handleCreateIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	project, _ := req.RequireString("project")
	summary, _ := req.RequireString("summary")
	if strings.TrimSpace(project) == "" || strings.TrimSpace(summary) == "" {
		return mcp.NewToolResultError("project and summary are required"), nil
	}
	fields, err := objectArg(req, "fields")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	projectID, shortName, err := y.resolveProject(ctx, project)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	for _, name := range sortedKeys(fields) {
		if y.isProtected(shortName, name) {
			return mcp.NewToolResultError(ytProtectedError(shortName, name).Error()), nil
		}
	}
	body := map[string]any{
		"project": map[string]any{"id": projectID},
		"summary": summary,
	}
	if d := req.GetString("description", ""); d != "" {
		body["description"] = d
	}
	if len(fields) > 0 {
		schema, err := y.projectFields(ctx, projectID)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		loc := y.location(ctx)
		var writes []ytFieldWrite
		for _, name := range sortedKeys(fields) {
			pf, ok := ytFindProjectField(schema, name)
			if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("project %s has no field %q; fields: %s", shortName, name, strings.Join(ytFieldNames(schema), ", "))), nil
			}
			typ, err := ytWriteType(pf.Field.FieldType.ID, "")
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("field %q: %v", name, err)), nil
			}
			value, err := ytEncodeValue(pf.Field.Name, pf.Field.FieldType.ID, fields[name], loc)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			writes = append(writes, ytFieldWrite{Name: pf.Field.Name, Type: typ, Value: value})
		}
		body["customFields"] = writes
	}
	var raw ytRawIssue
	if err := y.callJSON(ctx, "issue.create", nil, nil, body, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return ytResultJSON(y.renderIssue(raw, y.location(ctx), true))
}

func (y *YouTrackREST) handleUpdateIssue(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	if issue == "" {
		return mcp.NewToolResultError("issue is required"), nil
	}
	fields, err := objectArg(req, "fields")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	summary := req.GetString("summary", "")
	description, hasDescription := req.GetArguments()["description"].(string)
	if summary == "" && !hasDescription && len(fields) == 0 {
		return mcp.NewToolResultError("nothing to update: give summary, description or fields"), nil
	}

	// Current state decides the guards (set-once) and the $type of each field.
	var current ytRawIssue
	if err := y.callJSON(ctx, "issue.guard", map[string]string{"id": issue}, nil, nil, &current); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	shortName := current.Project.ShortName
	currentByName := make(map[string]ytRawCustomField, len(current.CustomFields))
	for _, f := range current.CustomFields {
		currentByName[strings.ToLower(f.Name)] = f
	}
	for _, name := range sortedKeys(fields) {
		if y.isProtected(shortName, name) {
			return mcp.NewToolResultError(ytProtectedError(shortName, name).Error()), nil
		}
		if y.isSetOnce(shortName, name) {
			if cur, ok := currentByName[strings.ToLower(name)]; ok && !cur.empty() {
				return mcp.NewToolResultError(ytSetOnceError(shortName, name, current.IDReadable).Error()), nil
			}
		}
	}

	body := map[string]any{}
	if summary != "" {
		body["summary"] = summary
	}
	if hasDescription {
		body["description"] = description
	}
	if len(fields) > 0 {
		var schema []ytRawProjectField
		loc := y.location(ctx)
		var writes []ytFieldWrite
		for _, name := range sortedKeys(fields) {
			if schema == nil {
				schema, err = y.projectFields(ctx, current.Project.ID)
				if err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
			}
			pf, ok := ytFindProjectField(schema, name)
			if !ok {
				return mcp.NewToolResultError(fmt.Sprintf("project %s has no field %q; fields: %s", shortName, name, strings.Join(ytFieldNames(schema), ", "))), nil
			}
			currentType := ""
			if cur, ok := currentByName[strings.ToLower(name)]; ok {
				currentType = cur.Type
			}
			typ, err := ytWriteType(pf.Field.FieldType.ID, currentType)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("field %q: %v", name, err)), nil
			}
			value, err := ytEncodeValue(pf.Field.Name, pf.Field.FieldType.ID, fields[name], loc)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			writes = append(writes, ytFieldWrite{Name: pf.Field.Name, Type: typ, Value: value})
		}
		body["customFields"] = writes
	}
	var raw ytRawIssue
	if err := y.callJSON(ctx, "issue.update", map[string]string{"id": issue}, nil, body, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return ytResultJSON(y.renderIssue(raw, y.location(ctx), true))
}

func (y *YouTrackREST) handleAddComment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	text, _ := req.RequireString("text")
	if issue == "" || strings.TrimSpace(text) == "" {
		return mcp.NewToolResultError("issue and text are required"), nil
	}
	groups, err := stringListArg(req, "visibility_groups")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	users, err := stringListArg(req, "visibility_users")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body := map[string]any{"text": text}
	if len(groups)+len(users) > 0 {
		vis := map[string]any{"$type": "LimitedVisibility"}
		var gids, uids []map[string]string
		for _, g := range groups {
			id, err := y.resolveGroup(ctx, g)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			gids = append(gids, map[string]string{"id": id})
		}
		for _, u := range users {
			id, err := y.resolveUser(ctx, u)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			uids = append(uids, map[string]string{"id": id})
		}
		if gids != nil {
			vis["permittedGroups"] = gids
		}
		if uids != nil {
			vis["permittedUsers"] = uids
		}
		body["visibility"] = vis
	}
	var raw ytRawComment
	if err := y.callJSON(ctx, "issue.comment.add", map[string]string{"id": issue}, nil, body, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return ytResultJSON(map[string]any{"issue": issue, "comment": renderComment(raw, y.location(ctx))})
}

// ytUploadFile is one validated file of an attach call.
type ytUploadFile struct {
	path        string
	name        string
	contentType string
	size        int64
}

// ytOpenUploadFiles validates the paths of an attach call the way parseUpload
// does for single uploads: absolute, existing, regular, readable — all before
// the first byte goes out.
func ytOpenUploadFiles(paths []string) ([]ytUploadFile, error) {
	if len(paths) == 0 {
		return nil, errors.New("file_paths is required (one or more absolute paths)")
	}
	files := make([]ytUploadFile, 0, len(paths))
	seen := map[string]bool{}
	for _, p := range paths {
		clean := filepath.Clean(config.ExpandHome(p))
		if !filepath.IsAbs(clean) {
			return nil, fmt.Errorf("file_paths: %q must be an absolute path", p)
		}
		fi, err := os.Stat(clean)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("file_paths: %s not found (resolved on the machine running mux)", clean)
			}
			return nil, fmt.Errorf("file_paths: %s is not usable: %w", clean, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("file_paths: %s is not a regular file", clean)
		}
		f, err := os.Open(clean)
		if err != nil {
			return nil, fmt.Errorf("file_paths: %s is not readable: %w", clean, err)
		}
		ct := detectFileContentType(clean, f)
		f.Close()
		name := filepath.Base(clean)
		if seen[strings.ToLower(name)] {
			// YouTrack references attachments by name in Markdown (![](name));
			// two equal names in one upload would be indistinguishable.
			return nil, fmt.Errorf("file_paths: two files are named %q — attachment names must be unique within an issue", name)
		}
		seen[strings.ToLower(name)] = true
		files = append(files, ytUploadFile{path: clean, name: name, contentType: ct, size: fi.Size()})
	}
	return files, nil
}

// ytMultipart builds a multipart/form-data body over several files without
// holding them in memory. Each file is one part named files[i] (the form the
// OpenAPI document shows). The returned open function yields the full body as
// one stream and is also used as GetBody for redirects.
func ytMultipart(files []ytUploadFile) (contentType string, length int64, open func() (io.ReadCloser, error)) {
	boundary := multipart.NewWriter(io.Discard).Boundary()
	heads := make([][]byte, len(files))
	for i, f := range files {
		heads[i] = []byte(fmt.Sprintf("--%s\r\nContent-Disposition: %s\r\nContent-Type: %s\r\n\r\n",
			boundary, partContentDisposition(fmt.Sprintf("files[%d]", i), f.name), f.contentType))
		length += int64(len(heads[i])) + f.size + 2 // trailing CRLF after the file bytes
	}
	tail := []byte("--" + boundary + "--\r\n")
	length += int64(len(tail))

	open = func() (io.ReadCloser, error) {
		readers := make([]io.Reader, 0, len(files)*3+1)
		var closers []io.Closer
		for i, f := range files {
			fh, err := os.Open(f.path)
			if err != nil {
				for _, c := range closers {
					c.Close()
				}
				return nil, fmt.Errorf("open %s: %w", f.path, err)
			}
			closers = append(closers, fh)
			readers = append(readers, bytes.NewReader(heads[i]), fh, strings.NewReader("\r\n"))
		}
		readers = append(readers, bytes.NewReader(tail))
		return &ytMultiCloser{Reader: io.MultiReader(readers...), closers: closers}, nil
	}
	return "multipart/form-data; boundary=" + boundary, length, open
}

type ytMultiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *ytMultiCloser) Close() error {
	var first error
	for _, c := range m.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (y *YouTrackREST) handleAttach(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	if issue == "" {
		return mcp.NewToolResultError("issue is required"), nil
	}
	paths, err := stringListArg(req, "file_paths")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	files, err := ytOpenUploadFiles(paths)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	contentType, length, open := ytMultipart(files)
	body, err := open()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	ep := ytEndpoints["issue.attachment.add"]
	u := y.apiURL + strings.ReplaceAll(ep.Path, "{id}", url.PathEscape(issue)) + "?fields=" + url.QueryEscape(ep.Fields)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		body.Close()
		return mcp.NewToolResultError(fmt.Sprintf("create request: %v", err)), nil
	}
	httpReq.ContentLength = length
	httpReq.GetBody = open
	httpReq.Header.Set("Content-Type", contentType)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+y.token)

	client := *y.client
	client.Timeout = uploadTimeout
	resp, err := client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("upload failed: %v", err)), nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, youtrackMaxBody))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcp.NewToolResultError(ytAPIError(resp.StatusCode, data).Error()), nil
	}

	// YouTrack answers a multi-file upload with a single attachment object;
	// the full list is what tells the caller everything arrived.
	var list []ytRawAttachment
	if err := y.callJSON(ctx, "issue.attachments", map[string]string{"id": issue}, nil, nil, &list); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("uploaded, but listing attachments failed: %v", err)), nil
	}
	loc := y.location(ctx)
	uploaded := map[string]bool{}
	for _, f := range files {
		uploaded[strings.ToLower(f.name)] = true
	}
	out := renderAttachments(list, loc)
	var missing []string
	found := map[string]bool{}
	for i := range out {
		if uploaded[strings.ToLower(out[i].Name)] {
			out[i].Uploaded = true
			found[strings.ToLower(out[i].Name)] = true
		}
	}
	for _, f := range files {
		if !found[strings.ToLower(f.name)] {
			missing = append(missing, f.name)
		}
	}
	result := map[string]any{
		"issue":       issue,
		"uploaded":    len(files),
		"attachments": out,
		"markdown":    "reference an attachment in a description or comment as ![](name) — the file name is the reference",
	}
	if len(missing) > 0 {
		result["warning"] = fmt.Sprintf("not found in the attachment list after upload: %s", strings.Join(missing, ", "))
	}
	return ytResultJSON(result)
}

func (y *YouTrackREST) handleWatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	if issue == "" {
		return mcp.NewToolResultError("issue is required"), nil
	}
	logins, err := stringListArg(req, "users")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if len(logins) == 0 {
		return mcp.NewToolResultError("users is required (one or more logins)"), nil
	}
	for _, login := range logins {
		id, err := y.resolveUser(ctx, login)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// isStarred must be sent explicitly: without it YouTrack answers 200
		// with an echo and stores nothing.
		body := map[string]any{
			"$type":     "IssueWatcher",
			"user":      map[string]any{"id": id, "$type": "User"},
			"isStarred": true,
		}
		if err := y.callJSON(ctx, "issue.watcher.add", map[string]string{"id": issue}, nil, body, nil); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("watch %s: %v", login, err)), nil
		}
	}
	var raw ytRawIssue
	if err := y.callJSON(ctx, "issue.watchers", map[string]string{"id": issue}, nil, nil, &raw); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("watchers set, but reading them back failed: %v", err)), nil
	}
	var watchers []string
	if raw.Watchers != nil {
		for _, w := range raw.Watchers.IssueWatchers {
			watchers = append(watchers, w.User.Login)
		}
	}
	var notListed []string
	for _, login := range logins {
		found := false
		for _, w := range watchers {
			if strings.EqualFold(w, login) {
				found = true
			}
		}
		if !found {
			notListed = append(notListed, login)
		}
	}
	out := map[string]any{"issue": raw.IDReadable, "watchers": watchers}
	if len(notListed) > 0 {
		out["warning"] = fmt.Sprintf("accepted but not listed as watchers afterwards: %s", strings.Join(notListed, ", "))
	}
	return ytResultJSON(out)
}

func (y *YouTrackREST) handleLinkIssues(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	relation, _ := req.RequireString("relation")
	target, _ := req.RequireString("target")
	if issue == "" || strings.TrimSpace(relation) == "" || target == "" {
		return mcp.NewToolResultError("issue, relation and target are required (relation as seen from issue, e.g. \"parent for\", \"depends on\", \"relates to\" — see list_link_types)"), nil
	}
	var links []ytRawLink
	if err := y.callJSON(ctx, "issue.links", map[string]string{"id": issue}, nil, nil, &links); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	relation = strings.TrimSpace(relation)
	var linkID string
	var roles []string
	for _, l := range links {
		role := l.role()
		if role != "" {
			roles = append(roles, role)
		}
		if strings.EqualFold(role, relation) || (l.Direction == "BOTH" && strings.EqualFold(l.LinkType.Name, relation)) {
			linkID = l.ID
			break
		}
	}
	if linkID == "" {
		sort.Strings(roles)
		return mcp.NewToolResultError(fmt.Sprintf("relation %q is not available on %s; use one of: %s", relation, issue, strings.Join(roles, ", "))), nil
	}
	var tgt struct {
		ID         string `json:"id"`
		IDReadable string `json:"idReadable"`
	}
	if err := y.callJSON(ctx, "issue.id", map[string]string{"id": target}, nil, nil, &tgt); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("target %s: %v", target, err)), nil
	}
	if err := y.callJSON(ctx, "issue.link.add", map[string]string{"id": issue, "issueLinkId": linkID}, nil, map[string]any{"id": tgt.ID}, nil); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := y.callJSON(ctx, "issue.links", map[string]string{"id": issue}, nil, nil, &links); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("linked, but reading links back failed: %v", err)), nil
	}
	return ytResultJSON(map[string]any{"issue": issue, "added": fmt.Sprintf("%s %s %s", issue, relation, tgt.IDReadable), "links": renderLinks(links)})
}

func (y *YouTrackREST) handleLogWork(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issue, _ := req.RequireString("issue")
	if issue == "" {
		return mcp.NewToolResultError("issue is required"), nil
	}
	durationArg, ok := req.GetArguments()["duration"]
	if !ok || durationArg == nil {
		return mcp.NewToolResultError("duration is required (minutes, or \"1h 30m\")"), nil
	}
	minutes, err := ytParsePeriod(durationArg)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	loc := y.location(ctx)
	date := req.GetString("date", "")
	if date == "" {
		date = time.Now().In(loc).Format("2006-01-02")
	}
	dateMs, err := ytParseWorkDate(date)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body := map[string]any{
		"duration": map[string]any{"minutes": minutes},
		"date":     dateMs,
	}
	if text := req.GetString("text", ""); text != "" {
		body["text"] = text
	}
	if typeName := req.GetString("type", ""); typeName != "" {
		var cur ytRawIssue
		if err := y.callJSON(ctx, "issue.guard", map[string]string{"id": issue}, nil, nil, &cur); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var types []ytRawRef
		if err := y.callJSON(ctx, "project.workitemtypes", map[string]string{"id": cur.Project.ID}, nil, nil, &types); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		var names []string
		typeID := ""
		for _, t := range types {
			names = append(names, t.Name)
			if strings.EqualFold(t.Name, typeName) {
				typeID = t.ID
			}
		}
		if typeID == "" {
			return mcp.NewToolResultError(fmt.Sprintf("work item type %q not found in project %s; known: %s", typeName, cur.Project.ShortName, strings.Join(names, ", "))), nil
		}
		body["type"] = map[string]any{"id": typeID}
	}
	var raw ytRawWorkItem
	if err := y.callJSON(ctx, "issue.workitem.add", map[string]string{"id": issue}, nil, body, &raw); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return ytResultJSON(map[string]any{"issue": issue, "work_item": renderWorkItem(raw, loc)})
}

// commandTargets loads the issues a command addresses — project and the
// current values of their custom fields, the input of the guards.
func (y *YouTrackREST) commandTargets(ctx context.Context, issues []string) ([]ytRawIssue, error) {
	targets := make([]ytRawIssue, 0, len(issues))
	for _, id := range issues {
		var cur ytRawIssue
		if err := y.callJSON(ctx, "issue.guard", map[string]string{"id": id}, nil, nil, &cur); err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
		targets = append(targets, cur)
	}
	return targets, nil
}

// guardCommandText runs the field guards over one piece of command text: the
// command as typed, and each command YouTrack parsed from it. A protected
// field the text names is refused outright; a set-once field is refused for
// every target that already carries a value. Only guards whose project
// matches a target's project are considered.
func (y *YouTrackREST) guardCommandText(targets []ytRawIssue, text string) error {
	for _, t := range targets {
		project := t.Project.ShortName
		for _, g := range y.protected {
			if g.MatchesProject(project) && ytCommandMentions(text, g.Field) {
				return ytProtectedError(project, g.Field)
			}
		}
		for _, g := range y.setOnce {
			if !g.MatchesProject(project) || !ytCommandMentions(text, g.Field) {
				continue
			}
			for _, f := range t.CustomFields {
				if strings.EqualFold(f.Name, g.Field) && !f.empty() {
					return ytSetOnceError(project, g.Field, t.IDReadable)
				}
			}
		}
	}
	return nil
}

type ytParsedCommand struct {
	Description string `json:"description"`
	Error       bool   `json:"error"`
}

type ytAssist struct {
	Commands    []ytParsedCommand `json:"commands"`
	Suggestions []struct {
		Option      string `json:"option"`
		Description string `json:"description"`
	} `json:"suggestions"`
}

// assistCommand asks YouTrack to parse a command without applying it. The
// descriptions come back as HTML fragments naming the canonical field ("Set
// Priority <span class=\"bold\">High</span>" for the bare command "High");
// tags are stripped so the guards and the caller see plain text.
func (y *YouTrackREST) assistCommand(ctx context.Context, command string, issueRefs []map[string]string) (*ytAssist, error) {
	body := map[string]any{"query": command, "caret": utf8.RuneCountInString(command), "issues": issueRefs}
	var assist ytAssist
	if err := y.callJSON(ctx, "commands.assist", nil, nil, body, &assist); err != nil {
		return nil, err
	}
	for i := range assist.Commands {
		assist.Commands[i].Description = ytStripTags(assist.Commands[i].Description)
	}
	for i := range assist.Suggestions {
		assist.Suggestions[i].Description = ytStripTags(assist.Suggestions[i].Description)
	}
	return &assist, nil
}

func (y *YouTrackREST) handleRunCommand(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	command, _ := req.RequireString("command")
	issues, err := stringListArg(req, "issues")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(command) == "" || len(issues) == 0 {
		return mcp.NewToolResultError("issues (one or more readable IDs) and command are required"), nil
	}
	targets, err := y.commandTargets(ctx, issues)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Cheap pre-check on the text as typed.
	if err := y.guardCommandText(targets, command); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	issueRefs := make([]map[string]string, 0, len(issues))
	for _, id := range issues {
		issueRefs = append(issueRefs, map[string]string{"idReadable": id})
	}
	// YouTrack's own parse is the authority on which fields a command touches:
	// a bare value ("High") or an alias never contains the field name, its
	// parsed description ("Set Priority High") does.
	assist, err := y.assistCommand(ctx, command, issueRefs)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var parseErrors []string
	var guardErr error
	for _, c := range assist.Commands {
		if c.Error {
			parseErrors = append(parseErrors, c.Description)
			continue
		}
		if guardErr == nil {
			guardErr = y.guardCommandText(targets, c.Description)
		}
	}
	if req.GetBool("dry_run", false) {
		sugg := assist.Suggestions
		if len(sugg) > 20 {
			sugg = sugg[:20]
		}
		out := map[string]any{
			"dry_run":     true,
			"command":     command,
			"issues":      issues,
			"valid":       len(assist.Commands) > 0 && len(parseErrors) == 0,
			"parsed":      assist.Commands,
			"suggestions": sugg,
		}
		if guardErr != nil {
			out["would_be_refused"] = guardErr.Error()
		}
		return ytResultJSON(out)
	}
	if len(parseErrors) > 0 {
		return mcp.NewToolResultError(fmt.Sprintf("YouTrack does not understand the command: %s — nothing applied (dry_run shows suggestions)", strings.Join(parseErrors, "; "))), nil
	}
	if len(assist.Commands) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("YouTrack parsed no command from %q — nothing applied", command)), nil
	}
	if guardErr != nil {
		return mcp.NewToolResultError(guardErr.Error()), nil
	}
	body := map[string]any{"query": command, "issues": issueRefs}
	if comment := req.GetString("comment", ""); comment != "" {
		body["comment"] = comment
	}
	if req.GetBool("silent", false) {
		body["silent"] = true
	}
	if err := y.callJSON(ctx, "commands", nil, nil, body, nil); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	loc := y.location(ctx)
	var after []ytIssueOut
	for i, id := range issues {
		if i >= youtrackCommandRereads {
			break
		}
		var raw ytRawIssue
		if err := y.callJSON(ctx, "issue.compact", map[string]string{"id": id}, nil, nil, &raw); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("command applied, but re-reading %s failed: %v", id, err)), nil
		}
		after = append(after, y.renderIssue(raw, loc, false))
	}
	out := map[string]any{"command": command, "applied": assist.Commands, "applied_to": issues, "issues": after}
	if len(issues) > youtrackCommandRereads {
		out["note"] = fmt.Sprintf("showing the first %d of %d issues", youtrackCommandRereads, len(issues))
	}
	return ytResultJSON(out)
}

// ytAdminIssuesPath matches the issue subtree of a project in the admin API
// (/admin/projects/{id}/issues…). It creates and writes issues past the field
// guards, so the pass-through refuses it: issues have their own tools.
var ytAdminIssuesPath = regexp.MustCompile(`^/projects/[^/]+/issues(?:/|$)`)

// adminURL resolves an admin pass-through path. Two roots are allowed:
// /api/admin/… on the YouTrack base URL and /hub/api/rest/… on the host (Hub
// lives beside YouTrack, not under its context path). The /admin/… shorthand
// is accepted. The path is parsed and cleaned before the prefix check, so dot
// segments or encoded separators cannot walk out of the two subtrees.
func (y *YouTrackREST) adminURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		return "", fmt.Errorf("path %q must be an absolute path on the instance (/api/admin/… or /hub/api/rest/…), not a URL", raw)
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	lower := strings.ToLower(raw)
	if strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("path %q: encoded dots, slashes or backslashes are not accepted", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.Path == "" {
		return "", fmt.Errorf("path %q must be an absolute path on the instance (/api/admin/… or /hub/api/rest/…), not a URL", raw)
	}
	cleaned := path.Clean(u.Path)
	if cleaned != u.Path && cleaned+"/" != u.Path {
		return "", fmt.Errorf("path %q is not normalized (no . or .. segments, no double slashes)", raw)
	}
	var root, rel string
	hub := false
	switch {
	case cleaned == "/api/admin" || strings.HasPrefix(cleaned, "/api/admin/"):
		root, rel = y.baseURL+"/api/admin", strings.TrimPrefix(cleaned, "/api/admin")
	case cleaned == "/admin" || strings.HasPrefix(cleaned, "/admin/"):
		root, rel = y.apiURL+"/admin", strings.TrimPrefix(cleaned, "/admin")
	case cleaned == "/hub/api/rest" || strings.HasPrefix(cleaned, "/hub/api/rest/"):
		root, rel, hub = y.rootURL+"/hub/api/rest", strings.TrimPrefix(cleaned, "/hub/api/rest"), true
	default:
		return "", fmt.Errorf("path must start with /api/admin/ (YouTrack administration) or /hub/api/rest/ (Hub: users, groups, roles), got %q — issue, comment and project reads have their own tools", raw)
	}
	if !hub && ytAdminIssuesPath.MatchString(rel) {
		return "", fmt.Errorf("path %q addresses issues through the administration API — use the issue tools (create_issue, update_issue, run_command), where the field guards apply", raw)
	}
	full := root + rel
	if u.RawQuery != "" {
		full += "?" + u.RawQuery
	}
	return full, nil
}

func (y *YouTrackREST) handleAdminGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, _ := req.RequireString("path")
	u, err := y.adminURL(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	outputFile := req.GetString("output_file", "")
	if outputFile != "" {
		if _, err := validateOutputFile(outputFile); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create request: %v", err)), nil
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+y.token)
	resp, err := y.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()
	if outputFile != "" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return saveResponseToFile(resp, resp.Header.Get("Content-Type"), outputFile)
	}
	return ytRawResult(resp)
}

func (y *YouTrackREST) handleAdminRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	method := strings.ToUpper(req.GetString("method", ""))
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return mcp.NewToolResultError("method must be POST, PUT, PATCH or DELETE"), nil
	}
	path, _ := req.RequireString("path")
	u, err := y.adminURL(path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	body, err := stringArg(req, "body")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var rdr io.Reader
	if body != "" {
		if !json.Valid([]byte(body)) {
			return mcp.NewToolResultError("body must be valid JSON"), nil
		}
		rdr = strings.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create request: %v", err)), nil
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+y.token)
	if rdr != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := y.client.Do(httpReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("request failed: %v", err)), nil
	}
	defer resp.Body.Close()
	return ytRawResult(resp)
}

// ytRawResult renders a pass-through response: status line plus the body,
// inlined up to youtrackAdminInlineMax.
func ytRawResult(resp *http.Response) (*mcp.CallToolResult, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, youtrackAdminInlineMax+1))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}
	truncated := false
	if len(data) > youtrackAdminInlineMax {
		data = data[:youtrackAdminInlineMax]
		truncated = true
	}
	text := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), string(data))
	if truncated {
		text += "\n\n[truncated at " + strconv.Itoa(youtrackAdminInlineMax) + " bytes — pass output_file to save the full response]"
	}
	if resp.StatusCode >= 400 {
		return mcp.NewToolResultError(text), nil
	}
	return mcp.NewToolResultText(text), nil
}
