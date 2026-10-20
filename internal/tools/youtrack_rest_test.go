package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// A fake YouTrack: enough of the REST API for the handlers to exercise their
// shaping, paging, guards and encodings. Requests are recorded so tests can
// assert on what went out. Shapes follow what a production instance returned
// while the type was built (field $types, link ids, activity items, error
// bodies); the fake ignores fields= and answers full objects.

type ytFake struct {
	mu       sync.Mutex
	requests []ytFakeRequest
	srv      *httptest.Server
}

type ytFakeRequest struct {
	Method string
	Path   string
	Query  map[string]string
	Body   string
	Files  []string // multipart file parts as field=filename, sorted
}

const (
	ytFakeDueDate  = int64(1792065600000) // 2026-10-15T12:00:00Z — a date-only field as YouTrack stores it
	ytFakeCreated  = int64(1792054800000) // 2026-10-15T09:00:00Z
	ytFakeMidnight = int64(1792022400000) // 2026-10-15T00:00:00Z — work item date / sprint start convention
)

func ytFakeIssueJSON(id, internalID string, department any) string {
	dep := "null"
	if department != nil {
		dep = fmt.Sprintf(`{"name":%q,"$type":"EnumBundleElement"}`, department)
	}
	return fmt.Sprintf(`{
	  "id":%q,"idReadable":%q,"summary":"Fix the thing","description":"Long text",
	  "created":%d,"updated":%d,"resolved":null,
	  "project":{"id":"77-27","shortName":"P","name":"Projects","$type":"Project"},
	  "reporter":{"login":"jane.doe","fullName":"Jane Doe"},"updater":{"login":"john.roe"},
	  "tags":[{"name":"reviewed"}],
	  "visibility":{"$type":"UnlimitedVisibility"},
	  "watchers":{"hasStar":true,"issueWatchers":[{"user":{"login":"john.roe"}}]},
	  "links":[
	    {"id":"57-3s","direction":"OUTWARD","linkType":{"id":"57-3","name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of","directed":true},"issues":[{"id":"2-101","idReadable":"I-1","summary":"child"}]},
	    {"id":"57-3t","direction":"INWARD","linkType":{"id":"57-3","name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of","directed":true},"issues":[]},
	    {"id":"57-0","direction":"BOTH","linkType":{"id":"57-0","name":"Relates","sourceToTarget":"relates to","targetToSource":"","directed":false},"issues":[]}
	  ],
	  "attachments":[{"id":"92-1","name":"shot.png","size":68,"mimeType":"image/png","created":%d,"author":{"login":"jane.doe"},"url":"/api/files/92-1?sign=abc"}],
	  "commentsCount":101,
	  "customFields":[
	    {"id":"1","name":"Priority","$type":"SingleEnumIssueCustomField","value":{"name":"High","$type":"EnumBundleElement"},"projectCustomField":{"field":{"name":"Priority","fieldType":{"id":"enum[1]"}}}},
	    {"id":"2","name":"State","$type":"StateIssueCustomField","value":{"name":"Open","isResolved":false,"$type":"StateBundleElement"},"projectCustomField":{"field":{"name":"State","fieldType":{"id":"state[1]"}}}},
	    {"id":"3","name":"Assignee","$type":"SingleUserIssueCustomField","value":{"login":"jane.doe","fullName":"Jane Doe","$type":"User"},"projectCustomField":{"field":{"name":"Assignee","fieldType":{"id":"user[1]"}}}},
	    {"id":"4","name":"Due date","$type":"DateIssueCustomField","value":%d,"projectCustomField":{"field":{"name":"Due date","fieldType":{"id":"date"}}}},
	    {"id":"5","name":"Spent time","$type":"PeriodIssueCustomField","value":{"minutes":270,"presentation":"4h 30m","$type":"PeriodValue"},"projectCustomField":{"field":{"name":"Spent time","fieldType":{"id":"period"}}}},
	    {"id":"6","name":"Fix versions","$type":"MultiVersionIssueCustomField","value":[{"name":"2026.20"},{"name":"2026.21"}],"projectCustomField":{"field":{"name":"Fix versions","fieldType":{"id":"version[*]"}}}},
	    {"id":"7","name":"Sprint priority","$type":"MultiVersionIssueCustomField","value":[],"projectCustomField":{"field":{"name":"Sprint priority","fieldType":{"id":"version[*]"}}}},
	    {"id":"8","name":"Department","$type":"SingleEnumIssueCustomField","value":%s,"projectCustomField":{"field":{"name":"Department","fieldType":{"id":"enum[1]"}}}},
	    {"id":"9","name":"Estimation","$type":"SimpleIssueCustomField","value":3,"projectCustomField":{"field":{"name":"Estimation","fieldType":{"id":"integer"}}}}
	  ],"$type":"Issue"}`, internalID, id, ytFakeCreated, ytFakeCreated, ytFakeCreated, ytFakeDueDate, dep)
}

const ytFakeProjectFields = `[
  {"id":"78-1","$type":"EnumProjectCustomField","canBeEmpty":false,"emptyFieldText":"No Priority","field":{"name":"Priority","fieldType":{"id":"enum[1]"}},"bundle":{"id":"41-1","$type":"EnumBundle","values":[{"name":"Low","archived":false},{"name":"High","archived":false},{"name":"Old","archived":true}]}},
  {"id":"78-2","$type":"StateProjectCustomField","canBeEmpty":false,"field":{"name":"State","fieldType":{"id":"state[1]"}},"bundle":{"id":"44-1","$type":"StateBundle","values":[{"name":"Open","isResolved":false},{"name":"Closed","isResolved":true}]}},
  {"id":"78-3","$type":"UserProjectCustomField","canBeEmpty":true,"field":{"name":"Assignee","fieldType":{"id":"user[1]"}}},
  {"id":"78-4","$type":"SimpleProjectCustomField","canBeEmpty":true,"field":{"name":"Due date","fieldType":{"id":"date"}}},
  {"id":"78-5","$type":"PeriodProjectCustomField","canBeEmpty":true,"field":{"name":"Spent time","fieldType":{"id":"period"}}},
  {"id":"78-6","$type":"VersionProjectCustomField","canBeEmpty":true,"field":{"name":"Fix versions","fieldType":{"id":"version[*]"}}},
  {"id":"78-7","$type":"VersionProjectCustomField","canBeEmpty":true,"field":{"name":"Sprint priority","localizedName":"Sprint-Priorität","fieldType":{"id":"version[*]"}}},
  {"id":"78-8","$type":"EnumProjectCustomField","canBeEmpty":true,"field":{"name":"Department","fieldType":{"id":"enum[1]"}}},
  {"id":"78-9","$type":"SimpleProjectCustomField","canBeEmpty":true,"field":{"name":"Estimation","fieldType":{"id":"integer"}}},
  {"id":"78-10","$type":"SimpleProjectCustomField","canBeEmpty":true,"field":{"name":"Deadline","fieldType":{"id":"date and time"}}}
]`

// 1x1 transparent PNG.
var ytFakePNG = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 0x49, 0x48, 0x44, 0x52, 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89, 0, 0, 0, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1, 0x0d, 0x0a, 0x2d, 0xb4, 0, 0, 0, 0, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}

