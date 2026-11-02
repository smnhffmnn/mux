package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/blob"
)

// useTestBlobStore installs a store under the test's temp dir for the
// duration of the test. Tests that need "no store" call SetBlobStore(nil)
// themselves after this.
func useTestBlobStore(t *testing.T) *blob.Store {
	t.Helper()
	s, err := blob.Open(filepath.Join(t.TempDir(), "blobs"), blob.Options{BaseURL: "http://127.0.0.1:7700"})
	if err != nil {
		t.Fatal(err)
	}
	prev := blobStore
	SetBlobStore(s)
	t.Cleanup(func() { SetBlobStore(prev) })
	return s
}

var blobIDPattern = regexp.MustCompile(`blob ([0-9a-f]{32})`)

// blobIDFromText finds the id a result's reference names.
func blobIDFromText(t *testing.T, text string) string {
	t.Helper()
	m := blobIDPattern.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no blob reference in result:\n%s", text)
	}
	return m[1]
}

// readBlob returns the bytes of a stored blob.
func readBlob(t *testing.T, s *blob.Store, id string) []byte {
	t.Helper()
	rc, _, err := s.Open(id)
	if err != nil {
		t.Fatalf("open blob %s: %v", id, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBlobRef(t *testing.T) {
	s := useTestBlobStore(t)
	info, _ := s.Put(strings.NewReader("x"), blob.Meta{Name: "a.txt"})

	if id, ok, err := blobRef("/tmp/file"); ok || err != nil || id != "" {
		t.Fatalf("plain path: %q %v %v", id, ok, err)
	}
	if id, ok, err := blobRef(" blob:" + info.ID + " "); !ok || err != nil || id != info.ID {
		t.Fatalf("reference: %q %v %v", id, ok, err)
	}
	if id, ok, err := blobRef("BLOB:" + info.ID); !ok || err != nil || id != info.ID {
		t.Fatalf("upper-case prefix: %q %v %v", id, ok, err)
	}
	if _, ok, err := blobRef("blob:../etc/passwd"); !ok || err == nil {
		t.Fatalf("bad id must be an error, got ok=%v err=%v", ok, err)
	}
	SetBlobStore(nil)
	if _, ok, err := blobRef("blob:" + info.ID); !ok || err == nil || !strings.Contains(err.Error(), "no blob store") {
		t.Fatalf("without a store: ok=%v err=%v", ok, err)
	}
}

func TestBlobInputFileAndTree(t *testing.T) {
	s := useTestBlobStore(t)
	file, _ := s.Put(strings.NewReader("content"), blob.Meta{Name: "f.bin"})
	var archive bytes.Buffer
	writeTestTarGz(t, &archive, map[string]string{"supabase/config.toml": "x", "supabase/functions/h/index.ts": "y"})
	tree, err := s.PutTree(&archive, blob.Meta{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}

	p, info, err := blobInputFile("/some/path", "file_path")
	if err != nil || info != nil || p != "/some/path" {
		t.Fatalf("plain path passes through: %q %v %v", p, info, err)
	}
	p, info, err = blobInputFile("blob:"+file.ID, "file_path")
	if err != nil || info == nil || info.ID != file.ID {
		t.Fatalf("file blob: %q %v %v", p, info, err)
	}
	if data, _ := os.ReadFile(p); string(data) != "content" {
		t.Fatalf("resolved path reads %q", data)
	}
	if _, _, err := blobInputFile("blob:"+tree.ID, "file_path"); err == nil || !strings.Contains(err.Error(), "directory tree") {
		t.Fatalf("tree as file: %v", err)
	}
	if _, _, err := blobInputTree("blob:"+file.ID, "path"); err == nil || !strings.Contains(err.Error(), "tree=1") {
		t.Fatalf("file as tree: %v", err)
	}
	dir, info, err := blobInputTree("blob:"+tree.ID, "path")
	if err != nil || info == nil {
		t.Fatalf("tree blob: %q %v %v", dir, info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "supabase", "functions", "h", "index.ts")); err != nil {
		t.Fatalf("tree not unpacked under the resolved dir: %v", err)
	}
	s.Delete(file.ID)
	if _, _, err := blobInputFile("blob:"+file.ID, "env_file"); err == nil || !strings.Contains(err.Error(), "env_file: blob") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("deleted blob: %v", err)
	}
	consumeBlob(info)
	if _, err := s.Get(tree.ID); err == nil {
		t.Fatal("consumeBlob left the tree")
	}
	consumeBlob(nil) // a plain path has nothing to consume
}

func TestWantsBlobOutput(t *testing.T) {
	for in, want := range map[string]bool{"blob": true, " Blob ": true, "": false, "/tmp/x": false, "blob:abc": false} {
		if got := wantsBlobOutput(in); got != want {
			t.Errorf("wantsBlobOutput(%q) = %v", in, got)
		}
	}
}

func TestReadBodyLimitedSpillsToBlob(t *testing.T) {
	s := useTestBlobStore(t)
	payload := bytes.Repeat([]byte("x"), 150)
	resp := &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(payload))}
	data, marker, err := readBodyLimited(resp, 100, "r.json", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 100 {
		t.Fatalf("preview = %d bytes", len(data))
	}
	if !strings.Contains(marker, "truncated at 100 bytes") || !strings.Contains(marker, "150 bytes") {
		t.Fatalf("marker = %q", marker)
	}
	full := readBlob(t, s, blobIDFromText(t, marker))
	if !bytes.Equal(full, payload) {
		t.Fatalf("stored body differs: %d bytes", len(full))
	}

	// Short bodies pass through untouched.
	resp = &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("short"))}
	data, marker, err = readBodyLimited(resp, 100, "r", "test")
	if err != nil || string(data) != "short" || marker != "" {
		t.Fatalf("short: %q %q %v", data, marker, err)
	}

	// Without a store the marker says how to get the rest.
	SetBlobStore(nil)
	resp = &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(payload))}
	data, marker, err = readBodyLimited(resp, 100, "r", "test")
	if err != nil || len(data) != 100 || !strings.Contains(marker, "output_file") {
		t.Fatalf("no store: %d %q %v", len(data), marker, err)
	}
}

