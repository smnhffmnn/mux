// Package blob is mux's temporary file store: the way bytes move between an
// agent and mux when the two do not share a disk.
//
// mux reads no file from the agent's machine and writes none there. A tool
// that needs a file takes a blob the agent uploaded (POST /blob); a tool that
// produces one stores it here and answers with a reference the agent fetches
// (GET /blob/{id}). On a host where agent and mux do share a disk, a local
// path still works as a convenience — the store is the way that works
// everywhere, a container included.
//
// Nothing here persists: the directory is emptied at startup, every blob
// carries an expiry, and the total size is capped (the oldest goes first).
// An id is 128 random bits; knowing it is the capability — the store hangs
// off the loopback listener next to /mcp and has no authentication of its
// own, the same trust boundary as the MCP endpoint.
package blob

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Kind tells a single file from an unpacked directory tree.
type Kind string

const (
	KindFile Kind = "file"
	KindTree Kind = "tree"
)

// Default limits. A blob is temporary: an hour covers a tool call and its
// retry; the size caps protect the disk of a container whose state volume is
// not sized for an agent's downloads.
const (
	DefaultTTL      = time.Hour
	DefaultMaxTotal = 1 << 30 // 1 GiB on disk, all blobs together
	DefaultMaxBlob  = 256 << 20
)

// maxTreeFiles caps the entries of one tree — files, directories and skipped
// entries together. A variable so a test can lower it.
var maxTreeFiles = 50_000

// Info describes one blob. Size is bytes on disk: the file, or the files of
// a tree together. Expires is absolute; the store deletes the blob at that
// time whether or not anything still refers to it.
type Info struct {
	ID          string    `json:"id"`
	Kind        Kind      `json:"kind"`
	Name        string    `json:"name,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	Size        int64     `json:"size"`
	Format      Format    `json:"format,omitempty"`   // trees: the archive the upload came as
	Files       int       `json:"files,omitempty"`    // trees: regular files unpacked
	Skipped     int       `json:"skipped,omitempty"`  // trees: links, special files and macOS sidecars left out
	TopLevel    []string  `json:"topLevel,omitempty"` // trees: the entries at the root
	Source      string    `json:"source,omitempty"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
}

// Meta is what a caller knows about the bytes it stores.
type Meta struct {
	Name        string // file name hint; a tree's root label
	ContentType string // files only
	Source      string // "upload" for POST /blob, otherwise the tool that made it
}

// Options configure a store. Zero values take the defaults above.
type Options struct {
	TTL      time.Duration
	MaxTotal int64
	MaxBlob  int64
	BaseURL  string // how the agent reaches the listener, for references
	Now      func() time.Time
}

// Store holds the blobs of one mux process.
type Store struct {
	dir      string
	ttl      time.Duration
	maxTotal int64
	maxBlob  int64
	now      func() time.Time

	baseURL string

	// mu guards blobs and total. File-system work for a removal happens
	// outside it: removeLocked only renames the entry away under the lock
	// (so no lookup can see it any more), the caller deletes it afterwards.
	mu    sync.Mutex
	blobs map[string]*Info
	total int64
}

// ErrNotFound is returned for an unknown or expired id.
var ErrNotFound = errors.New("blob not found")

// ErrTooLarge is returned when a blob would exceed the per-blob cap.
var ErrTooLarge = errors.New("blob too large")

// ErrTooManyFiles is returned when a tree would exceed the entry cap.
var ErrTooManyFiles = errors.New("too many entries in the tree")

// ErrArchive is returned for an archive that cannot be accepted as a tree
// for reasons of its content: an entry that would land outside the tree, a
// duplicate name, no regular file at all.
var ErrArchive = errors.New("archive not accepted")