// ytFakeComments renders one page of the 101-comment thread of P-1: the
// first two comments carry distinct authors and visibility, the rest are
// filler, the 101st sits alone on the second page.
func ytFakeComments(skip int) string {
	var parts []string
	for i := skip; i < skip+youtrackPageSize && i < 101; i++ {
		switch i {
		case 0:
			parts = append(parts, fmt.Sprintf(`{"id":"4-1","text":"first","created":%d,"author":{"login":"jane.doe"},"visibility":{"$type":"UnlimitedVisibility"}}`, ytFakeCreated))
		case 1:
			parts = append(parts, fmt.Sprintf(`{"id":"4-2","text":"second","created":%d,"updated":%d,"author":{"login":"john.roe"},"visibility":{"$type":"LimitedVisibility","permittedGroups":[{"name":"IT"}]}}`, ytFakeCreated+3600000, ytFakeCreated+7200000))
		default:
			parts = append(parts, fmt.Sprintf(`{"id":"4-%d","text":"comment %d","created":%d,"author":{"login":"jane.doe"}}`, i+1, i+1, ytFakeCreated+int64(i)*60000))
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

const ytFakeActivities = `[
  {"id":"a-1","$type":"CustomFieldActivityItem","timestamp":1792054800000,"author":{"login":"jane.doe"},"category":{"id":"CustomFieldCategory"},"target":{"$type":"Issue","id":"2-100","idReadable":"P-1"},"field":{"presentation":"State","id":"state","customField":{"fieldType":{"id":"state[1]"}}},"added":[{"name":"In Progress","$type":"StateBundleElement"}],"removed":[{"name":"Open","$type":"StateBundleElement"}]},
  {"id":"a-2","$type":"CommentActivityItem","timestamp":1792058400000,"author":{"login":"john.roe"},"category":{"id":"CommentsCategory"},"target":{"$type":"IssueComment","id":"4-2","issue":{"idReadable":"P-1"}},"added":[{"text":"second","$type":"IssueComment"}],"removed":[]},
  {"id":"a-3","$type":"SimpleValueActivityItem","timestamp":1792062000000,"author":{"login":"jane.doe"},"category":{"id":"CustomFieldCategory"},"target":{"$type":"Issue","id":"2-100","idReadable":"P-1"},"field":{"presentation":"Due date","id":"due","customField":{"fieldType":{"id":"date"}}},"added":1792065600000,"removed":1791979200000},
  {"id":"a-4","$type":"IssueResolvedActivityItem","timestamp":1792065600000,"author":{"login":"jane.doe"},"category":{"id":"IssueResolvedCategory"},"target":{"$type":"Issue","id":"2-100","idReadable":"P-1"},"field":{"presentation":"resolved","id":"resolved"},"added":1792065600000,"removed":null},
  {"id":"a-5","$type":"SimpleValueActivityItem","timestamp":1792065700000,"author":{"login":"jane.doe"},"category":{"id":"CustomFieldCategory"},"target":{"$type":"Issue","id":"2-100","idReadable":"P-1"},"field":{"presentation":"Estimation","id":"est","customField":{"fieldType":{"id":"integer"}}},"added":1500000,"removed":3},
  {"id":"a-6","$type":"CustomFieldActivityItem","timestamp":1792065800000,"author":{"login":"jane.doe"},"category":{"id":"CustomFieldCategory"},"target":{"$type":"Issue","id":"2-100","idReadable":"P-1"},"field":{"presentation":"Spent time","id":"spent","customField":{"fieldType":{"id":"period"}}},"added":1652,"removed":1562}
]`

func newYTFake(t *testing.T) *ytFake {
	t.Helper()
	f := &ytFake{}
	mux := http.NewServeMux()
	record := func(r *http.Request) ytFakeRequest {
		rec := ytFakeRequest{Method: r.Method, Path: r.URL.Path, Query: map[string]string{}}
		for k, v := range r.URL.Query() {
			rec.Query[k] = v[0]
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err == nil {
				for field, fhs := range r.MultipartForm.File {
					for _, fh := range fhs {
						rec.Files = append(rec.Files, field+"="+fh.Filename)
					}
				}
				sort.Strings(rec.Files)
			}
		} else if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			rec.Body = string(b)
		}
		f.mu.Lock()
		f.requests = append(f.requests, rec)
		f.mu.Unlock()
		return rec
	}
	jsonOK := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
	attachments := `[{"id":"92-1","name":"shot.png","size":68,"mimeType":"image/png","created":1792054800000,"author":{"login":"jane.doe"},"url":"/api/files/92-1?sign=abc"},` +
		`{"id":"92-3","name":"notes.txt","size":11,"mimeType":"text/plain","created":1792054800000,"author":{"login":"jane.doe"},"url":"/api/files/92-3?sign=def"},` +
		`{"id":"92-4","name":"blob.bin","size":4,"mimeType":"application/octet-stream","created":1792054800000,"author":{"login":"jane.doe"},"url":"/api/files/92-4?sign=ghi"}%s]`
	var uploaded []string

	mux.HandleFunc("/api/users/me", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Header.Get("Authorization") != "Bearer perm:test" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"Unauthorized","error_description":"You are not logged in."}`)
			return
		}
		jsonOK(w, `{"id":"26-0","login":"jane.doe","fullName":"Jane Doe","email":"jane@example.com","profiles":{"general":{"timezone":{"id":"Europe/Berlin","offset":120}}}}`)
	})
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"26-5","login":"john.roe","fullName":"John Roe","email":"john@example.com","banned":false},{"id":"26-6","login":"john.roe.test","fullName":"Test","banned":true}]`)
	})
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.URL.Query().Get("$skip") != "0" {
			jsonOK(w, `[]`)
			return
		}
		jsonOK(w, `[{"id":"25-1","name":"IT"},{"id":"25-2","name":"Reporters"}]`)
	})
	mux.HandleFunc("/api/issues", func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		if r.Method == http.MethodPost {
			jsonOK(w, ytFakeIssueJSON("P-3", "2-300", nil))
			return
		}
		if rec.Query["$skip"] == "50" {
			jsonOK(w, `[]`)
			return
		}
		jsonOK(w, "["+ytFakeIssueJSON("P-1", "2-100", "IT")+","+ytFakeIssueJSON("P-2", "2-200", nil)+"]")
	})
	mux.HandleFunc("/api/issues/P-1", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, ytFakeIssueJSON("P-1", "2-100", "IT"))
	})
	mux.HandleFunc("/api/issues/P-2", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, ytFakeIssueJSON("P-2", "2-200", nil))
	})
	mux.HandleFunc("/api/issues/P-404", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"Not Found","error_description":"Entity with id P-404 not found"}`)
	})
	mux.HandleFunc("/api/issues/P-1/comments", func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		if r.Method == http.MethodPost {
			jsonOK(w, `{"id":"4-9","text":"posted","created":1792054800000,"author":{"login":"jane.doe"},"visibility":{"$type":"LimitedVisibility","permittedGroups":[{"name":"IT"}],"permittedUsers":[]}}`)
			return
		}
		skip := 0
		fmt.Sscanf(rec.Query["$skip"], "%d", &skip)
		jsonOK(w, ytFakeComments(skip))
	})
	// One comment of P-1 with state, kept the way YouTrack keeps it: POST
	// changes text and visibility, DELETE marks it deleted and leaves it
	// readable, GET reads it back. Ids sent in a visibility come back as the
	// names a real instance would resolve them to.
	comment := struct {
		text, visibility string
		deleted          bool
	}{text: "posted", visibility: `{"$type":"UnlimitedVisibility"}`}
	visibilityNames := strings.NewReplacer(`{"id":"25-1"}`, `{"name":"IT"}`, `{"id":"26-5"}`, `{"login":"john.roe"}`)
	mux.HandleFunc("/api/issues/P-1/comments/4-9", func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		switch r.Method {
		case http.MethodPost:
			var body struct {
				Text       *string         `json:"text"`
				Visibility json.RawMessage `json:"visibility"`
			}
			json.Unmarshal([]byte(rec.Body), &body)
			if body.Text != nil {
				comment.text = *body.Text
			}
			if body.Visibility != nil {
				comment.visibility = visibilityNames.Replace(string(body.Visibility))
			}
		case http.MethodDelete:
			comment.deleted = true
			return
		}
		jsonOK(w, fmt.Sprintf(`{"id":"4-9","text":%q,"created":%d,"updated":%d,"author":{"login":"jane.doe"},"deleted":%t,"visibility":%s}`,
			comment.text, ytFakeCreated, ytFakeCreated+60000, comment.deleted, comment.visibility))
	})
	// A comment the API no longer returns once it is deleted.
	mux.HandleFunc("/api/issues/P-1/comments/4-8", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Method == http.MethodDelete {
			return
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"Not Found","error_description":"Entity with id 4-8 not found"}`)
	})
	// Someone else's comment the account may not change.
	mux.HandleFunc("/api/issues/P-1/comments/4-7", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(403)
		io.WriteString(w, `{"error":"Forbidden","error_description":"You have no permission to update the comment"}`)
	})
	mux.HandleFunc("/api/issues/P-1/attachments", func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		if r.Method == http.MethodPost {
			for _, f := range rec.Files {
				_, name, _ := strings.Cut(f, "=")
				uploaded = append(uploaded, name)
			}
			jsonOK(w, `{"id":"92-2","name":"one","size":1}`)
			return
		}
		extra := ""
		for i, name := range uploaded {
			extra += fmt.Sprintf(`,{"id":"92-%d","name":%q,"size":5,"mimeType":"text/plain","url":"/api/files/92-%d"}`, 10+i, name, 10+i)
		}
		jsonOK(w, fmt.Sprintf(attachments, extra))
	})
	mux.HandleFunc("/api/files/92-1", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "image/png")
		w.Write(ytFakePNG)
	})
	mux.HandleFunc("/api/files/92-3", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello notes")
	})
	mux.HandleFunc("/api/files/92-4", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 1, 2, 3})
	})
	mux.HandleFunc("/api/issues/P-1/links", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"57-3s","direction":"OUTWARD","linkType":{"id":"57-3","name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of","directed":true},"issues":[]},{"id":"57-3t","direction":"INWARD","linkType":{"id":"57-3","name":"Subtask","sourceToTarget":"parent for","targetToSource":"subtask of","directed":true},"issues":[]},{"id":"57-0","direction":"BOTH","linkType":{"id":"57-0","name":"Relates","sourceToTarget":"relates to","targetToSource":"","directed":false},"issues":[]}]`)
	})
	mux.HandleFunc("/api/issues/P-1/links/57-3s/issues", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `{"id":"2-200","idReadable":"P-2"}`)
	})
	mux.HandleFunc("/api/issues/P-1/watchers/issueWatchers", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `{"user":{"login":"john.roe"},"isStarred":true}`)
	})
	mux.HandleFunc("/api/issues/P-1/timeTracking/workItems", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, fmt.Sprintf(`{"id":"142-1","date":%d,"duration":{"minutes":90,"presentation":"1h 30m"},"text":"review","type":{"id":"63-0","name":"Development"},"author":{"login":"jane.doe"},"created":%d}`, ytFakeMidnight, ytFakeCreated))
	})
	mux.HandleFunc("/api/issues/P-1/activities", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, ytFakeActivities)
	})
	mux.HandleFunc("/api/activities", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, ytFakeActivities)
	})
	mux.HandleFunc("/api/admin/projects", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"77-27","shortName":"P","name":"Projects","archived":false,"leader":{"login":"jane.doe"}},{"id":"77-25","shortName":"T","name":"Tickets","archived":true}]`)
	})
	mux.HandleFunc("/api/admin/projects/77-27", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `{"id":"77-27","shortName":"P","name":"Projects","description":"Strategic projects","archived":false,"leader":{"login":"jane.doe"}}`)
	})
	mux.HandleFunc("/api/admin/projects/77-27/customFields", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, ytFakeProjectFields)
	})
	mux.HandleFunc("/api/admin/projects/77-27/timeTrackingSettings/workItemTypes", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"63-0","name":"Development"},{"id":"63-3","name":"Analyse"}]`)
	})
	mux.HandleFunc("/api/admin/customFieldSettings/bundles/enum", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"41-1","name":"Priorities"}]`)
	})
	mux.HandleFunc("/hub/api/rest/users", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `{"users":[{"login":"jane.doe"}]}`)
	})
	mux.HandleFunc("/api/commands", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `{"issues":[{"idReadable":"P-1"}]}`)
	})
	// The assist fake parses the way YouTrack does: it names the canonical
	// field for whatever the text sets, also for a bare value ("High" is read
	// as a Sprint priority value here) and for irregular whitespace.
	mux.HandleFunc("/api/commands/assist", func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		var body struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(rec.Body), &body)
		q := strings.ToLower(strings.Join(strings.Fields(body.Query), " "))
		var cmds []string
		switch {
		case strings.Contains(q, "bogus"):
			cmds = append(cmds, `{"description":"Unknown command: bogus","error":true}`)
		case q == "high":
			cmds = append(cmds, `{"description":"Set Sprint priority <span class=\"bold\">High</span>","error":false}`)
		case q == "hoch":
			// A localized instance describes the command in its own language.
			cmds = append(cmds, `{"description":"Setzen Sprint-Priorität <span class=\"bold\">Hoch</span>","error":false}`)
		default:
			for _, part := range strings.Split(q, " and ") {
				switch {
				case strings.HasPrefix(part, "sprint priority"):
					cmds = append(cmds, `{"description":"Set Sprint priority <span class=\"bold\">2026.21</span>","error":false}`)
				case strings.HasPrefix(part, "department"):
					cmds = append(cmds, `{"description":"Set Department <span class=\"bold\">Sales</span>","error":false}`)
				case strings.HasPrefix(part, "state"):
					cmds = append(cmds, `{"description":"Set State <span class=\"bold\">Open</span>","error":false}`)
				default:
					cmds = append(cmds, fmt.Sprintf(`{"description":%q,"error":false}`, "Add new tag <span class=\"bold\">"+part+"</span>"))
				}
			}
		}
		jsonOK(w, `{"commands":[`+strings.Join(cmds, ",")+`],"suggestions":[{"option":"Open","description":"State"}]}`)
	})
	mux.HandleFunc("/api/agiles", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"97-1","name":"Sprint Board","projects":[{"shortName":"I"}],"currentSprint":{"id":"101-5","name":"2026.21"},"sprintsSettings":{"disableSprints":false}}]`)
	})
	mux.HandleFunc("/api/agiles/97-1/sprints/current", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, fmt.Sprintf(`{"id":"101-5","name":"2026.21","start":%d,"finish":1793663999999,"goal":null,"archived":false,"isDefault":false,"unresolvedIssuesCount":7}`, ytFakeMidnight))
	})
	mux.HandleFunc("/api/agiles/97-1/sprints", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, `[{"id":"101-3","name":"2026.19","start":1,"finish":2},{"id":"101-4","name":"2026.20","start":3,"finish":4},{"id":"101-5","name":"2026.21","start":5,"finish":6}]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"Not Found","error_description":"fake has no `+r.URL.Path+`"}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *ytFake) last(method, pathSuffix string) (ytFakeRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		r := f.requests[i]
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			return r, true
		}
	}
	return ytFakeRequest{}, false
}