func TestReadBodyLimitedNeverStoresErrorBodies(t *testing.T) {
	s := useTestBlobStore(t)
	resp := &http.Response{StatusCode: 500, Status: "500 Internal Server Error", Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("e"), 150)))}
	data, marker, err := readBodyLimited(resp, 100, "r", "test")
	if err != nil || len(data) != 100 || !strings.Contains(marker, "error response truncated at 100 bytes") {
		t.Fatalf("error body: %d %q %v", len(data), marker, err)
	}
	if n, _ := s.Stats(); n != 0 {
		t.Fatalf("an error body was stored (%d blobs)", n)
	}
}

func TestStoreResponseAsBlob(t *testing.T) {
	s := useTestBlobStore(t)
	resp := &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader("audio-bytes"))}
	res, err := storeResponseAsBlob(resp, "audio/mpeg", "track.mp3", "test", "note line", true)
	if err != nil || res.IsError {
		t.Fatalf("%v %s", err, resultText(t, res))
	}
	text := resultText(t, res)
	for _, want := range []string{"HTTP 200", "Content-Type: audio/mpeg", "Size: 11 bytes", "note line", "blob:"} {
		if !strings.Contains(text, want) {
			t.Errorf("result lacks %q:\n%s", want, text)
		}
	}
	if got := readBlob(t, s, blobIDFromText(t, text)); string(got) != "audio-bytes" {
		t.Fatalf("stored %q", got)
	}

	// An empty body is an error where the caller needs one (audio), a plain
	// success where it does not (a POST answered 201 with nothing) — and
	// leaves nothing behind either way.
	resp = &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(""))}
	res, _ = storeResponseAsBlob(resp, "audio/mpeg", "", "test", "", true)
	if !res.IsError || !strings.Contains(resultText(t, res), "empty response body") {
		t.Fatalf("empty: %s", resultText(t, res))
	}
	resp = &http.Response{StatusCode: 201, Status: "201 Created", Body: io.NopCloser(strings.NewReader(""))}
	res, _ = storeResponseAsBlob(resp, "application/json", "", "output_file", "Uploaded x", false)
	if res.IsError || !strings.Contains(resultText(t, res), "Empty response body") || !strings.Contains(resultText(t, res), "Uploaded x") {
		t.Fatalf("empty success: %s", resultText(t, res))
	}
	if n, _ := s.Stats(); n != 1 {
		t.Fatalf("empty body stored a blob (%d blobs)", n)
	}

	// Without a store (a stdio instance) the body goes to the output directory
	// of the config dir, as it did before the store existed.
	SetBlobStore(nil)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	resp = &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader("x"))}
	res, _ = storeResponseAsBlob(resp, "audio/mpeg", "", "test", "", true)
	if res.IsError {
		t.Fatalf("no store: %s", resultText(t, res))
	}
	path := savedPath(t, resultText(t, res))
	if filepath.Dir(path) != defaultOutputDir() || filepath.Ext(path) != ".mp3" {
		t.Fatalf("fallback wrote %q, want an .mp3 under %q", path, defaultOutputDir())
	}
}

