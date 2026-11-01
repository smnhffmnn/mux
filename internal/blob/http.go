package blob

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The routes sit on mux's loopback listener next to /mcp. They have no token
// of their own: reaching the listener is the credential, as for the MCP
// endpoint. Two things narrow that further, because a browser on the same
// machine reaches loopback too: every request must name a loopback Host, and
// a request a browser marks as coming from another site (Origin,
// Sec-Fetch-Site) is refused — /mcp gets the same protection from the CORS
// preflight its JSON-only Content-Type forces, /blob accepts any body and
// would not. curl sends neither header and is unaffected. And every GET is
// served as an attachment with sniffing off and scripts sandboxed, so a blob
// that holds HTML or SVG from some remote API never runs on this origin.
//
//	POST   /blob            store the request body (or the file part of a
//	                        multipart form); ?tree=1 unpacks an archive
//	GET    /blob            list live blobs
//	GET    /blob/{id}       the bytes; a tree comes as tar.gz, ?list=1 lists it
//	DELETE /blob/{id}       delete

// Mount registers the routes on mux.
func (s *Store) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /blob", s.guard(s.handleList))
	mux.HandleFunc("POST /blob", s.guard(s.handleUpload))
	mux.HandleFunc("GET /blob/{id}", s.guard(s.handleGet))
	mux.HandleFunc("DELETE /blob/{id}", s.guard(s.handleDelete))
}

// SetBaseURL names how the agent reaches the listener the routes are mounted
// on ("http://127.0.0.1:7700"); references in tool results carry it.
func (s *Store) SetBaseURL(u string) { s.baseURL = strings.TrimRight(u, "/") }

// URL is where GET fetches the blob.
func (s *Store) URL(id string) string {
	base := s.baseURL
	if base == "" {
		base = "http://127.0.0.1:7700"
	}
	return base + "/blob/" + id
}

// Ref is the argument form that hands a blob to a tool.
func Ref(id string) string { return "blob:" + id }

// Reference renders a blob for a tool result: what it is, how long it lives,
// how to fetch it and how to pass it on. Never silent, never the bytes — and
// never the name inside the command: names come from mail senders, issue
// trackers and remote hosts, and a name in a shell line is a shell line.
func (s *Store) Reference(info *Info) string {
	expires := info.Expires.UTC().Format("2006-01-02 15:04 UTC")
	name := ""
	if info.Name != "" {
		name = fmt.Sprintf(", name %s", strconv.Quote(info.Name))
	}
	switch info.Kind {
	case KindTree:
		return fmt.Sprintf("blob %s (tree: %d files, %s%s, expires %s) — fetch as tar.gz: curl -sS %s | tar -xz -C DIR; pass on as %s",
			info.ID, info.Files, FormatSize(info.Size), name, expires, s.URL(info.ID), Ref(info.ID))
	default:
		ct := info.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		return fmt.Sprintf("blob %s (%s, %s%s, expires %s) — fetch: curl -sS -o FILE %s; pass on as %s",
			info.ID, ct, FormatSize(info.Size), name, expires, s.URL(info.ID), Ref(info.ID))
	}
}

// Instructions is the paragraph about files that goes into the MCP server
// instructions, so an agent learns the mechanics before its first tool call.
func (s *Store) Instructions() string {
	base := strings.TrimSuffix(s.URL(""), "/blob/")
	return fmt.Sprintf("Files: mux does not read files from your machine and does not write files to it. "+
		"To hand a file to a tool, upload it first — curl -sS --data-binary @FILE -H 'Content-Type: TYPE' -H 'X-Blob-Name: NAME' %s/blob — "+
		"and pass the returned id as blob:<id> wherever a tool takes a file (file_path, file_paths, images, env_file) or a project directory (the Supabase path). "+
		"A tool that needs a directory (supabase function_deploy, migrations_*) takes a tree: "+
		"tar -C PROJECT_ROOT -cz supabase | curl -sS --data-binary @- -H 'Content-Type: application/gzip' '%s/blob?tree=1'. "+
		"A result that carries a file names a blob instead of a path: fetch it with curl -sS -o FILE %s/blob/<id> (a tree arrives as tar.gz); choose FILE yourself, the result shows the original name separately. "+
		"Pass output_file=\"blob\" (or output_dir=\"blob\") to receive a result as a blob on purpose. "+
		"Blobs live %s; a blob a write used (deploy, upload, secrets) is deleted afterwards. "+
		"Where you and mux share a disk, absolute paths still work as before.",
		base, base, base, FormatDuration(s.ttl))
}

// FormatDuration renders a duration the way a person writes it: 1h, 30m, 90s.
func FormatDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

type listResponse struct {
	Blobs    []Info `json:"blobs"`
	Count    int    `json:"count"`
	Bytes    int64  `json:"bytes"`
	MaxBlob  int64  `json:"maxBlob"`
	MaxTotal int64  `json:"maxTotal"`
	TTL      string `json:"ttl"`
	BaseURL  string `json:"baseUrl"`
}

type blobResponse struct {
	Info
	URL string `json:"url"`
	Ref string `json:"ref"`
}

type treeResponse struct {
	blobResponse
	Entries []TreeEntry `json:"files"`
}

// guard refuses requests that did not come from this machine's own tools:
// a Host that is not loopback (DNS rebinding), a browser's cross-site marker.
func (s *Store) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, "the blob store answers loopback requests only")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !loopbackOrigin(origin) {
			writeError(w, http.StatusForbidden, "cross-site requests are refused")
			return
		}
		switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
		case "", "same-origin", "none":
		default:
			writeError(w, http.StatusForbidden, "cross-site requests are refused")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next(w, r)
	}
}