func (f *ytFake) count(method, pathSuffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			n++
		}
	}
	return n
}

func newTestYouTrack(t *testing.T, f *ytFake, mutate func(*config.Connection)) *YouTrackREST {
	t.Helper()
	conn := config.Connection{
		Name:            "yt",
		Type:            config.TypeYouTrackREST,
		URL:             f.srv.URL,
		Token:           "perm:test",
		ProtectedFields: []string{"P:Sprint priority"},
		SetOnceFields:   []string{"P:Department"},
	}
	if mutate != nil {
		mutate(&conn)
	}
	y, err := NewYouTrackREST(conn, nil)
	if err != nil {
		t.Fatalf("NewYouTrackREST: %v", err)
	}
	y.outputDir = t.TempDir()
	return y
}

func resultJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", resultText(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("result is not a JSON object: %v\n%s", err, resultText(t, res))
	}
	return out
}

func mustError(t *testing.T, res *mcp.CallToolResult, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a tool error containing %q, got success: %s", want, resultText(t, res))
	}
	if got := resultText(t, res); !strings.Contains(got, want) {
		t.Fatalf("error %q does not contain %q", got, want)
	}
}

// --- Tests ---

func TestYouTrackREST_BaseURL(t *testing.T) {
	cases := []struct {
		in, base, root string
		wantErr        string
	}{
		{"https://x.youtrack.cloud", "https://x.youtrack.cloud", "https://x.youtrack.cloud", ""},
		{"https://x.youtrack.cloud/", "https://x.youtrack.cloud", "https://x.youtrack.cloud", ""},
		{"https://x.youtrack.cloud/api/", "https://x.youtrack.cloud", "https://x.youtrack.cloud", ""},
		{"https://host.myjetbrains.com/youtrack", "https://host.myjetbrains.com/youtrack", "https://host.myjetbrains.com", ""},
		{"https://host.myjetbrains.com/youtrack/api", "https://host.myjetbrains.com/youtrack", "https://host.myjetbrains.com", ""},
		{"https://host.myjetbrains.com/mcp", "", "", "MCP endpoint"},
		{"host.myjetbrains.com", "", "", "absolute http(s) URL"},
	}
	for _, c := range cases {
		base, root, err := YouTrackBaseURL(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%q: want error containing %q, got %v", c.in, c.wantErr, err)
			}
			continue
		}
		if err != nil || base != c.base || root != c.root {
			t.Errorf("%q: got (%q, %q, %v), want (%q, %q)", c.in, base, root, err, c.base, c.root)
		}
	}
}

