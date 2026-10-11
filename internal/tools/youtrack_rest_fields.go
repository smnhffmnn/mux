package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Raw JSON shapes of the YouTrack REST API (only what the selectors request)
// and their rendering into the model-facing form.

type ytRawRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Login      string `json:"login"`
	FullName   string `json:"fullName"`
	ShortName  string `json:"shortName"`
	IDReadable string `json:"idReadable"`
	Summary    string `json:"summary"`
	Type       string `json:"$type"`
}

type ytRawCustomField struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	Type               string          `json:"$type"`
	Value              json.RawMessage `json:"value"`
	ProjectCustomField struct {
		Field struct {
			Name      string `json:"name"`
			FieldType struct {
				ID string `json:"id"`
			} `json:"fieldType"`
		} `json:"field"`
	} `json:"projectCustomField"`
}

// fieldType returns the project field type id (enum[1], date, period, …).
func (f ytRawCustomField) fieldType() string { return f.ProjectCustomField.Field.FieldType.ID }

// empty reports whether the field carries no value — the set-once guard's
// criterion. YouTrack renders an empty single value as null and an empty
// multi value as [].
func (f ytRawCustomField) empty() bool {
	v := strings.TrimSpace(string(f.Value))
	return v == "" || v == "null" || v == "[]"
}

type ytRawVisibility struct {
	Type            string     `json:"$type"`
	PermittedGroups []ytRawRef `json:"permittedGroups"`
	PermittedUsers  []ytRawRef `json:"permittedUsers"`
}

type ytRawAttachment struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	MimeType  string    `json:"mimeType"`
	Extension string    `json:"extension"`
	Created   int64     `json:"created"`
	Author    *ytRawRef `json:"author"`
	URL       string    `json:"url"`
	Comment   *ytRawRef `json:"comment"`
}

type ytRawLink struct {
	ID        string `json:"id"`
	Direction string `json:"direction"` // OUTWARD, INWARD, BOTH
	LinkType  struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		SourceToTarget string `json:"sourceToTarget"`
		TargetToSource string `json:"targetToSource"`
		Directed       bool   `json:"directed"`
	} `json:"linkType"`
	Issues []ytRawRef `json:"issues"`
}

// role is the phrase that describes the linked issues from this issue's
// point of view ("parent for", "subtask of", "relates to").
func (l ytRawLink) role() string {
	switch l.Direction {
	case "INWARD":
		return l.LinkType.TargetToSource
	case "OUTWARD":
		return l.LinkType.SourceToTarget
	default:
		if l.LinkType.SourceToTarget != "" {
			return l.LinkType.SourceToTarget
		}
		return l.LinkType.Name
	}
}

type ytRawComment struct {
	ID          string            `json:"id"`
	Text        string            `json:"text"`
	Created     int64             `json:"created"`
	Updated     *int64            `json:"updated"`
	Author      *ytRawRef         `json:"author"`
	Deleted     bool              `json:"deleted"`
	Visibility  *ytRawVisibility  `json:"visibility"`
	Attachments []ytRawAttachment `json:"attachments"`
}

type ytRawIssue struct {
	ID            string             `json:"id"`
	IDReadable    string             `json:"idReadable"`
	Summary       string             `json:"summary"`
	Description   *string            `json:"description"`
	Created       int64              `json:"created"`
	Updated       int64              `json:"updated"`
	Resolved      *int64             `json:"resolved"`
	Project       ytRawRef           `json:"project"`
	Reporter      *ytRawRef          `json:"reporter"`
	Updater       *ytRawRef          `json:"updater"`
	Tags          []ytRawRef         `json:"tags"`
	Visibility    *ytRawVisibility   `json:"visibility"`
	Watchers      *ytRawWatchers     `json:"watchers"`
	Links         []ytRawLink        `json:"links"`
	Attachments   []ytRawAttachment  `json:"attachments"`
	CommentsCount int                `json:"commentsCount"`
	CustomFields  []ytRawCustomField `json:"customFields"`
}

type ytRawWatchers struct {
	HasStar       bool `json:"hasStar"`
	IssueWatchers []struct {
		User ytRawRef `json:"user"`
	} `json:"issueWatchers"`
}