// Open prepares the store under root. Each process gets its own directory,
// root/<pid>: ids are random and die with the process, so a leftover blob is
// one nobody can name any more — but a second mux started while the first
// runs (a terminal next to the tray app, another port) must not wipe the
// running instance's blobs, which a shared directory would. Directories of
// processes that are gone are removed here; the live ones stay.
func Open(root string, opts Options) (*Store, error) {
	if root == "" {
		return nil, errors.New("blob store: directory is required")
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("blob store: directory must be absolute, got %s", root)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("blob store: create %s: %w", root, err)
	}
	reapStaleDirs(root)
	dir := filepath.Join(root, strconv.Itoa(os.Getpid()))
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("blob store: clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("blob store: create %s: %w", dir, err)
	}
	s := &Store{
		dir:      dir,
		ttl:      opts.TTL,
		maxTotal: opts.MaxTotal,
		maxBlob:  opts.MaxBlob,
		now:      opts.Now,
		baseURL:  strings.TrimRight(opts.BaseURL, "/"),
		blobs:    map[string]*Info{},
	}
	if s.ttl <= 0 {
		s.ttl = DefaultTTL
	}
	if s.maxTotal <= 0 {
		s.maxTotal = DefaultMaxTotal
	}
	if s.maxBlob <= 0 {
		s.maxBlob = DefaultMaxBlob
	}
	if s.maxBlob > s.maxTotal {
		s.maxBlob = s.maxTotal
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// reapStaleDirs removes the per-process directories under root whose process
// no longer exists. A directory whose name is not a pid is left alone.
func reapStaleDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		if !processAlive(pid) {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// processAlive reports whether a process with this pid exists. On Unix a
// signal 0 asks without delivering anything (EPERM still means "exists"); on
// Windows FindProcess opens a handle and fails when there is none.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// Close removes the store's directory. Blobs are temporary by contract; a
// normal shutdown should not leave an uploaded .env on disk until the next
// start reaps it.
func (s *Store) Close() error {
	s.mu.Lock()
	s.blobs = map[string]*Info{}
	s.total = 0
	s.mu.Unlock()
	return os.RemoveAll(s.dir)
}

// Dir is the store's directory on disk.
func (s *Store) Dir() string { return s.dir }

// TTL is the lifetime of a new blob.
func (s *Store) TTL() time.Duration { return s.ttl }

// MaxBlob is the cap for one blob.
func (s *Store) MaxBlob() int64 { return s.maxBlob }

// MaxTotal is the cap for all blobs together.
func (s *Store) MaxTotal() int64 { return s.maxTotal }

// Writer receives the bytes of a new file blob. Close commits it; Abort
// discards it. One of the two must be called.
type Writer struct {
	s    *Store
	id   string
	meta Meta
	f    *os.File
	n    int64
	done bool
}

// Create starts a new file blob whose bytes the caller streams in. The blob
// exists for others only after Close.
func (s *Store) Create(meta Meta) (*Writer, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.filePath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("blob store: create: %w", err)
	}
	return &Writer{s: s, id: id, meta: meta, f: f}, nil
}

// Write stores bytes, refusing to grow past the per-blob cap.
func (w *Writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, errors.New("blob writer is closed")
	}
	if w.n+int64(len(p)) > w.s.maxBlob {
		return 0, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, w.s.maxBlob)
	}
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

// Close commits the blob and returns its description.
func (w *Writer) Close() (*Info, error) {
	if w.done {
		return nil, errors.New("blob writer is closed")
	}
	w.done = true
	if err := w.f.Close(); err != nil {
		os.Remove(w.f.Name())
		return nil, fmt.Errorf("blob store: write: %w", err)
	}
	info := &Info{
		ID:          w.id,
		Kind:        KindFile,
		Name:        w.meta.Name,
		ContentType: w.meta.ContentType,
		Size:        w.n,
		Source:      w.meta.Source,
	}
	if err := w.s.commit(info); err != nil {
		os.Remove(w.f.Name())
		return nil, err
	}
	return info, nil
}

// Abort discards a blob that will not be committed.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.f.Close()
	os.Remove(w.f.Name())
}

// Put stores everything r yields as a file blob.
func (s *Store) Put(r io.Reader, meta Meta) (*Info, error) {
	w, err := s.Create(meta)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Abort()
		return nil, err
	}
	return w.Close()
}

// commit registers a written blob, making room by evicting the oldest ones
// when the total would exceed the cap.
func (s *Store) commit(info *Info) error {
	now := s.now()
	info.Created = now
	info.Expires = now.Add(s.ttl)
	var gone []string
	defer func() { reap(gone) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	gone = s.expireLocked(now)
	if info.Size > s.maxTotal {
		return fmt.Errorf("%w: %d bytes against a store cap of %d", ErrTooLarge, info.Size, s.maxTotal)
	}
	for s.total+info.Size > s.maxTotal {
		oldest := s.oldestLocked()
		if oldest == nil {
			break
		}
		gone = append(gone, s.removeLocked(oldest))
	}
	s.blobs[info.ID] = info
	s.total += info.Size
	return nil
}

// Get returns the description of a live blob.
func (s *Store) Get(id string) (*Info, error) {
	s.mu.Lock()
	gone := s.expireLocked(s.now())
	info, ok := s.blobs[id]
	var c Info
	if ok {
		c = *info
	}
	s.mu.Unlock()
	reap(gone)
	if !ok {
		return nil, ErrNotFound
	}
	return &c, nil
}

// Path returns where a blob lives on this machine: the file of a file blob,
// the root directory of a tree. Tools that take a path open it from here; the
// path is valid until the blob expires or is deleted.
func (s *Store) Path(id string) (string, *Info, error) {
	info, err := s.Get(id)
	if err != nil {
		return "", nil, err
	}
	if info.Kind == KindTree {
		return s.treePath(id), info, nil
	}
	return s.filePath(id), info, nil
}

// Open returns the bytes of a file blob.
func (s *Store) Open(id string) (io.ReadCloser, *Info, error) {
	info, err := s.Get(id)
	if err != nil {
		return nil, nil, err
	}
	if info.Kind != KindFile {
		return nil, nil, fmt.Errorf("blob %s is a tree, not a file — fetch it as an archive", id)
	}
	f, err := os.Open(s.filePath(id))
	if err != nil {
		return nil, nil, fmt.Errorf("blob store: open: %w", err)
	}
	return f, info, nil
}

// Delete removes a blob. Deleting an unknown id is not an error: the caller
// wanted it gone, and it is.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	var gone []string
	if info, ok := s.blobs[id]; ok {
		gone = append(gone, s.removeLocked(info))
	}
	s.mu.Unlock()
	reap(gone)
}

