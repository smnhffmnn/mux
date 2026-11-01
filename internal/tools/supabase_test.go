package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const sbTestRef = "abcdefghijklmnopqrst"

type sbFakeRequest struct {
	Method string
	Path   string
	Query  string
	Accept string
	Body   []byte
	Header http.Header
}

// sbFake is a Management API stand-in for one project with one deployed
// function, two secrets and a log stream.
type sbFake struct {
	srv        *httptest.Server
	mu         sync.Mutex
	requests   []sbFakeRequest
	version    int
	rateOnce   bool // answer the first deploy with 429, then succeed
	migrations []map[string]string
	failApply  string // name of a migration whose apply answers 500
}

func newSBFake(t *testing.T) *sbFake {
	t.Helper()
	f := &sbFake{version: 7, migrations: []map[string]string{
		{"version": "20260710085001", "name": "20260710085001_929350e6-7b35-4f7e-818f-91f6a6c404aa"}, // early API form: full stem
		{"version": "20260805175905", "name": "8447fd06-6bca-464e-88de-4bad72f7834c"},                // later API form: name part only, version shifted
		{"version": "20260901120000", "name": "add_widgets"},                                         // CLI form: version = file version
	}}
	record := func(r *http.Request) []byte {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, sbFakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Accept: r.Header.Get("Accept"), Body: body, Header: r.Header.Clone()})
		f.mu.Unlock()
		return body
	}
	jsonOK := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
	fn := func() string {
		return fmt.Sprintf(`{"id":"fid-1","slug":"hook","name":"hook","status":"ACTIVE","version":%d,"created_at":1761900000000,"updated_at":1761930000000,"verify_jwt":false,"import_map":false,"entrypoint_path":"supabase/functions/hook/index.ts","import_map_path":"","ezbr_sha256":"abc"}`, f.version)
	}
	mux := http.NewServeMux()
	base := "/v1/projects/" + sbTestRef
	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Header.Get("Authorization") != "Bearer sbp_test" {
			jsonOK(w, 401, `{"message":"Unauthorized"}`)
			return
		}
		jsonOK(w, 200, `{"id":"`+sbTestRef+`","ref":"`+sbTestRef+`","organization_id":"o","organization_slug":"org-x","name":"demo-project","region":"eu-central-1","created_at":"2026-01-01T00:00:00Z","status":"ACTIVE_HEALTHY","database":{"host":"db.`+sbTestRef+`.supabase.co","version":"17.4"}}`)
	})
	mux.HandleFunc(base+"/functions", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 200, `[`+fn()+`,{"id":"fid-2","slug":"other","name":"Other","status":"THROTTLED","version":1,"created_at":1761900000,"updated_at":1761900000}]`)
	})
	mux.HandleFunc(base+"/functions/hook", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Method == http.MethodDelete {
			jsonOK(w, 200, ``)
			return
		}
		jsonOK(w, 200, fn())
	})
	mux.HandleFunc(base+"/functions/hook/body", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if !strings.HasPrefix(r.Header.Get("Accept"), "multipart/form-data") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("ESZIP-bytes"))
			return
		}
		mw := multipart.NewWriter(w)
		w.Header().Set("Content-Type", mw.FormDataContentType())
		pw, _ := mw.CreateFormField("metadata")
		io.WriteString(pw, `{"entrypoint_path":"supabase/functions/hook/index.ts"}`)
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="file"; filename="functions/hook/index.ts"`}
		h["Supabase-Path"] = []string{"functions/hook/index.ts"}
		pw, _ = mw.CreatePart(h)
		io.WriteString(pw, "import '../_shared/a.ts';\n")
		h2 := make(map[string][]string)
		h2["Content-Disposition"] = []string{`form-data; name="file"; filename="functions/_shared/a.ts"`}
		pw, _ = mw.CreatePart(h2)
		io.WriteString(pw, "export const a = 1;\n")
		mw.Close()
	})
	mux.HandleFunc(base+"/functions/bundled", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 200, `{"id":"fid-3","slug":"bundled","name":"bundled","status":"ACTIVE","version":2,"created_at":1,"updated_at":2}`)
	})
	mux.HandleFunc(base+"/functions/bundled/body", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("ESZIP-bytes"))
	})
	mux.HandleFunc(base+"/functions/jsonbody", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 200, `{"id":"fid-4","slug":"jsonbody","name":"jsonbody","status":"ACTIVE","version":1,"created_at":1,"updated_at":2}`)
	})
	mux.HandleFunc(base+"/functions/jsonbody/body", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 200, `{"unexpected":true}`)
	})
	mux.HandleFunc(base+"/functions/missing", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 404, `{"message":"Function not found","statusCode":404}`)
	})
	mux.HandleFunc(base+"/functions/deploy", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.mu.Lock()
		rate := f.rateOnce
		f.rateOnce = false
		f.mu.Unlock()
		if rate {
			w.Header().Set("Retry-After", "1")
			jsonOK(w, 429, `{"message":"Rate limit exceeded"}`)
			return
		}
		f.mu.Lock()
		f.version++
		f.mu.Unlock()
		jsonOK(w, 201, fn())
	})
	mux.HandleFunc(base+"/secrets", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		switch r.Method {
		case http.MethodGet:
			jsonOK(w, 200, `[{"name":"OPENAI_API_KEY","value":"sk-secret","updated_at":"2026-10-30T10:00:00Z"},{"name":"MAIL_API_KEY","value":"tw-secret"}]`)
		case http.MethodPost:
			jsonOK(w, 201, ``)
		case http.MethodDelete:
			jsonOK(w, 200, ``)
		}
	})
	mux.HandleFunc(base+"/database/query", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 201, `[{"n":1},{"n":2}]`)
	})
	mux.HandleFunc(base+"/database/query/read-only", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 201, `[{"ro":true}]`)
	})
	mux.HandleFunc(base+"/database/migrations", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			out, _ := json.Marshal(f.migrations)
			jsonOK(w, 200, string(out))
		case http.MethodPost:
			var m struct {
				Query string `json:"query"`
				Name  string `json:"name"`
			}
			json.Unmarshal(body, &m)
			if m.Name == f.failApply {
				jsonOK(w, 500, `{"message":"failed to run sql query: syntax error at or near \"BROKEN\""}`)
				return
			}
			f.migrations = append(f.migrations, map[string]string{"version": fmt.Sprintf("2026110100%04d", len(f.migrations)), "name": m.Name})
			jsonOK(w, 201, ``)
		}
	})
	mux.HandleFunc(base+"/analytics/endpoints/logs", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if strings.Contains(r.URL.Query().Get("sql"), "BROKEN") {
			jsonOK(w, 200, `{"result":[],"error":"Syntax error near BROKEN"}`)
			return
		}
		jsonOK(w, 200, `{"result":[{"timestamp":1761934200123456,"event_message":"hello","function_id":"fid-1"},{"timestamp":"2026-11-01T13:04:01.230000","event_message":"naive"}]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		jsonOK(w, 404, `{"message":"fake has no `+r.URL.Path+`","statusCode":404}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sbFake) last(method, suffix string) (sbFakeRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.requests) - 1; i >= 0; i-- {
		r := f.requests[i]
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			return r, true
		}
	}
	return sbFakeRequest{}, false
}

func (f *sbFake) count(method, suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			n++
		}
	}
	return n
}

func newTestSupabase(t *testing.T, f *sbFake, mutate func(*config.Connection)) *Supabase {
	t.Helper()
	conn := config.Connection{Name: "sb", Type: config.TypeSupabase, URL: f.srv.URL, Token: "sbp_test", ProjectRef: sbTestRef}
	if mutate != nil {
		mutate(&conn)
	}
	s, err := NewSupabase(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC) }
	s.sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

func sbCall(t *testing.T, s *Supabase, h func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := h(context.Background(), toolRequest(args))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	text := resultText(t, res)
	if res.IsError {
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out, ""
}

func TestSupabase_NewAndTools(t *testing.T) {
	f := newSBFake(t)
	if _, err := NewSupabase(config.Connection{Type: config.TypeSupabase, ProjectRef: sbTestRef}, nil); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("missing token: %v", err)
	}
	if _, err := NewSupabase(config.Connection{Type: config.TypeSupabase, Token: "x", ProjectRef: "short"}, nil); err == nil || !strings.Contains(err.Error(), "project reference") {
		t.Errorf("bad ref: %v", err)
	}
	names := func(s *Supabase) []string {
		var out []string
		for _, td := range s.Tools() {
			out = append(out, td.Tool.Name)
		}
		return out
	}
	full := names(newTestSupabase(t, f, nil))
	want := []string{"project", "functions_list", "function_get", "secrets_list", "sql_query", "logs", "migrations_list", "migrations_pending", "migrations_apply", "function_deploy", "function_delete", "secrets_set", "secrets_delete"}
	if strings.Join(full, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v\nwant %v", full, want)
	}
	ro := names(newTestSupabase(t, f, func(c *config.Connection) { c.ReadOnly = true }))
	if strings.Join(ro, ",") != strings.Join(want[:8], ",") {
		t.Errorf("read-only tools = %v", ro)
	}
	s := newTestSupabase(t, f, func(c *config.Connection) { c.Instructions = "production project of the app" })
	if d := s.Tools()[0].Tool.Description; !strings.HasSuffix(d, "production project of the app") {
		t.Errorf("instructions not appended to project: %q", d)
	}
}

func TestSupabase_ProjectAndList(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	out, errText := sbCall(t, s, s.handleProject, nil)
	if errText != "" {
		t.Fatal(errText)
	}
	if out["name"] != "demo-project" || out["region"] != "eu-central-1" || out["organization"] != "org-x" || out["readOnly"] != false {
		t.Errorf("project = %v", out)
	}
	req, _ := f.last("GET", "/v1/projects/"+sbTestRef)
	if req.Accept != "application/json" || req.Header.Get("Authorization") != "Bearer sbp_test" {
		t.Errorf("headers = %v", req.Header)
	}

	out, errText = sbCall(t, s, s.handleFunctionsList, nil)
	if errText != "" {
		t.Fatal(errText)
	}
	fns := out["functions"].([]any)
	if len(fns) != 2 || out["count"] != float64(2) {
		t.Fatalf("functions = %v", out)
	}
	first := fns[0].(map[string]any)
	if first["slug"] != "hook" || first["version"] != float64(7) || first["verifyJwt"] != false || first["updated"] != "2025-10-31T17:00:00Z" || first["created"] != "2025-10-31T08:40:00Z" {
		t.Errorf("hook = %v", first)
	}
	if _, ok := first["name"]; ok {
		t.Error("name equal to slug should be omitted")
	}
	second := fns[1].(map[string]any)
	if second["name"] != "Other" || second["created"] != "2025-10-31T08:40:00Z" {
		t.Errorf("seconds-based timestamps: %v", second)
	}

	bad := newTestSupabase(t, f, func(c *config.Connection) { c.Token = "wrong" })
	if _, errText := sbCall(t, bad, bad.handleProject, nil); !strings.Contains(errText, "HTTP 401") || !strings.Contains(errText, "Unauthorized") {
		t.Errorf("401 = %q", errText)
	}
}

func TestSupabase_FunctionGet(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)

	out, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook"})
	if errText != "" {
		t.Fatal(errText)
	}
	files := out["files"].([]any)
	if len(files) != 2 || files[0].(map[string]any)["path"] != "functions/_shared/a.ts" || files[1].(map[string]any)["size"] != float64(26) {
		t.Errorf("files = %v", files)
	}
	if out["metadata"].(map[string]any)["entrypoint_path"] != "supabase/functions/hook/index.ts" {
		t.Errorf("metadata = %v", out["metadata"])
	}
	if _, ok := out["content"]; ok {
		t.Error("content without file argument")
	}
	req, _ := f.last("GET", "/functions/hook/body")
	if req.Accept != "multipart/form-data" {
		t.Errorf("Accept = %q", req.Accept)
	}

	out, errText = sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook", "file": "index.ts"})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["file"] != "functions/hook/index.ts" || out["content"] != "import '../_shared/a.ts';\n" {
		t.Errorf("inline = %v", out)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook", "file": "nope.ts"}); !strings.Contains(errText, "not among") {
		t.Errorf("unknown file: %q", errText)
	}

	dir := filepath.Join(t.TempDir(), "out")
	out, errText = sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook", "output_dir": dir})
	if errText != "" {
		t.Fatal(errText)
	}
	data, err := os.ReadFile(filepath.Join(dir, "functions", "_shared", "a.ts"))
	if err != nil || string(data) != "export const a = 1;\n" {
		t.Errorf("written file: %q %v", data, err)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook", "output_dir": dir}); !strings.Contains(errText, "already exists") {
		t.Errorf("second write must refuse: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook", "output_dir": "relative"}); !strings.Contains(errText, "absolute") {
		t.Errorf("relative output_dir: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "hook", "file": "x", "output_dir": dir}); !strings.Contains(errText, "alternatives") {
		t.Errorf("both: %q", errText)
	}

	out, errText = sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "bundled"})
	if errText != "" {
		t.Fatal(errText)
	}
	if b, ok := out["bundle"].(map[string]any); !ok || b["format"] != "eszip" || b["size"] != float64(11) {
		t.Errorf("bundle = %v", out)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "bundled", "file": "index.ts"}); !strings.Contains(errText, "pre-built bundle") {
		t.Errorf("file on a bundle: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "jsonbody"}); !strings.Contains(errText, "expected the function's files as multipart") {
		t.Errorf("json body: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "missing"}); !strings.Contains(errText, "not deployed") {
		t.Errorf("missing: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionGet, map[string]any{"slug": "../x"}); !strings.Contains(errText, "not a function slug") {
		t.Errorf("bad slug: %q", errText)
	}
}

func TestSupabase_Deploy(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"supabase/functions/hook/index.ts":  "import { a } from '../_shared/a.ts';\n",
		"supabase/functions/_shared/a.ts":   "export const a = 1;\n",
		"supabase/functions/_shared/b.ts":   "unused",
		"supabase/functions/hook/deno.json": `{"imports": {}}`,
	})

	// Dry run: plan only, no deploy request.
	out, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": root, "slug": "hook", "dry_run": true})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["deployed"] != false || out["dryRun"] != true || out["fileCount"] != float64(3) {
		t.Errorf("dry run = %v", out)
	}
	if f.count("POST", "/functions/deploy") != 0 {
		t.Fatal("dry run deployed")
	}
	if out["before"].(map[string]any)["version"] != float64(7) {
		t.Errorf("before = %v", out["before"])
	}
	// The deployed version holds index.ts (26 bytes) and _shared/a.ts under
	// the server's anchoring; the plan has them under supabase/ plus deno.json
	// and a differently sized index.ts.
	cmp := out["comparison"].(map[string]any)
	if cmp["sameFiles"] != false || fmt.Sprint(cmp["onlyLocal"]) != "[functions/hook/deno.json]" || cmp["onlyDeployed"] != nil {
		t.Errorf("comparison = %v", cmp)
	}
	if sd := fmt.Sprint(cmp["sizeDiffers"]); !strings.Contains(sd, "functions/hook/index.ts (local 37, deployed 26)") {
		t.Errorf("sizeDiffers = %v", sd)
	}
	// verify_jwt neither in config nor in the call: the deployed value is kept.
	if out["metadata"].(map[string]any)["verify_jwt"] != false {
		t.Errorf("metadata = %v", out["metadata"])
	}

	// Real deploy, with a 429 on the first try.
	f.mu.Lock()
	f.rateOnce = true
	f.mu.Unlock()
	out, errText = sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": root, "slug": "hook", "verify_jwt": true})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["deployed"] != true || out["function"].(map[string]any)["version"] != float64(8) {
		t.Errorf("deploy = %v", out)
	}
	if n := f.count("POST", "/functions/deploy"); n != 2 {
		t.Errorf("deploy requests = %d, want 2 (429 then 201)", n)
	}
	req, _ := f.last("POST", "/functions/deploy")
	if req.Query != "slug=hook" {
		t.Errorf("query = %q", req.Query)
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Fatalf("content type = %q", req.Header.Get("Content-Type"))
	}
	if cl := req.Header.Get("Content-Length"); cl == "" || cl != fmt.Sprint(len(req.Body)) {
		t.Errorf("Content-Length %q vs body %d", cl, len(req.Body))
	}
	mr := multipart.NewReader(strings.NewReader(string(req.Body)), params["boundary"])
	var partNames []string
	var meta map[string]any
	contents := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		if p.FormName() == "metadata" {
			json.Unmarshal(data, &meta)
			partNames = append(partNames, "metadata")
			continue
		}
		_, dp, _ := mime.ParseMediaType(p.Header.Get("Content-Disposition"))
		if p.FormName() != "file" {
			t.Errorf("part name = %q, want file", p.FormName())
		}
		partNames = append(partNames, dp["filename"])
		contents[dp["filename"]] = string(data)
	}
	if partNames[0] != "metadata" || len(partNames) != 4 {
		t.Errorf("parts = %v", partNames)
	}
	if contents["supabase/functions/_shared/a.ts"] != "export const a = 1;\n" || contents["supabase/functions/hook/index.ts"] == "" || contents["supabase/functions/hook/deno.json"] == "" {
		t.Errorf("contents = %v", contents)
	}
	if meta["name"] != "hook" || meta["entrypoint_path"] != "supabase/functions/hook/index.ts" || meta["import_map_path"] != "supabase/functions/hook/deno.json" || meta["verify_jwt"] != true {
		t.Errorf("metadata = %v", meta)
	}
	if sp, ok := meta["static_patterns"].([]any); !ok || len(sp) != 0 {
		t.Errorf("static_patterns = %v", meta["static_patterns"])
	}

	// Errors before the wire.
	if _, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": root, "slug": "nope"}); !strings.Contains(errText, "entrypoint not found") {
		t.Errorf("unknown slug: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": filepath.Join(root, "supabase"), "slug": "hook"}); !strings.Contains(errText, "project root") {
		t.Errorf("supabase dir as path: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"slug": "hook"}); !strings.Contains(errText, "path is required") {
		t.Errorf("no path: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": root, "slug": "hook", "verify_jwt": "yes"}); !strings.Contains(errText, "boolean") {
		t.Errorf("verify_jwt type: %q", errText)
	}
	before := f.count("POST", "/functions/deploy")
	if _, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": root, "slug": "hook", "dry_run": "yes"}); !strings.Contains(errText, "boolean") {
		t.Errorf("dry_run type: %q", errText)
	}
	if f.count("POST", "/functions/deploy") != before {
		t.Error("a non-boolean dry_run deployed")
	}
}

func TestSupabase_DeployNewFunction(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	root := t.TempDir()
	writeTree(t, root, map[string]string{"supabase/functions/missing/index.ts": "export default 1;"})
	// "missing" answers 404 on GET: a new function; the deploy endpoint of
	// the fake still answers with the hook record, fine for the shape.
	out, errText := sbCall(t, s, s.handleFunctionDeploy, map[string]any{"path": root, "slug": "missing", "dry_run": true})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["before"] != nil || !strings.Contains(out["note"].(string), "not deployed yet") {
		t.Errorf("new function = %v", out)
	}
	if _, ok := out["metadata"].(map[string]any)["verify_jwt"]; ok {
		t.Error("verify_jwt must be absent for a new function without config")
	}
}

func TestSupabase_FunctionDelete(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	out, errText := sbCall(t, s, s.handleFunctionDelete, map[string]any{"slug": "hook"})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["deleted"].(map[string]any)["slug"] != "hook" {
		t.Errorf("deleted = %v", out)
	}
	if f.count("DELETE", "/functions/hook") != 1 {
		t.Error("no DELETE sent")
	}
	if _, errText := sbCall(t, s, s.handleFunctionDelete, map[string]any{"slug": "missing"}); !strings.Contains(errText, "nothing to delete") {
		t.Errorf("missing: %q", errText)
	}
	if f.count("DELETE", "/functions/missing") != 0 {
		t.Error("DELETE sent for a function that does not exist")
	}
}

func TestSupabase_Secrets(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	out, errText := sbCall(t, s, s.handleSecretsList, nil)
	if errText != "" {
		t.Fatal(errText)
	}
	text, _ := json.Marshal(out)
	if strings.Contains(string(text), "sk-secret") || strings.Contains(string(text), "tw-secret") {
		t.Fatalf("secret values leaked: %s", text)
	}
	list := out["secrets"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["name"] != "OPENAI_API_KEY" || list[0].(map[string]any)["updated"] != "2026-10-30T10:00:00Z" {
		t.Errorf("secrets = %v", list)
	}

	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte("FROM_FILE=one\nSHARED=file\nSUPABASE_ANON_KEY=x\n"), 0o600)
	out, errText = sbCall(t, s, s.handleSecretsSet, map[string]any{"env_file": env, "secrets": map[string]any{"SHARED": "call", "DIRECT": "two"}})
	if errText != "" {
		t.Fatal(errText)
	}
	if fmt.Sprint(out["set"]) != "[DIRECT FROM_FILE SHARED]" || fmt.Sprint(out["fromEnvFile"]) != "[FROM_FILE]" || fmt.Sprint(out["skipped"]) != "[SUPABASE_ANON_KEY]" {
		t.Errorf("set = %v", out)
	}
	req, _ := f.last("POST", "/secrets")
	var body []map[string]string
	json.Unmarshal(req.Body, &body)
	got := map[string]string{}
	for _, e := range body {
		got[e["name"]] = e["value"]
	}
	if got["SHARED"] != "call" || got["FROM_FILE"] != "one" || got["DIRECT"] != "two" || len(got) != 3 {
		t.Errorf("body = %v", got)
	}
	if _, errText := sbCall(t, s, s.handleSecretsSet, map[string]any{"secrets": map[string]any{"SUPABASE_URL": "x"}}); !strings.Contains(errText, "reserved") {
		t.Errorf("reserved prefix: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleSecretsSet, nil); !strings.Contains(errText, "nothing to set") {
		t.Errorf("empty: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleSecretsSet, map[string]any{"secrets": map[string]any{"N": 1}}); !strings.Contains(errText, "must be a string") {
		t.Errorf("non-string: %q", errText)
	}

	out, errText = sbCall(t, s, s.handleSecretsDelete, map[string]any{"names": []any{"MAIL_API_KEY"}})
	if errText != "" {
		t.Fatal(errText)
	}
	req, _ = f.last("DELETE", "/secrets")
	if string(req.Body) != `["MAIL_API_KEY"]` {
		t.Errorf("delete body = %s", req.Body)
	}
	if _, errText := sbCall(t, s, s.handleSecretsDelete, nil); !strings.Contains(errText, "names is required") {
		t.Errorf("no names: %q", errText)
	}
}

func TestSupabase_SQL(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	out, errText := sbCall(t, s, s.handleSQLQuery, map[string]any{"query": "select 1 as n", "parameters": []any{"a", float64(2)}})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["rowCount"] != float64(2) || out["readOnly"] != false {
		t.Errorf("rows = %v", out)
	}
	req, _ := f.last("POST", "/database/query")
	if string(req.Body) != `{"parameters":["a",2],"query":"select 1 as n"}` {
		t.Errorf("body = %s", req.Body)
	}
	out, _ = sbCall(t, s, s.handleSQLQuery, map[string]any{"query": "select 1", "read_only": true})
	if out["readOnly"] != true || f.count("POST", "/database/query/read-only") != 1 {
		t.Errorf("read_only arg did not route to the read-only endpoint: %v", out)
	}
	if _, errText := sbCall(t, s, s.handleSQLQuery, map[string]any{"query": " "}); !strings.Contains(errText, "required") {
		t.Errorf("empty query: %q", errText)
	}

	ro := newTestSupabase(t, f, func(c *config.Connection) { c.ReadOnly = true })
	out, _ = sbCall(t, ro, ro.handleSQLQuery, map[string]any{"query": "delete from x"})
	if out["readOnly"] != true || f.count("POST", "/database/query/read-only") != 2 {
		t.Errorf("read-only connection must use the read-only endpoint: %v", out)
	}
	if _, errText := sbCall(t, ro, ro.handleSQLQuery, map[string]any{"query": "select 1", "read_only": false}); !strings.Contains(errText, "read-only") {
		t.Errorf("read_only=false on a read-only connection: %q", errText)
	}
}

func TestSupabase_Logs(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	out, errText := sbCall(t, s, s.handleLogs, map[string]any{"function": "hook", "search": "it's", "limit": float64(5)})
	if errText != "" {
		t.Fatal(errText)
	}
	sql := out["sql"].(string)
	for _, want := range []string{"FROM logs", "source = 'function_logs'", "log_attributes['function_id'] = 'fid-1'", `positionCaseInsensitive(event_message, 'it\'s') > 0`, "ORDER BY timestamp DESC LIMIT 5", `log_attributes['level'] AS "level"`, "event_message, severity_text,"} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql lacks %q:\n%s", want, sql)
		}
	}
	if out["start"] != "2026-10-31T19:00:00Z" || out["end"] != "2026-10-31T20:00:00Z" || out["source"] != "function_logs" {
		t.Errorf("window = %v", out)
	}
	rows := out["rows"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["timestamp"] != "2025-10-31T18:10:00.123456Z" || rows[1].(map[string]any)["timestamp"] != "2026-11-01T13:04:01.230000Z" {
		t.Errorf("rows = %v", rows)
	}
	req, _ := f.last("GET", "/analytics/endpoints/logs")
	if !strings.Contains(req.Query, "iso_timestamp_start=2026-10-31T19%3A00%3A00Z") || !strings.Contains(req.Query, "iso_timestamp_end=2026-10-31T20%3A00%3A00Z") {
		t.Errorf("query = %s", req.Query)
	}

	// Window rules.
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"start": "2026-10-30 10:00", "end": "2026-10-31T12:00:00Z"}); !strings.Contains(errText, "24 hours") {
		t.Errorf("long window: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"start": "2026-10-31T21:00:00Z"}); !strings.Contains(errText, "before end") {
		t.Errorf("inverted: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"start": "yesterday"}); !strings.Contains(errText, "not a timestamp") {
		t.Errorf("bad time: %q", errText)
	}
	out, _ = sbCall(t, s, s.handleLogs, map[string]any{"source": "postgres_logs", "start": "2026-10-31", "end": "2026-10-31 06:00"})
	if out["start"] != "2026-10-31T00:00:00Z" || out["end"] != "2026-10-31T06:00:00Z" || !strings.Contains(out["sql"].(string), "source = 'postgres_logs'") {
		t.Errorf("explicit window = %v", out)
	}

	// Source and function rules.
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"source": "nope"}); !strings.Contains(errText, "not offered") {
		t.Errorf("bad source: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"source": "edge_logs", "function": "hook"}); !strings.Contains(errText, "function applies") {
		t.Errorf("function on edge_logs: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"function": "ghost"}); !strings.Contains(errText, "not deployed") {
		t.Errorf("unknown function: %q", errText)
	}

	// Raw SQL passes through; the API's error envelope becomes a tool error.
	out, _ = sbCall(t, s, s.handleLogs, map[string]any{"sql": "select 1 from logs", "limit": float64(1)})
	if out["sql"] != "select 1 from logs" {
		t.Errorf("raw sql = %v", out["sql"])
	}
	if _, ok := out["source"]; ok {
		t.Error("source reported although the query is the caller's")
	}
	if _, ok := out["note"]; ok {
		t.Error("limit note although limit does not apply to the caller's SQL")
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"limit": "50"}); !strings.Contains(errText, "must be a number") {
		t.Errorf("string limit: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"limit": float64(0.5)}); !strings.Contains(errText, "at least 1") {
		t.Errorf("tiny limit: %q", errText)
	}
	out, _ = sbCall(t, s, s.handleLogs, map[string]any{"limit": float64(1e300)})
	if !strings.Contains(out["sql"].(string), fmt.Sprintf("LIMIT %d", sbLogsMaxRows)) {
		t.Errorf("huge limit not capped: %v", out["sql"])
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"sql": "select 1", "function": "hook"}); !strings.Contains(errText, "cannot be combined") {
		t.Errorf("sql + function: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleLogs, map[string]any{"sql": "BROKEN"}); !strings.Contains(errText, "Syntax error") {
		t.Errorf("api error: %q", errText)
	}
}

func TestSupabase_Errors(t *testing.T) {
	for body, want := range map[string]string{
		`{"message":"Unauthorized","statusCode":401}`: "Unauthorized",
		`{"message":["a must be x","b is missing"]}`:  "a must be x; b is missing",
		`{"error":"boom"}`:                            "boom",
		`<html>gateway</html>`:                        "<html>gateway</html>",
	} {
		if got := sbErrorMessage(500, []byte(body)).Error(); !strings.Contains(got, want) {
			t.Errorf("%s → %q, want %q", body, got, want)
		}
	}
	if d := sbRetryDelay("7", 1); d != 7*time.Second {
		t.Errorf("retry-after 7 = %s", d)
	}
	if d := sbRetryDelay("9999", 1); d != sbRetryMaxDelay {
		t.Errorf("retry-after cap = %s", d)
	}
	if d := sbRetryDelay("", 2); d != 4*time.Second {
		t.Errorf("fallback = %s", d)
	}
	if sbTime(0) != "" || sbTime(1761900000) != "2025-10-31T08:40:00Z" || sbTime(1761900000000) != "2025-10-31T08:40:00Z" {
		t.Errorf("sbTime: %q %q", sbTime(1761900000), sbTime(1761900000000))
	}

	// A call to an unregistered endpoint or with an unlisted parameter never
	// reaches the wire.
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	if _, err := s.call(context.Background(), "nope", nil, nil, nil, "", nil); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("unregistered: %v", err)
	}
	q := map[string][]string{"x": {"1"}}
	if _, err := s.call(context.Background(), "functions", nil, q, nil, "", nil); err == nil || !strings.Contains(err.Error(), "does not take query parameter") {
		t.Errorf("unlisted param: %v", err)
	}
	if _, err := s.call(context.Background(), "function", map[string]string{"function_slug": "a/b"}, nil, nil, "", nil); err == nil || !strings.Contains(err.Error(), "not a valid value") {
		t.Errorf("path-shaping value: %v", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("%d requests went out", len(f.requests))
	}

	// A redirect to another host is refused, so a body is never replayed
	// elsewhere.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request followed to the other host: %s", r.URL)
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	rs, err := NewSupabase(config.Connection{Name: "r", Type: config.TypeSupabase, URL: redirector.URL, Token: "sbp_test", ProjectRef: sbTestRef}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("cross-host redirect: %v", err)
	}
}