type ytRawActivity struct {
	ID        string    `json:"id"`
	Type      string    `json:"$type"`
	Timestamp int64     `json:"timestamp"`
	Author    *ytRawRef `json:"author"`
	Category  struct {
		ID string `json:"id"`
	} `json:"category"`
	Target *struct {
		Type       string `json:"$type"`
		ID         string `json:"id"`
		IDReadable string `json:"idReadable"`
		// Comment, attachment and work-item activities target that entity, not
		// the issue; the issue is one level down.
		Issue *struct {
			IDReadable string `json:"idReadable"`
		} `json:"issue"`
	} `json:"target"`
	Field *struct {
		Presentation string `json:"presentation"`
		ID           string `json:"id"`
		CustomField  *struct {
			FieldType struct {
				ID string `json:"id"`
			} `json:"fieldType"`
		} `json:"customField"`
	} `json:"field"`
	Added   json.RawMessage `json:"added"`
	Removed json.RawMessage `json:"removed"`
}

type ytRawWorkItem struct {
	ID       string `json:"id"`
	Date     int64  `json:"date"`
	Duration struct {
		Minutes      int    `json:"minutes"`
		Presentation string `json:"presentation"`
	} `json:"duration"`
	Text    string    `json:"text"`
	Type    *ytRawRef `json:"type"`
	Author  *ytRawRef `json:"author"`
	Creator *ytRawRef `json:"creator"`
	Created int64     `json:"created"`
}

type ytRawProjectField struct {
	ID             string `json:"id"`
	Type           string `json:"$type"`
	CanBeEmpty     bool   `json:"canBeEmpty"`
	EmptyFieldText string `json:"emptyFieldText"`
	IsPublic       bool   `json:"isPublic"`
	Field          struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		LocalizedName string `json:"localizedName"`
		FieldType     struct {
			ID string `json:"id"`
		} `json:"fieldType"`
	} `json:"field"`
	Bundle *struct {
		ID     string `json:"id"`
		Type   string `json:"$type"`
		Values []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			IsResolved *bool  `json:"isResolved"`
			Archived   bool   `json:"archived"`
		} `json:"values"`
	} `json:"bundle"`
}

// --- Rendered (model-facing) shapes ---

type ytIssueOut struct {
	ID            string            `json:"id"`
	InternalID    string            `json:"internal_id"`
	Project       string            `json:"project"`
	Summary       string            `json:"summary"`
	Description   *string           `json:"description,omitempty"`
	Reporter      string            `json:"reporter,omitempty"`
	Updater       string            `json:"updater,omitempty"`
	Created       string            `json:"created"`
	Updated       string            `json:"updated"`
	Resolved      *string           `json:"resolved"`
	Fields        map[string]any    `json:"fields"`
	Tags          []string          `json:"tags,omitempty"`
	Visibility    string            `json:"visibility,omitempty"`
	Links         []ytLinkOut       `json:"links,omitempty"`
	Attachments   []ytAttachmentOut `json:"attachments,omitempty"`
	Watchers      []string          `json:"watchers,omitempty"`
	Starred       *bool             `json:"starred,omitempty"`
	CommentsCount *int              `json:"comments_count,omitempty"`
	Comments      []ytCommentOut    `json:"comments,omitempty"`
	URL           string            `json:"url,omitempty"`
}

type ytLinkOut struct {
	Type   string   `json:"type"`
	Role   string   `json:"role"`
	Issues []string `json:"issues"`
}

type ytAttachmentOut struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type,omitempty"`
	Author   string `json:"author,omitempty"`
	Created  string `json:"created,omitempty"`
	Comment  string `json:"comment_id,omitempty"`
	Uploaded bool   `json:"uploaded,omitempty"`
}

type ytCommentOut struct {
	ID          string            `json:"id"`
	Author      string            `json:"author,omitempty"`
	Created     string            `json:"created"`
	Updated     *string           `json:"updated,omitempty"`
	Text        string            `json:"text"`
	Visibility  string            `json:"visibility,omitempty"`
	Deleted     bool              `json:"deleted,omitempty"`
	Attachments []ytAttachmentOut `json:"attachments,omitempty"`
}

type ytActivityOut struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Timestamp string   `json:"timestamp"`
	Author    string   `json:"author,omitempty"`
	Issue     string   `json:"issue,omitempty"`
	Field     string   `json:"field,omitempty"`
	Removed   []string `json:"removed,omitempty"`
	Added     []string `json:"added,omitempty"`
}