func TestParseUploadFromBlob(t *testing.T) {
	s := useTestBlobStore(t)
	info, _ := s.Put(strings.NewReader("PNGDATA"), blob.Meta{Name: "pic.png", ContentType: "image/png"})
	u, err := parseUpload(toolRequest(map[string]any{"file_path": "blob:" + info.ID, "form_fields": `{"title":"x"}`}), "file")
	if err != nil {
		t.Fatal(err)
	}
	if u.fileName != "pic.png" || u.contentType != "image/png" || u.size != 7 || u.blob == nil {
		t.Fatalf("upload = %+v", u)
	}
	if !strings.Contains(string(u.head), `filename="pic.png"`) {
		t.Fatalf("part header names %q, want the blob's name:\n%s", u.fileName, u.head)
	}
	if n := u.note(); !strings.Contains(n, "pic.png") {
		t.Fatalf("note = %q", n)
	}
	body, err := u.openBody()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(body)
	body.Close()
	if !bytes.Contains(all, []byte("PNGDATA")) {
		t.Fatal("body lacks the blob's bytes")
	}

	// A 2xx consumes the blob; a failure leaves it for the retry, and so does
	// a 2xx that answers a GET — Go replays a 302 as a GET without the body.
	httpReq, _ := u.newRequest(context.Background(), http.MethodPost, "http://example.invalid/upload")
	u.finish(&http.Response{StatusCode: 500, Request: httpReq})
	if _, err := s.Get(info.ID); err != nil {
		t.Fatal("5xx must not consume the blob")
	}
	getReq, _ := http.NewRequest(http.MethodGet, "http://example.invalid/login", nil)
	u.finish(&http.Response{StatusCode: 200, Request: getReq})
	if _, err := s.Get(info.ID); err != nil {
		t.Fatal("a 2xx on a replayed GET must not consume the blob")
	}
	u.finish(&http.Response{StatusCode: 201, Request: httpReq})
	if _, err := s.Get(info.ID); err == nil {
		t.Fatal("2xx must consume the blob")
	}

	if _, err := parseUpload(toolRequest(map[string]any{"file_path": "blob:nope"}), "file"); err == nil || !strings.Contains(err.Error(), "file_path") {
		t.Fatalf("bad reference: %v", err)
	}

	// A nameless blob gets its id plus an extension from the type, so an API
	// that reads the format off the file name (Whisper) has one.
	nameless, _ := s.Put(strings.NewReader("RIFF"), blob.Meta{ContentType: "audio/mpeg"})
	u2, err := parseUpload(toolRequest(map[string]any{"file_path": "blob:" + nameless.ID}), "file")
	if err != nil || u2.fileName != nameless.ID+".mp3" {
		t.Fatalf("nameless blob part name = %q (%v)", u2.fileName, err)
	}
}

func TestValidateOutputFileBlobSentinel(t *testing.T) {
	useTestBlobStore(t)
	got, err := validateOutputFile("blob")
	if err != nil || got != blobOutputSentinel {
		t.Fatalf("%q %v", got, err)
	}
	resp := &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader("data"))}
	res, _ := saveResponseToFileWithNote(resp, "text/plain", "blob", "")
	if res.IsError || !strings.Contains(resultText(t, res), "Stored as blob") {
		t.Fatalf("sentinel did not store a blob: %s", resultText(t, res))
	}
	SetBlobStore(nil)
	if _, err := validateOutputFile("blob"); err == nil || !strings.Contains(err.Error(), "no blob store") {
		t.Fatalf("without a store: %v", err)
	}
}

// writeTestTarGz writes a tar.gz with the given files (path → content) to w.
func writeTestTarGz(t *testing.T, w io.Writer, files map[string]string) {
	t.Helper()
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(files[n]))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}
