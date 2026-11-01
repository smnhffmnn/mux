package blob

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A tree blob is a directory: an archive the agent uploaded, unpacked once
// into the store so a tool that walks files (the Supabase deploy, the
// migrations) finds them under a plain path. Accepted are tar, tar.gz and
// zip, told apart by their first bytes, not by a header the caller sets.
//
// Unpacking is where an archive from the agent's shell meets mux's disk, so
// the rules are strict rather than forgiving: an entry whose name would leave
// the tree (absolute, or with a ".." segment) fails the whole upload instead
// of being remapped; symbolic and hard links, devices and FIFOs are skipped
// and counted — a link could point anywhere on the host; the bytes written
// are capped like a single blob, and every entry — files, directories and
// the skipped ones alike — counts against maxTreeFiles, so an archive of a
// million empty directories is refused like one of a million files.

// TreeEntry is one file of a tree.
type TreeEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Format is the archive container of a tree upload.
type Format string

const (
	FormatTar   Format = "tar"
	FormatTarGz Format = "tar.gz"
	FormatZip   Format = "zip"
)

// ErrFormat is returned when an upload marked as a tree is none of the
// accepted archives.
var ErrFormat = errors.New("not a tar, tar.gz or zip archive")

// PutTree stores an archive as a tree blob. r is read in full first (a zip
// needs random access), then unpacked.
func (s *Store) PutTree(r io.Reader, meta Meta) (*Info, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	// Spool the archive next to its destination so a half-received upload
	// never lingers under the id of a live blob.
	spool, err := os.CreateTemp(s.dir, "upload-*")
	if err != nil {
		return nil, fmt.Errorf("blob store: spool: %w", err)
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	n, err := io.Copy(spool, io.LimitReader(r, s.maxBlob+1))
	if err != nil {
		return nil, fmt.Errorf("blob store: receive archive: %w", err)
	}
	if n > s.maxBlob {
		return nil, fmt.Errorf("%w: the archive is larger than %d bytes", ErrTooLarge, s.maxBlob)
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: the upload is empty", ErrArchive)
	}

	format, err := detectFormat(spool)
	if err != nil {
		return nil, err
	}
	dir := s.treePath(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("blob store: create tree: %w", err)
	}
	u := &unpacker{root: dir, budget: s.maxBlob}
	switch format {
	case FormatZip:
		err = u.zip(spool, n)
	case FormatTarGz:
		if _, serr := spool.Seek(0, io.SeekStart); serr != nil {
			err = serr
		} else {
			var gz *gzip.Reader
			gz, err = gzip.NewReader(spool)
			if err == nil {
				err = u.tar(tar.NewReader(gz))
			}
		}
	default:
		if _, serr := spool.Seek(0, io.SeekStart); serr != nil {
			err = serr
		} else {
			err = u.tar(tar.NewReader(spool))
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if u.files == 0 {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("%w: the archive holds no regular files", ErrArchive)
	}
	info := &Info{
		ID:       id,
		Kind:     KindTree,
		Name:     meta.Name,
		Format:   format,
		Size:     u.written,
		Files:    u.files,
		Skipped:  u.skipped,
		TopLevel: u.topLevel(),
		Source:   meta.Source,
	}
	if err := s.commit(info); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return info, nil
}

// detectFormat reads the first bytes of f (rewinding afterwards) and names
// the archive, or fails with ErrFormat.
func detectFormat(f *os.File) (Format, error) {
	head := make([]byte, 512)
	n, err := f.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("blob store: read archive: %w", err)
	}
	head = head[:n]
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return FormatTarGz, nil
	case len(head) >= 4 && bytes.Equal(head[:4], []byte("PK\x03\x04")):
		return FormatZip, nil
	case len(head) >= 4 && bytes.Equal(head[:4], []byte("PK\x05\x06")):
		// An empty zip: no files is an error further down, but it is a zip.
		return FormatZip, nil
	case len(head) >= 263 && string(head[257:262]) == "ustar":
		return FormatTar, nil
	}
	// A tar without the ustar magic (old v7 archives) still parses: try
	// one header before giving up.
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if _, err := tar.NewReader(f).Next(); err == nil {
			return FormatTar, nil
		}
	}
	return "", ErrFormat
}