type ytWorkItemOut struct {
	ID           string `json:"id"`
	Date         string `json:"date"`
	Minutes      int    `json:"minutes"`
	Presentation string `json:"duration"`
	Text         string `json:"text,omitempty"`
	Type         string `json:"type,omitempty"`
	Author       string `json:"author,omitempty"`
	Created      string `json:"created,omitempty"`
}

type ytProjectFieldOut struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Required       bool     `json:"required"`
	EmptyText      string   `json:"empty_text,omitempty"`
	Values         []string `json:"values,omitempty"`
	ResolvedValues []string `json:"resolved_values,omitempty"`
	ArchivedValues []string `json:"archived_values,omitempty"`
}

// --- Renderers ---

func ytRefName(r *ytRawRef) string {
	if r == nil {
		return ""
	}
	if r.Login != "" {
		return r.Login
	}
	if r.IDReadable != "" {
		return r.IDReadable
	}
	if r.ShortName != "" {
		return r.ShortName
	}
	return r.Name
}

func ytVisibility(v *ytRawVisibility) string {
	if v == nil || v.Type == "" || v.Type == "UnlimitedVisibility" {
		return ""
	}
	var parts []string
	for _, g := range v.PermittedGroups {
		parts = append(parts, "group "+g.Name)
	}
	for _, u := range v.PermittedUsers {
		parts = append(parts, "user "+u.Login)
	}
	if len(parts) == 0 {
		return "limited"
	}
	return "limited to " + strings.Join(parts, ", ")
}

// renderFieldValue turns a raw custom field value into its model-facing form.
func renderFieldValue(f ytRawCustomField, loc *time.Location) any {
	raw := strings.TrimSpace(string(f.Value))
	if raw == "" || raw == "null" {
		return nil
	}
	switch raw[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(f.Value, &items); err != nil {
			return raw
		}
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, renderFieldValue(ytRawCustomField{Value: it, ProjectCustomField: f.ProjectCustomField}, loc))
		}
		return out
	case '{':
		var obj map[string]any
		if err := json.Unmarshal(f.Value, &obj); err != nil {
			return raw
		}
		if m, ok := obj["minutes"]; ok {
			out := map[string]any{"minutes": m}
			if p, ok := obj["presentation"]; ok {
				out["presentation"] = p
			}
			return out
		}
		if login, ok := obj["login"].(string); ok && login != "" {
			return login
		}
		if text, ok := obj["text"]; ok {
			return text
		}
		if name, ok := obj["name"].(string); ok {
			return name
		}
		delete(obj, "$type")
		return obj
	case '"':
		var s string
		if err := json.Unmarshal(f.Value, &s); err != nil {
			return raw
		}
		return s
	case 't', 'f':
		return raw == "true"
	default:
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return raw
		}
		switch f.fieldType() {
		case "date":
			return ytDate(int64(n), loc)
		case "date and time":
			return ytTime(int64(n), loc)
		}
		if n == float64(int64(n)) {
			return int64(n)
		}
		return n
	}
}

func renderFields(fields []ytRawCustomField, loc *time.Location) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		out[f.Name] = renderFieldValue(f, loc)
	}
	return out
}

func renderAttachment(a ytRawAttachment, loc *time.Location) ytAttachmentOut {
	out := ytAttachmentOut{
		ID:       a.ID,
		Name:     a.Name,
		Size:     a.Size,
		MimeType: a.MimeType,
		Author:   ytRefName(a.Author),
	}
	if a.Created != 0 {
		out.Created = ytTime(a.Created, loc)
	}
	if a.Comment != nil {
		out.Comment = a.Comment.ID
	}
	return out
}

func renderAttachments(list []ytRawAttachment, loc *time.Location) []ytAttachmentOut {
	out := make([]ytAttachmentOut, 0, len(list))
	for _, a := range list {
		out = append(out, renderAttachment(a, loc))
	}
	return out
}

func renderLinks(links []ytRawLink) []ytLinkOut {
	var out []ytLinkOut
	for _, l := range links {
		if len(l.Issues) == 0 {
			continue
		}
		ids := make([]string, 0, len(l.Issues))
		for _, i := range l.Issues {
			ids = append(ids, i.IDReadable)
		}
		out = append(out, ytLinkOut{Type: l.LinkType.Name, Role: l.role(), Issues: ids})
	}
	return out
}

