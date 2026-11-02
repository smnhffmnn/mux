package blob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestStore(t *testing.T, opts Options) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	opts.Now = c.now
	s, err := Open(filepath.Join(t.TempDir(), "blobs"), opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestPutGetOpenDelete(t *testing.T) {
	s, c := newTestStore(t, Options{TTL: time.Hour})
	info, err := s.Put(strings.NewReader("hello"), Meta{Name: "a.txt", ContentType: "text/plain", Source: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(info.ID) || info.Kind != KindFile || info.Size != 5 || info.Name != "a.txt" {
		t.Fatalf("info = %+v", info)
	}
	if !info.Expires.Equal(c.t.Add(time.Hour)) {
		t.Fatalf("expires %v, want %v", info.Expires, c.t.Add(time.Hour))
	}
	got, err := s.Get(info.ID)
	if err != nil || got.ID != info.ID {
		t.Fatalf("Get: %v %+v", err, got)
	}
	rc, _, err := s.Open(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "hello" {
		t.Fatalf("body %q", data)
	}
	p, _, err := s.Path(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("stored file: %v %v", err, fi)
	}
	s.Delete(info.ID)
	if _, err := s.Get(info.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still there: %v", err)
	}
	// Deleting twice is not an error.
	s.Delete(info.ID)
}

func TestExpiry(t *testing.T) {
	s, c := newTestStore(t, Options{TTL: time.Minute})
	info, err := s.Put(strings.NewReader("x"), Meta{})
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(59 * time.Second)
	if _, err := s.Get(info.ID); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	c.t = c.t.Add(time.Second)
	if _, err := s.Get(info.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("at expiry: %v", err)
	}
	if n, b := s.Stats(); n != 0 || b != 0 {
		t.Fatalf("stats after expiry: %d %d", n, b)
	}
}

func TestEvictionOldestFirst(t *testing.T) {
	s, c := newTestStore(t, Options{MaxTotal: 1000, MaxBlob: 1000})
	var ids []string
	for i := 0; i < 3; i++ {
		info, err := s.Put(bytes.NewReader(make([]byte, 400)), Meta{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, info.ID)
		c.t = c.t.Add(time.Second)
	}
	if _, err := s.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest should be evicted: %v", err)
	}
	for _, id := range ids[1:] {
		if _, err := s.Get(id); err != nil {
			t.Fatalf("%s evicted: %v", id, err)
		}
	}
	if n, b := s.Stats(); n != 2 || b != 800 {
		t.Fatalf("stats %d %d", n, b)
	}
}

func TestTooLarge(t *testing.T) {
	s, _ := newTestStore(t, Options{MaxTotal: 1000, MaxBlob: 100})
	_, err := s.Put(bytes.NewReader(make([]byte, 101)), Meta{})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 0 {
		t.Fatalf("leftover files after refused put: %v", entries)
	}
	if _, err := s.Put(bytes.NewReader(make([]byte, 100)), Meta{}); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
}

func TestOpenPerProcessDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	// A directory of a process that is gone, one of a live process (our own
	// parent), and a stray file.
	stale := filepath.Join(root, "4000000000")
	os.MkdirAll(stale, 0o700)
	os.WriteFile(filepath.Join(stale, "x"), []byte("x"), 0o600)
	live := filepath.Join(root, strconv.Itoa(os.Getppid()))
	os.MkdirAll(live, 0o700)
	os.WriteFile(filepath.Join(live, "y"), []byte("y"), 0o600)
	os.WriteFile(filepath.Join(root, "stray"), []byte("z"), 0o600)

	s, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Dir() != filepath.Join(root, strconv.Itoa(os.Getpid())) {
		t.Fatalf("dir = %q", s.Dir())
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale process directory survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(live, "y")); err != nil {
		t.Fatalf("a live process's directory was touched: %v", err)
	}
	// A second Open of the same root by this process starts fresh without
	// touching the live one; a real second instance has another pid.
	s2, err := Open(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(s2.Dir()); len(entries) != 0 {
		t.Fatalf("own directory not cleared: %v", entries)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s2.Dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Close left the directory")
	}
	if _, err := Open("relative/blobs", Options{}); err == nil {
		t.Fatal("relative root accepted")
	}
}

func TestListNewestFirst(t *testing.T) {
	s, c := newTestStore(t, Options{})
	a, _ := s.Put(strings.NewReader("a"), Meta{Name: "a"})
	c.t = c.t.Add(time.Second)
	b, _ := s.Put(strings.NewReader("b"), Meta{Name: "b"})
	list := s.List()
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != a.ID {
		t.Fatalf("list = %+v", list)
	}
}

func TestWriterAbort(t *testing.T) {
	s, _ := newTestStore(t, Options{})
	w, err := s.Create(Meta{})
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("partial"))
	w.Abort()
	if n, _ := s.Stats(); n != 0 {
		t.Fatalf("aborted blob counted")
	}
	entries, _ := os.ReadDir(s.Dir())
	if len(entries) != 0 {
		t.Fatalf("aborted file left: %v", entries)
	}
	if _, err := w.Close(); err == nil {
		t.Fatal("Close after Abort should fail")
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("0123456789abcdef0123456789abcdef") {
		t.Fatal("valid id rejected")
	}
	for _, bad := range []string{"", "../x", "0123456789ABCDEF0123456789abcdef", "0123456789abcdef0123456789abcde", "0123456789abcdef0123456789abcdefg"} {
		if ValidID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int64]string{5: "5 bytes", 2048: "2.0 KB", 3 << 20: "3.0 MB", 1 << 30: "1.0 GB"}
	for n, want := range cases {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTempPathAdopt(t *testing.T) {
	s, _ := newTestStore(t, Options{MaxTotal: 1000, MaxBlob: 100})
	p, err := s.TempPath()
	if err != nil || filepath.Dir(p) != s.Dir() {
		t.Fatalf("TempPath: %q %v", p, err)
	}
	os.WriteFile(p, []byte("stdout"), 0o600)
	info, err := s.Adopt(p, Meta{Name: "stdout.txt", ContentType: "text/plain", Source: "ssh"})
	if err != nil || info.Size != 6 || info.Name != "stdout.txt" {
		t.Fatalf("Adopt: %+v %v", info, err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file still there after adopt: %v", err)
	}
	rc, _, err := s.Open(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "stdout" {
		t.Fatalf("adopted content %q", data)
	}
	// Not from TempPath: refused.
	other := filepath.Join(t.TempDir(), "x")
	os.WriteFile(other, []byte("x"), 0o600)
	if _, err := s.Adopt(other, Meta{}); err == nil {
		t.Fatal("adopted a foreign path")
	}
	// Over the cap: refused and removed.
	p2, _ := s.TempPath()
	os.WriteFile(p2, make([]byte, 101), 0o600)
	if _, err := s.Adopt(p2, Meta{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over cap: %v", err)
	}
	if _, err := os.Stat(p2); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("over-cap temp file kept")
	}
}