type unpacker struct {
	root    string
	budget  int64
	written int64
	files   int
	skipped int
	entries int // files, directories and skipped entries together
	dirs    map[string]bool
	tops    map[string]bool
}

// count charges one archive entry against the entry cap.
func (u *unpacker) count() error {
	u.entries++
	if u.entries > maxTreeFiles {
		return fmt.Errorf("%w: more than %d", ErrTooManyFiles, maxTreeFiles)
	}
	return nil
}

// safeName validates an archive entry name and returns it cleaned and
// relative, or an error for a name that would land outside the tree.
func safeName(name string) (string, error) {
	n := strings.ReplaceAll(name, "\\", "/")
	if strings.ContainsRune(n, 0) {
		return "", fmt.Errorf("%w: an entry name contains a NUL byte", ErrArchive)
	}
	if strings.HasPrefix(n, "/") || isDriveLetter(n) {
		return "", fmt.Errorf("%w: entry %q is an absolute path — archive the directory with relative names (tar -C <root> …)", ErrArchive, name)
	}
	// Checked before Clean: "a/../x" would clean to "x" and look harmless,
	// but an archive that spells names that way is not one to trust.
	for _, seg := range strings.Split(n, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: entry %q escapes the archive root", ErrArchive, name)
		}
	}
	clean := path.Clean(n)
	if clean == "." || clean == "" {
		return "", nil
	}
	// IsLocal adds the host's own rules: reserved device names and trailing
	// dots or spaces on Windows, a NUL anywhere.
	if !filepath.IsLocal(filepath.FromSlash(clean)) {
		return "", fmt.Errorf("%w: entry %q is not a local name on this system", ErrArchive, name)
	}
	return clean, nil
}

// isDriveLetter reports the Windows absolute form "C:/…" (or a bare "C:");
// "1:2.sql" and "x:y" are ordinary Unix names, and on Windows IsLocal refuses
// them anyway.
func isDriveLetter(n string) bool {
	if len(n) < 2 || n[1] != ':' {
		return false
	}
	letter := (n[0] >= 'A' && n[0] <= 'Z') || (n[0] >= 'a' && n[0] <= 'z')
	return letter && (len(n) == 2 || n[2] == '/')
}

// sidecar reports the metadata files macOS tools slip into archives: the
// AppleDouble "._name" twins bsdtar writes for extended attributes and the
// __MACOSX folder of a Finder zip. They are not part of any project and
// would only confuse a tool that lists or walks the tree.
func sidecar(rel string) bool {
	return strings.HasPrefix(path.Base(rel), "._") || rel == "__MACOSX" || strings.HasPrefix(rel, "__MACOSX/")
}

func (u *unpacker) mkdir(rel string) error {
	if rel == "" || rel == "." {
		return nil
	}
	if u.dirs == nil {
		u.dirs = map[string]bool{}
	}
	if u.dirs[rel] {
		return nil
	}
	if err := u.count(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(u.root, filepath.FromSlash(rel)), 0o700); err != nil {
		return fmt.Errorf("blob store: unpack: %w", err)
	}
	u.dirs[rel] = true
	u.noteTop(rel)
	return nil
}

// skip counts an entry that is not materialised.
func (u *unpacker) skip() error {
	u.skipped++
	return u.count()
}

func (u *unpacker) noteTop(rel string) {
	if u.tops == nil {
		u.tops = map[string]bool{}
	}
	top, _, _ := strings.Cut(rel, "/")
	u.tops[top] = true
}

// topLevel lists the root entries, at most maxTopLevel of them — the list
// travels in every listing and error, and a tree of a thousand root entries
// is described well enough by the first few.
func (u *unpacker) topLevel() []string {
	out := make([]string, 0, len(u.tops))
	for t := range u.tops {
		out = append(out, t)
	}
	sort.Strings(out)
	if len(out) > maxTopLevel {
		out = append(out[:maxTopLevel], fmt.Sprintf("… %d more", len(out)-maxTopLevel))
	}
	return out
}

const maxTopLevel = 16