// loopbackHost accepts 127.0.0.1, ::1 and localhost, with or without a port.
func loopbackHost(host string) bool {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func loopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return loopbackHost(u.Host)
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	list := s.List()
	count, total := s.Stats()
	writeJSON(w, http.StatusOK, listResponse{
		Blobs: list, Count: count, Bytes: total,
		MaxBlob: s.maxBlob, MaxTotal: s.maxTotal, TTL: FormatDuration(s.ttl), BaseURL: strings.TrimSuffix(s.URL(""), "/blob/"),
	})
}

func (s *Store) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tree := isTrue(q.Get("tree")) || strings.EqualFold(r.Header.Get("X-Blob-Kind"), "tree")
	meta := Meta{Name: cleanName(firstNonEmpty(q.Get("name"), r.Header.Get("X-Blob-Name"))), Source: "upload"}

	// One byte past the cap is enough to know the upload is too large; the
	// store refuses at the cap itself. (A multipart body is read by the
	// multipart reader directly; the file part is still capped by the store.)
	body := io.Reader(http.MaxBytesReader(w, r.Body, s.maxBlob+1))
	contentType := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "multipart form: "+err.Error())
			return
		}
		var part io.Reader
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, "multipart form: "+err.Error())
				return
			}
			if p.FileName() == "" {
				// A text field: nothing to store. The file part is what counts.
				continue
			}
			if meta.Name == "" {
				meta.Name = cleanName(p.FileName())
			}
			contentType = p.Header.Get("Content-Type")
			part = p
			break
		}
		if part == nil {
			writeError(w, http.StatusBadRequest, "multipart form without a file part")
			return
		}
		body = part
	}
	meta.ContentType = normalizeContentType(contentType)

	var info *Info
	var err error
	if tree {
		info, err = s.PutTree(body, meta)
	} else {
		info, err = s.Put(body, meta)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.Is(err, ErrTooLarge), errors.As(err, &mbe):
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("blob too large: at most %d bytes (%s)", s.maxBlob, FormatSize(s.maxBlob)))
		case errors.Is(err, ErrTooManyFiles):
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("tree too large: at most %d entries (files, directories and links together)", maxTreeFiles))
		case errors.Is(err, ErrFormat):
			writeError(w, http.StatusUnsupportedMediaType, "tree upload: "+err.Error())
		case errors.Is(err, ErrArchive):
			writeError(w, http.StatusBadRequest, "tree upload: "+err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "store: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, s.describe(info))
}

func (s *Store) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidID(id) {
		writeError(w, http.StatusBadRequest, "not a blob id")
		return
	}
	info, err := s.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such blob (expired, deleted or never stored)")
		return
	}
	// Served as a download whatever it holds: the bytes came from some
	// remote API and must never render as a page on this origin.
	w.Header().Set("Content-Security-Policy", "sandbox")
	if info.Kind == KindTree {
		if isTrue(r.URL.Query().Get("list")) {
			entries, _, err := s.Entries(id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, treeResponse{blobResponse: s.describe(info), Entries: entries})
			return
		}
		name := info.Name
		if name == "" {
			name = id
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".tar.gz"}))
		w.Header().Set("X-Blob-Files", strconv.Itoa(info.Files))
		w.WriteHeader(http.StatusOK)
		if err := s.Archive(id, w); err != nil {
			// Headers are out: abort the connection so the client sees a
			// failed transfer, not a short archive that unpacks clean.
			panic(http.ErrAbortHandler)
		}
		return
	}
	f, info, err := s.Open(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such blob")
		return
	}
	defer f.Close()
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	name := info.Name
	if name == "" {
		name = id + extensionFor(ct)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Blob-Expires", info.Expires.UTC().Format(time.RFC3339))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}

func (s *Store) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !ValidID(id) {
		writeError(w, http.StatusBadRequest, "not a blob id")
		return
	}
	s.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) describe(info *Info) blobResponse {
	return blobResponse{Info: *info, URL: s.URL(info.ID), Ref: Ref(info.ID)}
}

// cleanName keeps a name hint to a base name without control characters: it
// is shown, quoted, in results and used as the download's file name.
func cleanName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "." || name == "/" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
}

// normalizeContentType turns the types that mean "nothing declared" into
// octet-stream: an absent header, a multipart container type, and curl's
// default for --data-binary, application/x-www-form-urlencoded.
func normalizeContentType(ct string) string {
	mediaType, _, _ := mime.ParseMediaType(ct)
	switch {
	case mediaType == "", strings.HasPrefix(mediaType, "multipart/"), mediaType == "application/x-www-form-urlencoded":
		return "application/octet-stream"
	}
	return ct
}

// extensionFor picks a file extension for a content type, for a blob that
// was stored without a name.
func extensionFor(ct string) string {
	mediaType, _, _ := mime.ParseMediaType(ct)
	if ext, ok := commonExtensions[mediaType]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// commonExtensions names the extension a person expects where the system's
// MIME table lists several (text/html → .ehtml first, sorted).
var commonExtensions = map[string]string{
	"application/json":         ".json",
	"application/pdf":          ".pdf",
	"application/zip":          ".zip",
	"application/gzip":         ".tar.gz",
	"application/octet-stream": ".bin",
	"text/plain":               ".txt",
	"text/html":                ".html",
	"text/csv":                 ".csv",
	"text/xml":                 ".xml",
	"application/xml":          ".xml",
	"image/jpeg":               ".jpg",
	"image/png":                ".png",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/svg+xml":            ".svg",
	"audio/mpeg":               ".mp3",
	"audio/wav":                ".wav",
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