func TestYouTrackREST_Me(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleMe(context.Background(), toolRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	if out["login"] != "jane.doe" || out["name"] != "Jane Doe" || out["time_zone"] != "Europe/Berlin" || out["read_only"] != false {
		t.Errorf("me = %v", out)
	}
	if r, _ := f.last(http.MethodGet, "/api/users/me"); r.Query["fields"] != ytMeFields {
		t.Errorf("me selector = %q", r.Query["fields"])
	}
}

func TestYouTrackREST_GetIssueShapesTheIssue(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)

	res, err := y.handleGetIssue(context.Background(), toolRequest(map[string]any{"issue": "P-1"}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)

	if out["id"] != "P-1" || out["project"] != "P" || out["reporter"] != "jane.doe" {
		t.Errorf("identity fields wrong: %v %v %v", out["id"], out["project"], out["reporter"])
	}
	// 2026-10-15T09:00:00Z in Europe/Berlin (CEST) is 11:00+02:00.
	if out["created"] != "2026-10-15T11:00:00+02:00" {
		t.Errorf("created = %v, want profile-zone ISO time", out["created"])
	}
	fields := out["fields"].(map[string]any)
	if fields["Priority"] != "High" || fields["State"] != "Open" || fields["Assignee"] != "jane.doe" {
		t.Errorf("named values wrong: %v", fields)
	}
	if fields["Due date"] != "2026-10-15" {
		t.Errorf("date-only field = %v, want 2026-10-15 (stored noon UTC)", fields["Due date"])
	}
	if spent := fields["Spent time"].(map[string]any); spent["minutes"] != float64(270) || spent["presentation"] != "4h 30m" {
		t.Errorf("period rendering wrong: %v", spent)
	}
	if fv := fields["Fix versions"].([]any); len(fv) != 2 || fv[0] != "2026.20" {
		t.Errorf("multi value wrong: %v", fv)
	}
	if sp, ok := fields["Sprint priority"].([]any); !ok || len(sp) != 0 {
		t.Errorf("empty multi field should be [], got %v", fields["Sprint priority"])
	}
	if fields["Estimation"] != float64(3) {
		t.Errorf("integer field = %v", fields["Estimation"])
	}
	links := out["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links: empty link groups must be dropped, got %v", links)
	}
	if l := links[0].(map[string]any); l["role"] != "parent for" || l["issues"].([]any)[0] != "I-1" {
		t.Errorf("link rendering wrong: %v", l)
	}
	// Comments are paged to completion: 100 on the first page, 1 on the second.
	comments := out["comments"].([]any)
	if len(comments) != 101 {
		t.Fatalf("comments: want 101 merged over two pages, got %d", len(comments))
	}
	if c := comments[1].(map[string]any); c["visibility"] != "limited to group IT" || c["author"] != "john.roe" {
		t.Errorf("comment rendering wrong: %v", c)
	}
	if c := comments[100].(map[string]any); c["text"] != "comment 101" {
		t.Errorf("last comment = %v", c)
	}
	if n := f.count(http.MethodGet, "/api/issues/P-1/comments"); n != 2 {
		t.Errorf("comments fetched in %d requests, want 2 pages", n)
	}
	if r, _ := f.last(http.MethodGet, "/api/issues/P-1/comments"); r.Query["$skip"] != "100" || r.Query["$top"] != "100" {
		t.Errorf("second page query = %v", r.Query)
	}
	if w := out["watchers"].([]any); len(w) != 1 || w[0] != "john.roe" {
		t.Errorf("watchers wrong: %v", w)
	}
	if out["url"] != f.srv.URL+"/issue/P-1" {
		t.Errorf("url = %v", out["url"])
	}
	if r, _ := f.last(http.MethodGet, "/api/issues/P-1"); r.Query["fields"] != ytIssueFields {
		t.Errorf("issue selector = %q", r.Query["fields"])
	}
}