// List returns every live blob, newest first.
func (s *Store) List() []Info {
	s.mu.Lock()
	gone := s.expireLocked(s.now())
	out := make([]Info, 0, len(s.blobs))
	for _, info := range s.blobs {
		out = append(out, *info)
	}
	s.mu.Unlock()
	reap(gone)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].ID < out[j].ID
		}
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// Stats reports how many blobs are live and how many bytes they hold.
func (s *Store) Stats() (count int, bytes int64) {
	s.mu.Lock()
	gone := s.expireLocked(s.now())
	count, bytes = len(s.blobs), s.total
	s.mu.Unlock()
	reap(gone)
	return count, bytes
}

// Sweep deletes expired blobs. The store does this on every access too;
// Sweep exists for the ticker that keeps an idle store clean.
func (s *Store) Sweep() {
	s.mu.Lock()
	gone := s.expireLocked(s.now())
	s.mu.Unlock()
	reap(gone)
}

// Run sweeps every interval until ctx ends.
func (s *Store) Run(done <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.Sweep()
		}
	}
}

// expireLocked drops every blob past its expiry and returns the paths the
// caller deletes once the lock is released.
func (s *Store) expireLocked(now time.Time) []string {
	var gone []string
	for _, info := range s.blobs {
		if !now.Before(info.Expires) {
			gone = append(gone, s.removeLocked(info))
		}
	}
	return gone
}

func (s *Store) oldestLocked() *Info {
	var oldest *Info
	for _, info := range s.blobs {
		if oldest == nil || info.Created.Before(oldest.Created) || (info.Created.Equal(oldest.Created) && info.ID < oldest.ID) {
			oldest = info
		}
	}
	return oldest
}

// removeLocked forgets a blob and renames its file or tree out of the way, so
// that deleting it — slow for a tree of many files — can happen without the
// lock. It returns the renamed path; reap deletes it.
func (s *Store) removeLocked(info *Info) string {
	delete(s.blobs, info.ID)
	s.total -= info.Size
	var p string
	if info.Kind == KindTree {
		p = s.treePath(info.ID)
	} else {
		p = s.filePath(info.ID)
	}
	gone := filepath.Join(s.dir, "gone-"+info.ID)
	if err := os.Rename(p, gone); err != nil {
		// A file still open on Windows, or already gone: delete in place.
		return p
	}
	return gone
}

// reap deletes what removeLocked renamed away. Errors are not reported: the
// entry is already forgotten, and the next Open clears the directory.
func reap(paths []string) {
	for _, p := range paths {
		os.RemoveAll(p)
	}
}

func (s *Store) filePath(id string) string { return filepath.Join(s.dir, id) }
func (s *Store) treePath(id string) string { return filepath.Join(s.dir, id+".tree") }

// newID returns 128 random bits as 32 hex characters.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("blob store: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidID reports whether s has the shape of a blob id. Used before any
// lookup so a path argument like "blob:../x" never reaches the file system.
func ValidID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// FormatSize renders a byte count the way a person reads it.
func FormatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// TempPath returns a fresh path inside the store for a tool that writes a
// file itself (a command's stdout, an SFTP download) and adopts it afterwards.
// Nothing is created; a file left there dies with the next Open.
func (s *Store) TempPath() (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir, "tmp-"+id), nil
}

// Adopt turns a file written to a TempPath into a blob by renaming it — no
// copy, so a large download costs nothing extra.
func (s *Store) Adopt(path string, meta Meta) (*Info, error) {
	if filepath.Dir(path) != s.dir || !strings.HasPrefix(filepath.Base(path), "tmp-") {
		return nil, fmt.Errorf("blob store: %s is not a path from TempPath", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("blob store: adopt: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("blob store: adopt: %s is not a regular file", path)
	}
	if fi.Size() > s.maxBlob {
		os.Remove(path)
		return nil, fmt.Errorf("%w: %d bytes against a cap of %d", ErrTooLarge, fi.Size(), s.maxBlob)
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	if err := os.Rename(path, s.filePath(id)); err != nil {
		return nil, fmt.Errorf("blob store: adopt: %w", err)
	}
	info := &Info{ID: id, Kind: KindFile, Name: meta.Name, ContentType: meta.ContentType, Size: fi.Size(), Source: meta.Source}
	if err := s.commit(info); err != nil {
		os.Remove(s.filePath(id))
		return nil, err
	}
	return info, nil
}