// file writes one regular file of size (when known, -1 otherwise) from r.
func (u *unpacker) file(rel string, r io.Reader) error {
	if err := u.count(); err != nil {
		return err
	}
	if err := u.mkdir(path.Dir(rel)); err != nil {
		return err
	}
	u.noteTop(rel)
	dest := filepath.Join(u.root, filepath.FromSlash(rel))
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: it lists %q twice (or two names that differ only in case)", ErrArchive, rel)
		}
		return fmt.Errorf("blob store: unpack: %w", err)
	}
	remaining := u.budget - u.written
	n, err := io.Copy(f, io.LimitReader(r, remaining+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("blob store: unpack %s: %w", rel, err)
	}
	if n > remaining {
		return fmt.Errorf("%w: the unpacked tree is larger than %d bytes", ErrTooLarge, u.budget)
	}
	u.written += n
	u.files++
	return nil
}

func (u *unpacker) tar(tr *tar.Reader) error {
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFormat, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			// git archive's pax_global_header: metadata, not an entry.
			continue
		}
		rel, err := safeName(hdr.Name)
		if err != nil {
			return err
		}
		if sidecar(rel) {
			if err := u.skip(); err != nil {
				return err
			}
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := u.mkdir(rel); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if rel == "" {
				return fmt.Errorf("%w: a file without a name", ErrArchive)
			}
			if err := u.file(rel, tr); err != nil {
				return err
			}
		default:
			// Links, devices, FIFOs: never materialised.
			if err := u.skip(); err != nil {
				return err
			}
		}
	}
}

func (u *unpacker) zip(f *os.File, size int64) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFormat, err)
	}
	// The central directory is in memory already; refuse a bomb before
	// touching the disk for it.
	if len(zr.File) > maxTreeFiles {
		return fmt.Errorf("%w: %d entries, at most %d", ErrTooManyFiles, len(zr.File), maxTreeFiles)
	}
	for _, zf := range zr.File {
		rel, err := safeName(zf.Name)
		if err != nil {
			return err
		}
		if sidecar(rel) {
			if err := u.skip(); err != nil {
				return err
			}
			continue
		}
		mode := zf.Mode()
		switch {
		case zf.FileInfo().IsDir():
			if err := u.mkdir(rel); err != nil {
				return err
			}
		case !mode.IsRegular():
			if err := u.skip(); err != nil {
				return err
			}
		default:
			if rel == "" {
				return fmt.Errorf("%w: a file without a name", ErrArchive)
			}
			rc, err := zf.Open()
			if err != nil {
				return fmt.Errorf("%w: %v", ErrFormat, err)
			}
			err = u.file(rel, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Entries lists the files of a tree blob, sorted by path.
func (s *Store) Entries(id string) ([]TreeEntry, *Info, error) {
	info, err := s.Get(id)
	if err != nil {
		return nil, nil, err
	}
	if info.Kind != KindTree {
		return nil, nil, fmt.Errorf("blob %s is a file, not a tree", id)
	}
	root := s.treePath(id)
	var out []TreeEntry
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, TreeEntry{Path: filepath.ToSlash(rel), Size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("blob store: list tree: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, info, nil
}

// Archive writes a tree blob as tar.gz to w — the form GET /blob/{id}
// returns for a tree, and what a tool that produced a directory hands back.
func (s *Store) Archive(id string, w io.Writer) error {
	entries, _, err := s.Entries(id)
	if err != nil {
		return err
	}
	root := s.treePath(id)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		p := filepath.Join(root, filepath.FromSlash(e.Path))
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = e.Path
		hdr.Mode = 0o644
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// PutDir stores an existing directory as a tree blob by copying its regular
// files — the way a tool that wrote files hands them back as one blob.
func (s *Store) PutDir(dir string, meta Meta) (*Info, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	dest := s.treePath(id)
	if err := os.Mkdir(dest, 0o700); err != nil {
		return nil, fmt.Errorf("blob store: create tree: %w", err)
	}
	u := &unpacker{root: dest, budget: s.maxBlob}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if sidecar(rel) {
			if err := u.skip(); err != nil {
				return err
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return u.mkdir(rel)
		}
		if !d.Type().IsRegular() {
			return u.skip()
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		return u.file(rel, f)
	})
	if err != nil {
		os.RemoveAll(dest)
		return nil, err
	}
	info := &Info{
		ID: id, Kind: KindTree, Name: meta.Name, Format: FormatTarGz,
		Size: u.written, Files: u.files, Skipped: u.skipped, TopLevel: u.topLevel(), Source: meta.Source,
	}
	if err := s.commit(info); err != nil {
		os.RemoveAll(dest)
		return nil, err
	}
	return info, nil
}