func TestYouTrackREST_GetComments(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleGetComments(context.Background(), toolRequest(map[string]any{"issue": "P-1"}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	if out["count"] != float64(101) || out["note"] != nil {
		t.Errorf("whole thread: %v %v", out["count"], out["note"])
	}
	res, err = y.handleGetComments(context.Background(), toolRequest(map[string]any{"issue": "P-1", "top": float64(2), "skip": float64(1)}))
	if err != nil {
		t.Fatal(err)
	}
	out = resultJSON(t, res)
	r, _ := f.last(http.MethodGet, "/api/issues/P-1/comments")
	if r.Query["$top"] != "2" || r.Query["$skip"] != "1" {
		t.Errorf("page query = %v", r.Query)
	}
	if comments := out["comments"].([]any); len(comments) == 0 || comments[0].(map[string]any)["text"] != "second" {
		t.Errorf("page content = %v", comments)
	}
}

func TestYouTrackREST_GetIssueNotFoundIsReadable(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleGetIssue(context.Background(), toolRequest(map[string]any{"issue": "P-404"}))
	mustError(t, res, err, "HTTP 404")
	mustError(t, res, err, "Entity with id P-404 not found")
}

func TestYouTrackREST_SearchPagesAndHints(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleSearchIssues(context.Background(), toolRequest(map[string]any{"query": "project: P", "top": float64(2)}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	if out["count"] != float64(2) || out["more"] == nil {
		t.Errorf("full page must carry a paging hint: %v", out)
	}
	issues := out["issues"].([]any)
	first := issues[0].(map[string]any)
	if _, has := first["links"]; has {
		t.Errorf("search results must stay compact (no links), got %v", first)
	}
	if first["description"] != nil {
		t.Errorf("description must be absent by default")
	}
	r, _ := f.last(http.MethodGet, "/api/issues")
	if r.Query["$top"] != "2" || r.Query["$skip"] != "0" || r.Query["query"] != "project: P" || r.Query["fields"] != ytIssueCompactFields {
		t.Errorf("search query = %v", r.Query)
	}
	res, err = y.handleSearchIssues(context.Background(), toolRequest(map[string]any{"query": "project: P", "include_description": true}))
	if err != nil {
		t.Fatal(err)
	}
	out = resultJSON(t, res)
	if out["issues"].([]any)[0].(map[string]any)["description"] != "Long text" {
		t.Error("include_description must return the description")
	}
	if r, _ := f.last(http.MethodGet, "/api/issues"); r.Query["fields"] != ytIssueSearchFields {
		t.Errorf("search selector with description = %q", r.Query["fields"])
	}
}

func TestYouTrackREST_UpdateIssueGuardsAndEncoding(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	ctx := context.Background()

	// Protected field: refused before anything is written.
	res, err := y.handleUpdateIssue(ctx, toolRequest(map[string]any{"issue": "P-1", "fields": map[string]any{"Sprint priority": "2026.21"}}))
	mustError(t, res, err, "protected")
	if _, posted := f.last(http.MethodPost, "/api/issues/P-1"); posted {
		t.Fatal("protected field must not reach the server")
	}

	// Set-once field with a value: refused.
	res, err = y.handleUpdateIssue(ctx, toolRequest(map[string]any{"issue": "P-1", "fields": map[string]any{"department": "Sales"}}))
	mustError(t, res, err, "set-once")

	// Set-once field still empty (P-2): allowed.
	res, err = y.handleUpdateIssue(ctx, toolRequest(map[string]any{"issue": "P-2", "fields": map[string]any{"Department": "Sales"}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("empty set-once field must be writable: %s", resultText(t, res))
	}

	// Encoding: $type from the issue, values per field type, dates at noon UTC.
	res, err = y.handleUpdateIssue(ctx, toolRequest(map[string]any{
		"issue":   "P-1",
		"summary": "New summary",
		"fields": map[string]any{
			"State":        "Closed",
			"Assignee":     "john.roe",
			"Due date":     "2026-11-03",
			"Spent time":   "1h 30m",
			"Fix versions": []any{"2026.22"},
			"Estimation":   float64(5),
			"Deadline":     "2026-11-03 14:30",
			"Priority":     nil,
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("update failed: %s", resultText(t, res))
	}
	r, ok := f.last(http.MethodPost, "/api/issues/P-1")
	if !ok {
		t.Fatal("no update request recorded")
	}
	var body struct {
		Summary      string `json:"summary"`
		CustomFields []struct {
			Name  string          `json:"name"`
			Type  string          `json:"$type"`
			Value json.RawMessage `json:"value"`
		} `json:"customFields"`
	}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("body: %v\n%s", err, r.Body)
	}
	if body.Summary != "New summary" {
		t.Errorf("summary = %q", body.Summary)
	}
	got := map[string]string{}
	types := map[string]string{}
	for _, cf := range body.CustomFields {
		got[cf.Name] = string(cf.Value)
		types[cf.Name] = cf.Type
	}
	want := map[string]string{
		"State":        `{"name":"Closed"}`,
		"Assignee":     `{"login":"john.roe"}`,
		"Due date":     `1793707200000`, // 2026-11-03T12:00:00Z
		"Spent time":   `{"minutes":90,"presentation":"1h 30m"}`,
		"Fix versions": `[{"name":"2026.22"}]`,
		"Estimation":   `5`,
		"Deadline":     `1793712600000`, // 2026-11-03 14:30 Europe/Berlin (CET) = 13:30Z
		"Priority":     `null`,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s encoded as %s, want %s", name, got[name], w)
		}
	}
	if types["State"] != "StateIssueCustomField" || types["Spent time"] != "PeriodIssueCustomField" || types["Due date"] != "DateIssueCustomField" {
		t.Errorf("$types wrong: %v", types)
	}
	if r.Query["fields"] != ytIssueFields {
		t.Errorf("update must request the full issue back, got %q", r.Query["fields"])
	}

	// Clearing a multi-value field sends an empty list, not null.
	res, err = y.handleUpdateIssue(ctx, toolRequest(map[string]any{"issue": "P-1", "fields": map[string]any{"Fix versions": nil}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("clear failed: %s", resultText(t, res))
	}
	if r, _ := f.last(http.MethodPost, "/api/issues/P-1"); !strings.Contains(r.Body, `"value":[]`) {
		t.Errorf("multi-value clear body = %s", r.Body)
	}

	// Unknown field names the project's fields.
	res, err = y.handleUpdateIssue(ctx, toolRequest(map[string]any{"issue": "P-1", "fields": map[string]any{"Nope": "x"}}))
	mustError(t, res, err, `has no field "Nope"`)
	mustError(t, res, err, "Priority")
}

func TestYouTrackREST_CreateIssueEncodesFromProjectSchema(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleCreateIssue(context.Background(), toolRequest(map[string]any{
		"project": "p", "summary": "New", "description": "Body",
		"fields": `{"Priority":"High","Department":"IT","Due date":"2026-10-15"}`, // object sent as a JSON string
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("create failed: %s", resultText(t, res))
	}
	r, ok := f.last(http.MethodPost, "/api/issues")
	if !ok {
		t.Fatal("no create request")
	}
	var body map[string]any
	json.Unmarshal([]byte(r.Body), &body)
	if body["project"].(map[string]any)["id"] != "77-27" || body["summary"] != "New" || body["description"] != "Body" {
		t.Errorf("create body wrong: %s", r.Body)
	}
	cfs := body["customFields"].([]any)
	if len(cfs) != 3 {
		t.Fatalf("want 3 custom fields, got %d", len(cfs))
	}
	seen := map[string]map[string]any{}
	for _, c := range cfs {
		m := c.(map[string]any)
		seen[m["name"].(string)] = m
	}
	if seen["Priority"]["$type"] != "SingleEnumIssueCustomField" || seen["Due date"]["value"] != float64(ytFakeDueDate) {
		t.Errorf("encoding wrong: %v", seen)
	}
	// Set-once fields may be set on creation (the field is empty by definition);
	// protected fields may not.
	res, err = y.handleCreateIssue(context.Background(), toolRequest(map[string]any{"project": "P", "summary": "x", "fields": map[string]any{"Sprint priority": "2026.21"}}))
	mustError(t, res, err, "protected")
}

func TestYouTrackREST_RunCommandGuardsThroughAssist(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	ctx := context.Background()
	run := func(args map[string]any) (*mcp.CallToolResult, error) {
		return y.handleRunCommand(ctx, toolRequest(args))
	}

	// Field named as typed: refused by the text pre-check, before assist.
	res, err := run(map[string]any{"issues": []any{"P-1"}, "command": "sprint priority 2026.21 State Open"})
	mustError(t, res, err, "protected")
	if f.count(http.MethodPost, "/api/commands/assist") != 0 {
		t.Fatal("text pre-check must refuse before asking YouTrack to parse")
	}
	// Irregular whitespace still matches.
	res, err = run(map[string]any{"issues": []any{"P-1"}, "command": "Sprint  priority\t2026.21"})
	mustError(t, res, err, "protected")

	// Bare value: the text names no field, YouTrack's parse does.
	res, err = run(map[string]any{"issues": []any{"P-1"}, "command": "High"})
	mustError(t, res, err, "protected")
	if _, posted := f.last(http.MethodPost, "/api/commands"); posted {
		t.Fatal("guarded command must not be sent")
	}

	// Localized instance: the parse names the field in the instance language.
	res, err = run(map[string]any{"issues": []any{"P-1"}, "command": "Hoch"})
	mustError(t, res, err, "protected")

	// Set-once: P-1 has a Department, P-2 does not.
	res, err = run(map[string]any{"issues": []any{"P-2", "P-1"}, "command": "Department Sales"})
	mustError(t, res, err, "set-once")
	mustError(t, res, err, "P-1")
	res, err = run(map[string]any{"issues": []any{"P-2"}, "command": "Department Sales"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("set-once on an empty field must pass: %s", resultText(t, res))
	}

	// A command YouTrack does not understand is refused, nothing applied.
	before := f.count(http.MethodPost, "/api/commands")
	res, err = run(map[string]any{"issues": []any{"P-1"}, "command": "bogus"})
	mustError(t, res, err, "does not understand")
	if f.count(http.MethodPost, "/api/commands") != before {
		t.Fatal("unparseable command must not be applied")
	}

	// dry_run: parse plus the guard verdict, nothing applied.
	res, err = run(map[string]any{"issues": []any{"P-1"}, "command": "High", "dry_run": true})
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	if out["valid"] != true || out["dry_run"] != true || out["would_be_refused"] == nil {
		t.Errorf("dry run result: %v", out)
	}
	if parsed := out["parsed"].([]any); parsed[0].(map[string]any)["description"] != "Set Sprint priority High" {
		t.Errorf("parsed description must be plain text: %v", parsed)
	}
	if f.count(http.MethodPost, "/api/commands") != before {
		t.Fatal("dry run must not apply")
	}

	// A clean command is applied with comment and silent, and the issues re-read.
	res, err = run(map[string]any{"issues": []any{"P-1"}, "command": "State Open", "comment": "done", "silent": true})
	if err != nil {
		t.Fatal(err)
	}
	out = resultJSON(t, res)
	r, ok := f.last(http.MethodPost, "/api/commands")
	if !ok {
		t.Fatal("command not sent")
	}
	if !strings.Contains(r.Body, `"query":"State Open"`) || !strings.Contains(r.Body, `"comment":"done"`) || !strings.Contains(r.Body, `"silent":true`) || !strings.Contains(r.Body, `"idReadable":"P-1"`) {
		t.Errorf("command body = %s", r.Body)
	}
	if issues := out["issues"].([]any); len(issues) != 1 {
		t.Errorf("command result must re-read the issues, got %v", out)
	}
	if r, _ := f.last(http.MethodPost, "/api/commands/assist"); !strings.Contains(r.Body, `"caret":10`) {
		t.Errorf("assist caret must be the rune count, body = %s", r.Body)
	}
}

func TestYouTrackREST_AttachStreamsAllFiles(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	dir := t.TempDir()
	p1 := filepath.Join(dir, "one.txt")
	p2 := filepath.Join(dir, "two.txt")
	os.WriteFile(p1, []byte("hello"), 0o644)
	os.WriteFile(p2, []byte("world"), 0o644)

	res, err := y.handleAttach(context.Background(), toolRequest(map[string]any{"issue": "P-1", "file_paths": []any{p1, p2}}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	r, ok := f.last(http.MethodPost, "/api/issues/P-1/attachments")
	if !ok {
		t.Fatal("no upload")
	}
	if len(r.Files) != 2 || r.Files[0] != "files[0]=one.txt" || r.Files[1] != "files[1]=two.txt" {
		t.Errorf("multipart parts = %v", r.Files)
	}
	atts := out["attachments"].([]any)
	uploaded := 0
	for _, a := range atts {
		if a.(map[string]any)["uploaded"] == true {
			uploaded++
		}
	}
	if uploaded != 2 || len(atts) != 5 {
		t.Errorf("attachment list after upload: %v", atts)
	}
	if out["warning"] != nil {
		t.Errorf("unexpected warning: %v", out["warning"])
	}

	res, err = y.handleAttach(context.Background(), toolRequest(map[string]any{"issue": "P-1", "file_paths": []any{"relative.txt"}}))
	mustError(t, res, err, "absolute path")
	res, err = y.handleAttach(context.Background(), toolRequest(map[string]any{"issue": "P-1", "file_paths": []any{p1, filepath.Join(dir, "..", filepath.Base(dir), "one.txt")}}))
	mustError(t, res, err, "unique")
}

func TestYouTrackREST_WatchSendsIsStarred(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleWatch(context.Background(), toolRequest(map[string]any{"issue": "P-1", "users": []any{"John.Roe"}}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	r, ok := f.last(http.MethodPost, "/api/issues/P-1/watchers/issueWatchers")
	if !ok {
		t.Fatal("no watcher request")
	}
	if !strings.Contains(r.Body, `"isStarred":true`) || !strings.Contains(r.Body, `"id":"26-5"`) {
		t.Errorf("watcher body = %s", r.Body)
	}
	if w := out["watchers"].([]any); len(w) != 1 || w[0] != "john.roe" {
		t.Errorf("watchers = %v", w)
	}
	res, err = y.handleWatch(context.Background(), toolRequest(map[string]any{"issue": "P-1", "users": []any{"nobody"}}))
	mustError(t, res, err, `user "nobody" not found`)
}

func TestYouTrackREST_LinkIssuesResolvesRelation(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleLinkIssues(context.Background(), toolRequest(map[string]any{"issue": "P-1", "relation": "Parent For", "target": "P-2"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("link failed: %s", resultText(t, res))
	}
	r, ok := f.last(http.MethodPost, "/api/issues/P-1/links/57-3s/issues")
	if !ok {
		t.Fatal("link not posted to the outward subtask link")
	}
	if !strings.Contains(r.Body, `"id":"2-200"`) {
		t.Errorf("link body must carry the target's internal id (2-200), got %s", r.Body)
	}
	res, err = y.handleLinkIssues(context.Background(), toolRequest(map[string]any{"issue": "P-1", "relation": "blocks", "target": "P-2"}))
	mustError(t, res, err, "parent for")
	mustError(t, res, err, "relates to")
}

func TestYouTrackREST_LogWork(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleLogWork(context.Background(), toolRequest(map[string]any{"issue": "P-1", "duration": "1h 30m", "date": "2026-10-15", "text": "review", "type": "development"}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	r, _ := f.last(http.MethodPost, "/api/issues/P-1/timeTracking/workItems")
	if !strings.Contains(r.Body, `"minutes":90`) || !strings.Contains(r.Body, `"date":1792022400000`) || !strings.Contains(r.Body, `"id":"63-0"`) {
		t.Errorf("work item body = %s", r.Body)
	}
	if wi := out["work_item"].(map[string]any); wi["date"] != "2026-10-15" || wi["duration"] != "1h 30m" {
		t.Errorf("work item rendering = %v", wi)
	}
	res, err = y.handleLogWork(context.Background(), toolRequest(map[string]any{"issue": "P-1", "duration": "2d"}))
	mustError(t, res, err, "days and weeks")

	// Work item dates are stored at 00:00 UTC; a profile zone west of UTC
	// must still read the same day.
	ny, _ := time.LoadLocation("America/New_York")
	var w ytRawWorkItem
	w.Date = ytFakeMidnight
	if got := renderWorkItem(w, ny).Date; got != "2026-10-15" {
		t.Errorf("work item date in New York = %s, want 2026-10-15", got)
	}
}

func TestYouTrackREST_AddCommentWithVisibility(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleAddComment(context.Background(), toolRequest(map[string]any{"issue": "P-1", "text": "hi", "visibility_groups": []any{"it"}, "visibility_users": []any{"john.roe"}}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	r, _ := f.last(http.MethodPost, "/api/issues/P-1/comments")
	if !strings.Contains(r.Body, `"$type":"LimitedVisibility"`) || !strings.Contains(r.Body, `"permittedGroups":[{"id":"25-1"}]`) || !strings.Contains(r.Body, `"permittedUsers":[{"id":"26-5"}]`) {
		t.Errorf("comment body = %s", r.Body)
	}
	if c := out["comment"].(map[string]any); c["visibility"] != "limited to group IT" {
		t.Errorf("comment rendering = %v", c)
	}
}

func TestYouTrackREST_UpdateComment(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	run := func(args map[string]any) (*mcp.CallToolResult, error) {
		return y.handleUpdateComment(context.Background(), toolRequest(args))
	}

	// Text only: the body carries the text and nothing else; the result is
	// the comment as the API returns it afterwards.
	res, err := run(map[string]any{"issue": "P-1", "comment": "4-9", "text": "edited"})
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	r, ok := f.last(http.MethodPost, "/api/issues/P-1/comments/4-9")
	if !ok || r.Body != `{"text":"edited"}` {
		t.Errorf("text-only body = %q", r.Body)
	}
	if c := out["comment"].(map[string]any); c["id"] != "4-9" || c["text"] != "edited" || c["updated"] == nil || c["deleted"] != nil {
		t.Errorf("comment rendering = %v", c)
	}

	// Visibility only: the text is left alone, names and logins resolve to ids.
	res, _ = run(map[string]any{"issue": "P-1", "comment": "4-9", "visibility_groups": []any{"it"}, "visibility_users": []any{"john.roe"}})
	out = resultJSON(t, res)
	r, _ = f.last(http.MethodPost, "/api/issues/P-1/comments/4-9")
	if strings.Contains(r.Body, `"text"`) || !strings.Contains(r.Body, `"$type":"LimitedVisibility"`) || !strings.Contains(r.Body, `"permittedGroups":[{"id":"25-1"}]`) || !strings.Contains(r.Body, `"permittedUsers":[{"id":"26-5"}]`) {
		t.Errorf("visibility-only body = %s", r.Body)
	}
	if c := out["comment"].(map[string]any); c["text"] != "edited" || c["visibility"] != "limited to group IT, user john.roe" {
		t.Errorf("comment rendering = %v", c)
	}

	// Empty lists lift the restriction; an explicit null is not a change.
	res, _ = run(map[string]any{"issue": "P-1", "comment": "4-9", "visibility_groups": []any{}, "visibility_users": nil})
	out = resultJSON(t, res)
	r, _ = f.last(http.MethodPost, "/api/issues/P-1/comments/4-9")
	if r.Body != `{"visibility":{"$type":"UnlimitedVisibility"}}` {
		t.Errorf("unlimited body = %s", r.Body)
	}
	if c := out["comment"].(map[string]any); c["visibility"] != nil {
		t.Errorf("visibility after lifting = %v", c["visibility"])
	}

	// Text and visibility together.
	res, _ = run(map[string]any{"issue": "P-1", "comment": "4-9", "text": "both", "visibility_users": []any{"john.roe"}})
	resultJSON(t, res)
	r, _ = f.last(http.MethodPost, "/api/issues/P-1/comments/4-9")
	if !strings.Contains(r.Body, `"text":"both"`) || !strings.Contains(r.Body, `"permittedUsers":[{"id":"26-5"}]`) || strings.Contains(r.Body, "permittedGroups") {
		t.Errorf("combined body = %s", r.Body)
	}

	// Refused before anything is sent.
	sent := f.count(http.MethodPost, "/api/issues/P-1/comments/4-9")
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"issue": "P-1", "text": "x"}, "issue and comment are required"},
		{map[string]any{"issue": "P-1", "comment": "4-9"}, "nothing to update"},
		{map[string]any{"issue": "P-1", "comment": "4-9", "visibility_groups": nil}, "nothing to update"},
		{map[string]any{"issue": "P-1", "comment": "4-9", "text": "   "}, "text must not be empty"},
		{map[string]any{"issue": "P-1", "comment": "4-9", "visibility_groups": []any{"nope"}}, `group "nope" not found`},
	} {
		res, err := run(c.args)
		mustError(t, res, err, c.want)
	}
	if n := f.count(http.MethodPost, "/api/issues/P-1/comments/4-9"); n != sent {
		t.Errorf("a refused update reached the API (%d calls, had %d)", n, sent)
	}

	// YouTrack's own refusal (someone else's comment) is passed through.
	res, err = run(map[string]any{"issue": "P-1", "comment": "4-7", "text": "x"})
	mustError(t, res, err, "HTTP 403")
}

func TestYouTrackREST_DeleteComment(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	run := func(args map[string]any) (*mcp.CallToolResult, error) {
		return y.handleDeleteComment(context.Background(), toolRequest(args))
	}

	// The DELETE goes out without a body; the re-read shows the comment the
	// way YouTrack keeps it: deleted, text still readable.
	res, err := run(map[string]any{"issue": "P-1", "comment": "4-9"})
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	if r, ok := f.last(http.MethodDelete, "/api/issues/P-1/comments/4-9"); !ok || r.Body != "" || r.Query["fields"] != "" {
		t.Errorf("DELETE request = %+v", r)
	}
	if r, ok := f.last(http.MethodGet, "/api/issues/P-1/comments/4-9"); !ok || !strings.Contains(r.Query["fields"], "deleted") {
		t.Errorf("deleted comment was not re-read with its deleted flag: %+v", r)
	}
	if c := out["comment"].(map[string]any); c["id"] != "4-9" || c["deleted"] != true || c["text"] != "posted" {
		t.Errorf("comment rendering = %v", c)
	}

	// A comment the API no longer returns is reported as gone.
	res, _ = run(map[string]any{"issue": "P-1", "comment": "4-8"})
	out = resultJSON(t, res)
	if c := out["comment"].(map[string]any); c["id"] != "4-8" || c["deleted"] != true || out["note"] == nil {
		t.Errorf("gone comment = %v", out)
	}

	// Missing parameters send nothing; YouTrack's refusal is passed through.
	res, err = run(map[string]any{"issue": "P-1"})
	mustError(t, res, err, "issue and comment are required")
	res, err = run(map[string]any{"issue": "P-1", "comment": "4-7"})
	mustError(t, res, err, "HTTP 403")
	if n := f.count(http.MethodDelete, "/api/issues/P-1/comments/4-7"); n != 1 {
		t.Errorf("refused delete sent %d times", n)
	}
}

func TestYouTrackREST_GetAttachmentContentPaths(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	ctx := context.Background()

	// Image: inline image content.
	res, err := y.handleGetAttachment(ctx, toolRequest(map[string]any{"issue": "P-1", "attachment": "shot.png"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get_attachment failed: %s", resultText(t, res))
	}
	var image *mcp.ImageContent
	for _, c := range res.Content {
		if ic, ok := c.(mcp.ImageContent); ok {
			image = &ic
		}
	}
	if image == nil || image.MIMEType != "image/png" || image.Data == "" {
		t.Fatalf("want image content, got %+v", res.Content)
	}
	if r, ok := f.last(http.MethodGet, "/api/files/92-1"); !ok || r.Query["sign"] != "abc" {
		t.Errorf("download must follow the signed host-relative url, got %v", r)
	}

	// Text: inlined.
	res, err = y.handleGetAttachment(ctx, toolRequest(map[string]any{"issue": "P-1", "attachment": "notes.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(resultText(t, res), "hello notes") {
		t.Errorf("text attachment must be inlined: %s", resultText(t, res))
	}

	// Binary without output_file: saved under the output directory.
	res, err = y.handleGetAttachment(ctx, toolRequest(map[string]any{"issue": "P-1", "attachment": "92-4"}))
	if err != nil {
		t.Fatal(err)
	}
	text := resultText(t, res)
	if res.IsError || !strings.Contains(text, "Saved to: "+y.outputDir) {
		t.Fatalf("binary attachment must be saved to the output directory: %s", text)
	}
	saved, _ := os.ReadDir(y.outputDir)
	if len(saved) != 1 || !strings.Contains(saved[0].Name(), "blob.bin") {
		t.Errorf("saved files = %v", saved)
	}

	// output_file wins over inlining.
	dest := filepath.Join(t.TempDir(), "shot.png")
	res, err = y.handleGetAttachment(ctx, toolRequest(map[string]any{"issue": "P-1", "attachment": "92-1", "output_file": dest}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("save failed: %s", resultText(t, res))
	}
	if b, err := os.ReadFile(dest); err != nil || len(b) != len(ytFakePNG) {
		t.Errorf("saved file wrong: %v, %d bytes", err, len(b))
	}
	res, err = y.handleGetAttachment(ctx, toolRequest(map[string]any{"issue": "P-1", "attachment": "missing.png"}))
	mustError(t, res, err, "available: shot.png")
}

func TestYouTrackREST_ActivitiesAndSprints(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	ctx := context.Background()

	res, err := y.handleGetActivities(ctx, toolRequest(map[string]any{"issue": "P-1", "start": "2026-10-01"}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	acts := out["activities"].([]any)
	if len(acts) != 6 {
		t.Fatalf("activities = %v", acts)
	}
	a0 := acts[0].(map[string]any)
	if a0["type"] != "field" || a0["field"] != "State" || a0["added"].([]any)[0] != "In Progress" || a0["removed"].([]any)[0] != "Open" {
		t.Errorf("field activity = %v", a0)
	}
	// A comment activity targets the comment; the issue comes from target.issue.
	if a1 := acts[1].(map[string]any); a1["type"] != "comment" || a1["added"].([]any)[0] != "second" || a1["issue"] != "P-1" {
		t.Errorf("comment activity = %v", a1)
	}
	// Date field changes render as dates, resolution as a timestamp, numbers as written.
	if a2 := acts[2].(map[string]any); a2["type"] != "field" || a2["added"].([]any)[0] != "2026-10-15" || a2["removed"].([]any)[0] != "2026-10-14" {
		t.Errorf("date activity = %v", a2)
	}
	if a3 := acts[3].(map[string]any); a3["type"] != "resolved" || a3["added"].([]any)[0] != "2026-10-15T14:00:00+02:00" || a3["removed"] != nil {
		t.Errorf("resolved activity = %v", a3)
	}
	if a4 := acts[4].(map[string]any); a4["added"].([]any)[0] != "1500000" || a4["removed"].([]any)[0] != "3" {
		t.Errorf("integer activity = %v", a4)
	}
	if a5 := acts[5].(map[string]any); a5["added"].([]any)[0] != "27h 32m" || a5["removed"].([]any)[0] != "26h 2m" {
		t.Errorf("period activity = %v", a5)
	}
	r, _ := f.last(http.MethodGet, "/api/issues/P-1/activities")
	if r.Query["categories"] != ytDefaultActivityCategories || r.Query["start"] != "1790805600000" { // 2026-10-01 00:00 Europe/Berlin (CEST) = 2026-09-30T22:00Z
		t.Errorf("activity query = %v", r.Query)
	}
	if r.Query["fields"] != ytActivityFields {
		t.Errorf("activity selector = %q", r.Query["fields"])
	}

	// Query mode goes to /activities with issueQuery and author.
	res, err = y.handleGetActivities(ctx, toolRequest(map[string]any{"query": "project: P", "author": "jane.doe", "categories": "CommentsCategory", "newest_first": true}))
	if err != nil {
		t.Fatal(err)
	}
	out = resultJSON(t, res)
	r, ok := f.last(http.MethodGet, "/api/activities")
	if !ok || r.Query["issueQuery"] != "project: P" || r.Query["author"] != "jane.doe" || r.Query["categories"] != "CommentsCategory" || r.Query["reverse"] != "true" {
		t.Errorf("query-mode request = %v", r.Query)
	}
	if out["query"] != "project: P" {
		t.Errorf("query-mode result = %v", out)
	}

	// Sprint bounds are rendered in UTC, the zone they are stored in.
	res, err = y.handleGetCurrentSprint(ctx, toolRequest(map[string]any{"board": "sprint board"}))
	if err != nil {
		t.Fatal(err)
	}
	out = resultJSON(t, res)
	if out["name"] != "2026.21" || out["start"] != "2026-10-15T00:00:00Z" || out["finish"] != "2026-11-02T23:59:59Z" {
		t.Errorf("sprint = %v", out)
	}
	res, err = y.handleListSprints(ctx, toolRequest(map[string]any{"board": "97-1", "top": float64(2)}))
	if err != nil {
		t.Fatal(err)
	}
	var sprints []map[string]any
	json.Unmarshal([]byte(resultText(t, res)), &sprints)
	if len(sprints) != 2 || sprints[0]["name"] != "2026.21" || sprints[1]["name"] != "2026.20" {
		t.Errorf("sprints must be newest first and trimmed: %v", sprints)
	}
}

func TestYouTrackREST_ReadOnlyHidesWritesAndAdmin(t *testing.T) {
	f := newYTFake(t)
	rw := newTestYouTrack(t, f, nil)
	ro := newTestYouTrack(t, f, func(c *config.Connection) { c.ReadOnly = true })
	names := func(y *YouTrackREST) map[string]bool {
		m := map[string]bool{}
		for _, td := range y.Tools() {
			m[td.Tool.Name] = true
		}
		return m
	}
	rwNames, roNames := names(rw), names(ro)
	for _, w := range []string{"create_issue", "update_issue", "add_comment", "update_comment", "delete_comment", "attach", "watch", "link_issues", "log_work", "run_command", "admin_get", "admin_request"} {
		if !rwNames[w] {
			t.Errorf("read-write connection lacks %s", w)
		}
		if roNames[w] {
			t.Errorf("read-only connection exposes %s", w)
		}
	}
	for _, r := range []string{"me", "search_issues", "get_issue", "get_comments", "get_activities", "get_attachment", "list_projects", "get_project", "find_users", "list_link_types", "list_boards", "get_current_sprint", "list_sprints"} {
		if !roNames[r] {
			t.Errorf("read-only connection lacks %s", r)
		}
	}
	// Guards are announced in the write tools' descriptions.
	for _, td := range rw.Tools() {
		if td.Tool.Name == "update_issue" && !strings.Contains(td.Tool.Description, `"Sprint priority" in P`) {
			t.Errorf("update_issue description does not name the guard: %s", td.Tool.Description)
		}
	}
}

func TestYouTrackREST_DescribeGuards(t *testing.T) {
	p := []config.FieldGuard{{Project: "P", Field: "Sprint priority"}}
	s := []config.FieldGuard{{Field: "Department"}}
	if got := describeGuards(nil, nil); got != "" {
		t.Errorf("no guards: %q", got)
	}
	if got := describeGuards(nil, s); got != ` This connection writes the set-once fields "Department" (every project) only while they are still empty.` {
		t.Errorf("set-once only: %q", got)
	}
	if got := describeGuards(p, nil); got != ` This connection refuses writes to the protected fields "Sprint priority" in P.` {
		t.Errorf("protected only: %q", got)
	}
	if got := describeGuards(p, s); !strings.Contains(got, `"Sprint priority" in P and writes the set-once fields`) || strings.Contains(got, " to ;") {
		t.Errorf("both: %q", got)
	}
}

func TestYouTrackREST_AdminPassThrough(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	ctx := context.Background()

	res, err := y.handleAdminGet(ctx, toolRequest(map[string]any{"path": "/api/admin/customFieldSettings/bundles/enum?fields=id,name"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(resultText(t, res), "Priorities") {
		t.Errorf("admin_get: %s", resultText(t, res))
	}
	if r, _ := f.last(http.MethodGet, "/api/admin/customFieldSettings/bundles/enum"); r.Query["fields"] != "id,name" {
		t.Errorf("query string must be forwarded, got %v", r.Query)
	}
	res, err = y.handleAdminGet(ctx, toolRequest(map[string]any{"path": "/hub/api/rest/users?fields=login"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(resultText(t, res), "jane.doe") {
		t.Errorf("hub get: %s", resultText(t, res))
	}
	res, err = y.handleAdminGet(ctx, toolRequest(map[string]any{"path": "/api/issues/P-1"}))
	mustError(t, res, err, "must start with /api/admin/")

	// Dot segments, encoded separators and the issue subtree never reach the server.
	before := len(f.requests)
	for path, want := range map[string]string{
		"/api/admin/../issues/P-1":                "not normalized",
		"/api/admin/./projects":                   "not normalized",
		"/api/admin//projects":                    "not normalized",
		"/hub/api/rest/../../api/issues/P-1":      "not normalized",
		"/api/admin/%2e%2e/issues/P-1":            "encoded",
		"/api/admin/projects%2F..%2Fissues":       "encoded",
		"/api/admin/projects/77-27/issues":        "addresses issues",
		"/api/admin/projects/77-27/issues/2-100":  "addresses issues",
		"/admin/projects/77-27/issues/2-100":      "addresses issues",
		"https://evil.example/api/admin/projects": "not a URL",
	} {
		res, err = y.handleAdminRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": path, "body": `{"customFields":[]}`}))
		mustError(t, res, err, want)
		res, err = y.handleAdminGet(ctx, toolRequest(map[string]any{"path": path}))
		mustError(t, res, err, want)
	}
	if len(f.requests) != before {
		t.Fatalf("refused admin paths must not produce requests, got %d", len(f.requests)-before)
	}
	// Allowed neighbours of the refused subtree still work.
	for _, path := range []string{"/api/admin/projects/77-27/customFields", "/admin/projects/77-27", "/api/admin/projects/", "/api/admin/projects/77-27/customFields?$top=5"} {
		u, err := y.adminURL(path)
		if err != nil {
			t.Errorf("%s refused: %v", path, err)
		}
		if !strings.HasPrefix(u, f.srv.URL+"/api/admin/projects") {
			t.Errorf("%s resolved to %s", path, u)
		}
	}

	res, err = y.handleAdminRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/api/admin/customFieldSettings/bundles/enum", "body": "{not json"}))
	mustError(t, res, err, "valid JSON")
	res, err = y.handleAdminRequest(ctx, toolRequest(map[string]any{"method": "GET", "path": "/api/admin/x"}))
	mustError(t, res, err, "POST, PUT, PATCH or DELETE")
}

func TestYouTrackREST_ProjectFieldsRendering(t *testing.T) {
	f := newYTFake(t)
	y := newTestYouTrack(t, f, nil)
	res, err := y.handleGetProject(context.Background(), toolRequest(map[string]any{"project": "P"}))
	if err != nil {
		t.Fatal(err)
	}
	out := resultJSON(t, res)
	fields := out["fields"].([]any)
	var prio, state map[string]any
	for _, fld := range fields {
		m := fld.(map[string]any)
		switch m["name"] {
		case "Priority":
			prio = m
		case "State":
			state = m
		}
	}
	if prio["required"] != true || len(prio["values"].([]any)) != 2 || prio["archived_values"].([]any)[0] != "Old" {
		t.Errorf("priority field = %v", prio)
	}
	if state["resolved_values"].([]any)[0] != "Closed" || state["values"].([]any)[0] != "Open" {
		t.Errorf("state field = %v", state)
	}
}

func TestYouTrackREST_Helpers(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	if ms, _ := ytParseDateOnly("2026-10-15"); ms != ytFakeDueDate {
		t.Errorf("date-only must be noon UTC, got %d", ms)
	}
	if ms, _ := ytParseWorkDate("2026-10-15"); ms != ytFakeMidnight {
		t.Errorf("work date must be midnight UTC, got %d", ms)
	}
	if ytDate(ytFakeDueDate, berlin) != "2026-10-15" || ytDate(ytFakeDueDate, time.UTC) != "2026-10-15" {
		t.Error("noon UTC must render as the same day in Berlin and UTC")
	}
	if ms, _ := ytParseDateTime("2026-11-03 14:30", berlin); ms != 1793712600000 {
		t.Errorf("wall clock in profile zone: %d", ms)
	}
	if _, err := ytParseDateOnly("15.10.2026"); err == nil {
		t.Error("dotted date must be refused")
	}
	for in, want := range map[any]int{"90": 90, "1h 30m": 90, "1h30m": 90, "2h": 120, "45m": 45, float64(15): 15} {
		if got, err := ytParsePeriod(in); err != nil || got != want {
			t.Errorf("period %v = %d (%v), want %d", in, got, err, want)
		}
	}
	for _, bad := range []any{"1d", "1w 2d", "abc", "0", float64(-1)} {
		if _, err := ytParsePeriod(bad); err == nil {
			t.Errorf("period %v must be refused", bad)
		}
	}
	if ytPeriodPresentation(90) != "1h 30m" || ytPeriodPresentation(120) != "2h" || ytPeriodPresentation(45) != "45m" {
		t.Error("period presentation is off")
	}
	if !ytCommandMentions("Sprint Priority High", "sprint priority") || ytCommandMentions("Department Sales", "Depart") || !ytCommandMentions("add tag x Department IT", "Department") {
		t.Error("command mention matching is off")
	}
	if !ytCommandMentions("Sprint  priority\tHigh", "Sprint priority") || !ytCommandMentions("Set Sprint priority High", "sprint  priority") {
		t.Error("command mention matching must collapse whitespace on both sides")
	}
	if got := ytStripTags(`Set Priority <span class="bold">High</span> &amp; more`); got != "Set Priority High & more" {
		t.Errorf("strip tags = %q", got)
	}
	if y, err := NewYouTrackREST(config.Connection{URL: "https://x.youtrack.cloud/mcp", Token: "t"}, nil); err == nil || y != nil {
		t.Error("MCP url must be refused at construction")
	}
	if _, err := NewYouTrackREST(config.Connection{URL: "https://x.youtrack.cloud", Token: "t", ProtectedFields: []string{"P:"}}, nil); err == nil || !strings.Contains(err.Error(), "protected_fields") {
		t.Errorf("guard entry without field must be refused at construction, got %v", err)
	}
}