func renderComment(c ytRawComment, loc *time.Location) ytCommentOut {
	out := ytCommentOut{
		ID:         c.ID,
		Author:     ytRefName(c.Author),
		Created:    ytTime(c.Created, loc),
		Updated:    ytTimePtr(c.Updated, loc),
		Text:       c.Text,
		Visibility: ytVisibility(c.Visibility),
		Deleted:    c.Deleted,
	}
	if len(c.Attachments) > 0 {
		out.Attachments = renderAttachments(c.Attachments, loc)
	}
	return out
}

// renderIssue renders an issue fetched with ytIssueFields (full) or one of
// the compact selectors (fields the selector did not request stay empty and
// are omitted from the JSON).
func (y *YouTrackREST) renderIssue(i ytRawIssue, loc *time.Location, full bool) ytIssueOut {
	out := ytIssueOut{
		ID:          i.IDReadable,
		InternalID:  i.ID,
		Project:     i.Project.ShortName,
		Summary:     i.Summary,
		Description: i.Description,
		Reporter:    ytRefName(i.Reporter),
		Updater:     ytRefName(i.Updater),
		Created:     ytTime(i.Created, loc),
		Updated:     ytTime(i.Updated, loc),
		Resolved:    ytTimePtr(i.Resolved, loc),
		Fields:      renderFields(i.CustomFields, loc),
	}
	if !full {
		return out
	}
	for _, t := range i.Tags {
		out.Tags = append(out.Tags, t.Name)
	}
	out.Visibility = ytVisibility(i.Visibility)
	out.Links = renderLinks(i.Links)
	if len(i.Attachments) > 0 {
		out.Attachments = renderAttachments(i.Attachments, loc)
	}
	if i.Watchers != nil {
		for _, w := range i.Watchers.IssueWatchers {
			out.Watchers = append(out.Watchers, w.User.Login)
		}
		starred := i.Watchers.HasStar
		out.Starred = &starred
	}
	count := i.CommentsCount
	out.CommentsCount = &count
	out.URL = y.baseURL + "/issue/" + i.IDReadable
	return out
}

// ytActivityTypeName shortens "CustomFieldActivityItem" to "field" and the
// other item types to one word each.
func ytActivityTypeName(t string) string {
	switch t {
	case "CustomFieldActivityItem":
		return "field"
	case "CommentActivityItem":
		return "comment"
	case "LinksActivityItem":
		return "link"
	case "AttachmentActivityItem":
		return "attachment"
	case "IssueCreatedActivityItem":
		return "created"
	case "IssueResolvedActivityItem":
		return "resolved"
	case "TextCustomFieldActivityItem", "SimpleValueActivityItem", "TextMarkupActivityItem":
		return "field"
	}
	s := strings.TrimSuffix(t, "ActivityItem")
	if s == "" {
		return t
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ytActivityValues renders the added/removed side of an activity item. The
// payload is polymorphic (field values, users, issues, comments, plain
// values); each element becomes the most telling string it carries. kind
// says how a bare number is to be read: "date", "date and time" and "period"
// for custom-field changes of those types, "timestamp" for resolution times,
// anything else leaves the number as written (never in exponent form).
func ytActivityValues(raw json.RawMessage, kind string, loc *time.Location) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "[]" {
		return nil
	}
	var items []json.RawMessage
	if s[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return []string{s}
		}
	} else {
		items = []json.RawMessage{raw}
	}
	var out []string
	for _, it := range items {
		t := strings.TrimSpace(string(it))
		if t == "" || t == "null" {
			continue
		}
		if t[0] != '{' {
			dec := json.NewDecoder(strings.NewReader(t))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				out = append(out, t)
				continue
			}
			if n, ok := v.(json.Number); ok {
				out = append(out, ytActivityNumber(n, kind, loc))
				continue
			}
			out = append(out, fmt.Sprint(v))
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(it, &obj); err != nil {
			out = append(out, t)
			continue
		}
		switch {
		case str(obj["text"]) != "":
			out = append(out, str(obj["text"]))
		case str(obj["idReadable"]) != "":
			if sum := str(obj["summary"]); sum != "" {
				out = append(out, str(obj["idReadable"])+" "+sum)
			} else {
				out = append(out, str(obj["idReadable"]))
			}
		case str(obj["login"]) != "":
			out = append(out, str(obj["login"]))
		case str(obj["presentation"]) != "":
			out = append(out, str(obj["presentation"]))
		case str(obj["name"]) != "":
			out = append(out, str(obj["name"]))
		case str(obj["id"]) != "":
			out = append(out, str(obj["id"]))
		}
	}
	return out
}

