package blob

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name    string
	body    string
	dir     bool
	symlink string
}

func tarArchive(t *testing.T, gz bool, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(&buf)
		w = gzw
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644}
		switch {
		case e.dir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		case e.symlink != "":
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = e.symlink
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	if gzw != nil {
		gzw.Close()
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		name := e.name
		if e.dir && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if e.symlink != "" {
			hdr.SetMode(os.ModeSymlink | 0o777)
		} else if e.dir {
			hdr.SetMode(os.ModeDir | 0o755)
		} else {
			hdr.SetMode(0o644)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if e.symlink != "" {
			w.Write([]byte(e.symlink))
		} else if !e.dir {
			w.Write([]byte(e.body))
		}
	}
	zw.Close()
	return buf.Bytes()
}

var projectEntries = []entry{
	{name: "supabase/", dir: true},
	{name: "supabase/config.toml", body: "project_id = \"x\"\n"},
	{name: "supabase/functions/hook/index.ts", body: "import { a } from '../_shared/a.ts'\n"},
	{name: "supabase/functions/_shared/a.ts", body: "export const a = 1\n"},
	{name: "supabase/functions/hook/link", symlink: "/etc/passwd"},
}

func TestPutTreeTarGz(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	info, err := s.PutTree(bytes.NewReader(tarArchive(t, true, projectEntries)), Meta{Name: "myproject", Source: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != KindTree || info.Format != FormatTarGz || info.Files != 3 || info.Skipped != 1 {
		t.Fatalf("info = %+v", info)
	}
	if len(info.TopLevel) != 1 || info.TopLevel[0] != "supabase" {
		t.Fatalf("topLevel = %v", info.TopLevel)
	}
	dir, _, err := s.Path(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "supabase", "functions", "_shared", "a.ts"))
	if err != nil || string(data) != "export const a = 1\n" {
		t.Fatalf("unpacked file: %v %q", err, data)
	}
	if _, err := os.Lstat(filepath.Join(dir, "supabase", "functions", "hook", "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink was materialised: %v", err)
	}
	entries, _, err := s.Entries(info.ID)
	if err != nil || len(entries) != 3 || entries[0].Path != "supabase/config.toml" {
		t.Fatalf("entries: %v %+v", err, entries)
	}
	// Spool file is gone, only the tree remains.
	left, _ := os.ReadDir(s.Dir())
	if len(left) != 1 || left[0].Name() != info.ID+".tree" {
		t.Fatalf("store dir after PutTree: %v", left)
	}

	// Round trip through Archive.
	var out bytes.Buffer
	if err := s.Archive(info.ID, &out); err != nil {
		t.Fatal(err)
	}
	s2, _ := newTestStore(t, Options{})
	again, err := s2.PutTree(bytes.NewReader(out.Bytes()), Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Files != 3 || again.Size != info.Size {
		t.Fatalf("round trip: %+v vs %+v", again, info)
	}

	s.Delete(info.ID)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tree dir after delete: %v", err)
	}
}

func TestPutTreePlainTarAndZip(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	plain, err := s.PutTree(bytes.NewReader(tarArchive(t, false, projectEntries)), Meta{})
	if err != nil || plain.Format != FormatTar || plain.Files != 3 {
		t.Fatalf("tar: %v %+v", err, plain)
	}
	z, err := s.PutTree(bytes.NewReader(zipArchive(t, projectEntries)), Meta{})
	if err != nil || z.Format != FormatZip || z.Files != 3 || z.Skipped != 1 {
		t.Fatalf("zip: %v %+v", err, z)
	}
}

func TestSafeNameEdges(t *testing.T) {
	for _, ok := range []string{"1:2.sql", "a..b/c", "./x/y.ts", "x:y"} {
		if _, err := safeName(ok); err != nil {
			t.Errorf("safeName(%q) refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"C:/x", "c:", "/etc/passwd", "a/../../x", "a/../x", "a\\..\\x", "x\x00y"} {
		if _, err := safeName(bad); err == nil || !errors.Is(err, ErrArchive) {
			t.Errorf("safeName(%q) accepted (%v)", bad, err)
		}
	}
}

func TestPutTreeCountsEveryEntry(t *testing.T) {
	old := maxTreeFiles
	maxTreeFiles = 4
	t.Cleanup(func() { maxTreeFiles = old })
	s, _ := newTestStore(t, Options{})
	dirs := []entry{{name: "a/", dir: true}, {name: "b/", dir: true}, {name: "c/", dir: true}, {name: "d/", dir: true}, {name: "e.txt", body: "x"}}
	if _, err := s.PutTree(bytes.NewReader(tarArchive(t, true, dirs)), Meta{}); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("directories over the cap: %v", err)
	}
	if _, err := s.PutTree(bytes.NewReader(zipArchive(t, dirs)), Meta{}); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("zip over the cap: %v", err)
	}
	links := []entry{{name: "l1", symlink: "/x"}, {name: "l2", symlink: "/x"}, {name: "l3", symlink: "/x"}, {name: "l4", symlink: "/x"}, {name: "f", body: "x"}}
	if _, err := s.PutTree(bytes.NewReader(tarArchive(t, true, links)), Meta{}); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("skipped entries over the cap: %v", err)
	}
	left, _ := os.ReadDir(s.Dir())
	if len(left) != 0 {
		t.Fatalf("refused archives left files: %v", left)
	}
	fine := []entry{{name: "a/", dir: true}, {name: "a/f", body: "x"}}
	if info, err := s.PutTree(bytes.NewReader(tarArchive(t, true, fine)), Meta{}); err != nil || info.Files != 1 {
		t.Fatalf("within the cap: %v %+v", err, info)
	}
}

func TestPutTreeIgnoresPaxGlobalHeaderAndBoundsTopLevel(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "abc"}})
	for i := 0; i < 20; i++ {
		body := "x"
		name := string(rune('a'+i)) + ".txt"
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	info, err := s.PutTree(&buf, Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Skipped != 0 || info.Files != 20 {
		t.Fatalf("pax header counted: %+v", info)
	}
	if len(info.TopLevel) != maxTopLevel+1 || !strings.HasPrefix(info.TopLevel[maxTopLevel], "… 4 more") {
		t.Fatalf("topLevel = %v", info.TopLevel)
	}
}

func TestPutTreeRejectsEscapes(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	for _, bad := range [][]entry{
		{{name: "../evil.txt", body: "x"}},
		{{name: "a/../../evil.txt", body: "x"}},
		{{name: "/etc/evil.txt", body: "x"}},
	} {
		if _, err := s.PutTree(bytes.NewReader(tarArchive(t, true, bad)), Meta{}); err == nil {
			t.Fatalf("accepted %q", bad[0].name)
		}
		if _, err := s.PutTree(bytes.NewReader(zipArchive(t, bad)), Meta{}); err == nil {
			t.Fatalf("zip accepted %q", bad[0].name)
		}
	}
	left, _ := os.ReadDir(s.Dir())
	if len(left) != 0 {
		t.Fatalf("refused archives left files: %v", left)
	}
	// A name that only looks suspicious is fine.
	if _, err := s.PutTree(bytes.NewReader(tarArchive(t, true, []entry{{name: "./a..b/c.txt", body: "x"}})), Meta{}); err != nil {
		t.Fatalf("rejected a legitimate name: %v", err)
	}
}

func TestPutTreeLimits(t *testing.T) {
	s, _ := newTestStore(t, Options{MaxTotal: 10_000, MaxBlob: 100})
	big := []entry{{name: "a", body: strings.Repeat("x", 60)}, {name: "b", body: strings.Repeat("y", 60)}}
	_, err := s.PutTree(bytes.NewReader(tarArchive(t, false, big)), Meta{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("unpacked size over cap: %v", err)
	}
	if _, err := s.PutTree(bytes.NewReader([]byte("not an archive at all")), Meta{}); !errors.Is(err, ErrFormat) {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := s.PutTree(bytes.NewReader(tarArchive(t, true, []entry{{name: "d/", dir: true}})), Meta{}); err == nil {
		t.Fatal("archive without files accepted")
	}
	if _, err := s.PutTree(bytes.NewReader(nil), Meta{}); err == nil {
		t.Fatal("empty upload accepted")
	}
	left, _ := os.ReadDir(s.Dir())
	if len(left) != 0 {
		t.Fatalf("failed uploads left files: %v", left)
	}
}

func TestPutDir(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "functions", "x"), 0o755)
	os.WriteFile(filepath.Join(src, "functions", "x", "index.ts"), []byte("hi"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(src, "functions", "link"))
	info, err := s.PutDir(src, Meta{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Files != 1 || info.Skipped != 1 || info.Size != 2 || info.TopLevel[0] != "functions" {
		t.Fatalf("info = %+v", info)
	}
}

func TestPutTreeSkipsMacOSSidecars(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	entries := []entry{
		{name: "./", dir: true},
		{name: "._supabase", body: "AppleDouble"},
		{name: "supabase/", dir: true},
		{name: "supabase/._functions", body: "AppleDouble"},
		{name: "supabase/functions/hook/index.ts", body: "export default 1\n"},
		{name: "supabase/functions/hook/._index.ts", body: "AppleDouble"},
		{name: "__MACOSX/", dir: true},
		{name: "__MACOSX/._x", body: "AppleDouble"},
	}
	info, err := s.PutTree(bytes.NewReader(tarArchive(t, true, entries)), Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Files != 1 || info.Skipped != 5 {
		t.Fatalf("files %d skipped %d, want 1 and 5: %+v", info.Files, info.Skipped, info)
	}
	if len(info.TopLevel) != 1 || info.TopLevel[0] != "supabase" {
		t.Fatalf("topLevel = %v, want [supabase] (no \".\", no sidecars)", info.TopLevel)
	}
	list, _, _ := s.Entries(info.ID)
	if len(list) != 1 || list[0].Path != "supabase/functions/hook/index.ts" {
		t.Fatalf("entries = %+v", list)
	}
	z, err := s.PutTree(bytes.NewReader(zipArchive(t, entries)), Meta{})
	if err != nil || z.Files != 1 || z.Skipped != 5 {
		t.Fatalf("zip: %v %+v", err, z)
	}
}