// ytActivityNumber renders a numeric activity value according to kind.
func ytActivityNumber(n json.Number, kind string, loc *time.Location) string {
	ms, err := n.Int64()
	if err != nil {
		return n.String()
	}
	switch kind {
	case "date":
		return ytDate(ms, loc)
	case "date and time", "timestamp":
		return ytTime(ms, loc)
	case "period":
		// Period changes arrive as bare minutes (1562 → 26h 2m).
		return ytPeriodPresentation(int(ms))
	}
	return n.String()
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func renderActivity(a ytRawActivity, loc *time.Location) ytActivityOut {
	kind := ""
	switch {
	case a.Type == "IssueResolvedActivityItem":
		kind = "timestamp"
	case a.Field != nil && a.Field.CustomField != nil:
		kind = a.Field.CustomField.FieldType.ID
	}
	out := ytActivityOut{
		ID:        a.ID,
		Type:      ytActivityTypeName(a.Type),
		Timestamp: ytTime(a.Timestamp, loc),
		Author:    ytRefName(a.Author),
		Removed:   ytActivityValues(a.Removed, kind, loc),
		Added:     ytActivityValues(a.Added, kind, loc),
	}
	if a.Target != nil {
		switch {
		case a.Target.IDReadable != "":
			out.Issue = a.Target.IDReadable
		case a.Target.Issue != nil:
			out.Issue = a.Target.Issue.IDReadable
		}
	}
	if a.Field != nil {
		out.Field = a.Field.Presentation
	}
	return out
}

func renderWorkItem(w ytRawWorkItem, loc *time.Location) ytWorkItemOut {
	out := ytWorkItemOut{
		ID: w.ID,
		// Work item dates are stored at 00:00 UTC (see ytParseWorkDate); read
		// in UTC they name the same day everywhere, read in a zone west of UTC
		// they would name the day before.
		Date:         ytDate(w.Date, time.UTC),
		Minutes:      w.Duration.Minutes,
		Presentation: w.Duration.Presentation,
		Text:         w.Text,
		Type:         ytRefName(w.Type),
		Author:       ytRefName(w.Author),
	}
	if w.Created != 0 {
		out.Created = ytTime(w.Created, loc)
	}
	return out
}

func renderProjectField(f ytRawProjectField) ytProjectFieldOut {
	out := ytProjectFieldOut{
		Name:      f.Field.Name,
		Type:      f.Field.FieldType.ID,
		Required:  !f.CanBeEmpty,
		EmptyText: f.EmptyFieldText,
	}
	if f.Bundle != nil {
		for _, v := range f.Bundle.Values {
			switch {
			case v.Archived:
				out.ArchivedValues = append(out.ArchivedValues, v.Name)
			case v.IsResolved != nil && *v.IsResolved:
				out.ResolvedValues = append(out.ResolvedValues, v.Name)
			default:
				out.Values = append(out.Values, v.Name)
			}
		}
	}
	return out
}

// --- Encoding field values for writes ---

// ytWriteType returns the $type YouTrack expects when a field of the given
// project field type is written. The current $type of the issue's field is
// preferred when known (state machines use StateMachineIssueCustomField).
func ytWriteType(fieldType, current string) (string, error) {
	if current != "" {
		return current, nil
	}
	switch fieldType {
	case "enum[1]":
		return "SingleEnumIssueCustomField", nil
	case "enum[*]":
		return "MultiEnumIssueCustomField", nil
	case "state[1]":
		return "StateIssueCustomField", nil
	case "user[1]":
		return "SingleUserIssueCustomField", nil
	case "user[*]":
		return "MultiUserIssueCustomField", nil
	case "version[1]":
		return "SingleVersionIssueCustomField", nil
	case "version[*]":
		return "MultiVersionIssueCustomField", nil
	case "build[1]":
		return "SingleBuildIssueCustomField", nil
	case "build[*]":
		return "MultiBuildIssueCustomField", nil
	case "ownedField[1]":
		return "SingleOwnedIssueCustomField", nil
	case "ownedField[*]":
		return "MultiOwnedIssueCustomField", nil
	case "group[1]":
		return "SingleGroupIssueCustomField", nil
	case "group[*]":
		return "MultiGroupIssueCustomField", nil
	case "date", "date and time":
		return "DateIssueCustomField", nil
	case "integer", "float", "string":
		return "SimpleIssueCustomField", nil
	case "period":
		return "PeriodIssueCustomField", nil
	case "text":
		return "TextIssueCustomField", nil
	}
	return "", fmt.Errorf("field type %q is not supported for writing", fieldType)
}

// ytEncodeValue converts a caller value into the JSON shape YouTrack expects
// for a field of the given project field type. nil clears the field.
func ytEncodeValue(name, fieldType string, v any, loc *time.Location) (any, error) {
	if v == nil {
		if strings.HasSuffix(fieldType, "[*]") {
			return []any{}, nil // a multi-value field is cleared with an empty list
		}
		return nil, nil
	}
	named := func(key string) (any, error) {
		switch t := v.(type) {
		case string:
			return map[string]any{key: t}, nil
		default:
			return nil, fmt.Errorf("field %q expects a single %s as a string, got %T", name, key, v)
		}
	}
	namedList := func(key string) (any, error) {
		switch t := v.(type) {
		case string:
			return []any{map[string]any{key: t}}, nil
		case []any:
			out := make([]any, 0, len(t))
			for _, e := range t {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("field %q expects a list of %ss as strings, got %T", name, key, e)
				}
				out = append(out, map[string]any{key: s})
			}
			return out, nil
		case []string:
			out := make([]any, 0, len(t))
			for _, s := range t {
				out = append(out, map[string]any{key: s})
			}
			return out, nil
		default:
			return nil, fmt.Errorf("field %q expects a list of %ss, got %T", name, key, v)
		}
	}
	switch fieldType {
	case "enum[1]", "state[1]", "version[1]", "build[1]", "ownedField[1]", "group[1]":
		return named("name")
	case "enum[*]", "version[*]", "build[*]", "ownedField[*]", "group[*]":
		return namedList("name")
	case "user[1]":
		return named("login")
	case "user[*]":
		return namedList("login")
	case "date":
		ms, err := ytToMillis(v, loc, true)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		return ms, nil
	case "date and time":
		ms, err := ytToMillis(v, loc, false)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		return ms, nil
	case "integer":
		switch t := v.(type) {
		case float64:
			if t != float64(int64(t)) {
				return nil, fmt.Errorf("field %q expects an integer, got %v", name, t)
			}
			return int64(t), nil
		case json.Number:
			n, err := t.Int64()
			if err != nil {
				return nil, fmt.Errorf("field %q expects an integer, got %s", name, t)
			}
			return n, nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("field %q expects an integer, got %q", name, t)
			}
			return n, nil
		}
		return nil, fmt.Errorf("field %q expects an integer, got %T", name, v)
	case "float":
		switch t := v.(type) {
		case float64:
			return t, nil
		case json.Number:
			f, err := t.Float64()
			if err != nil {
				return nil, fmt.Errorf("field %q expects a number, got %s", name, t)
			}
			return f, nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
			if err != nil {
				return nil, fmt.Errorf("field %q expects a number, got %q", name, t)
			}
			return f, nil
		}
		return nil, fmt.Errorf("field %q expects a number, got %T", name, v)
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("field %q expects a string, got %T", name, v)
		}
		return s, nil
	case "text":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("field %q expects a string, got %T", name, v)
		}
		return map[string]any{"text": s}, nil
	case "period":
		minutes, err := ytParsePeriod(v)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		// PeriodValue carries both; the document marks both read-only, so the
		// write shape is sent in both forms until the live write test settles it.
		return map[string]any{"minutes": minutes, "presentation": ytPeriodPresentation(minutes)}, nil
	}
	return nil, fmt.Errorf("field %q has type %q, which this connection cannot write", name, fieldType)
}

// ytPeriodPresentation renders minutes the way YouTrack does ("1h 30m", "45m").
func ytPeriodPresentation(minutes int) string {
	h, m := minutes/60, minutes%60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}

// ytFieldNames returns the sorted names of a project's fields — for error
// messages that tell the caller what exists.
func ytFieldNames(fields []ytRawProjectField) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Field.Name)
	}
	sort.Strings(names)
	return names
}

// ytFindProjectField resolves a field name case-insensitively against a
// project's fields.
func ytFindProjectField(fields []ytRawProjectField, name string) (ytRawProjectField, bool) {
	for _, f := range fields {
		if strings.EqualFold(f.Field.Name, name) {
			return f, true
		}
	}
	return ytRawProjectField{}, false
}
