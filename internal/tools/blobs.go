package tools

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/blob"
	"github.com/smnhffmnn/mux/internal/config"
)

// Files between the agent and mux go through the blob store (internal/blob)
// whenever the two do not share a disk — and may go through it anyway. Two
// spellings carry that in tool arguments:
//
//   - blob:<id> where a tool takes a file or directory (file_path,
//     file_paths, images, env_file, path): the agent uploaded it first;
//   - output_file = "blob" (or output_dir) where a tool writes a file: the
//     result names a blob the agent fetches instead of a path on mux's disk.
//
// Without being asked, a tool stores as a blob what it would otherwise have
// written to mux's own output directory or cut off in silence: binary
// responses, oversized bodies, attachments.
//
// A blob a write consumed (a deploy, an upload, secrets) is deleted
// afterwards; a read (a dry run, a comparison) leaves it for the next call.

// blobStore is set once at startup. It stays nil when mux serves stdio
// without an HTTP listener — then no route could hand a blob back, and the
// helpers below say so instead of minting references nobody can fetch.
var blobStore *blob.Store

// SetBlobStore installs the process-wide store.
func SetBlobStore(s *blob.Store) { blobStore = s }

// BlobStore returns the installed store, or nil.
func BlobStore() *blob.Store { return blobStore }

// MountBlobRoutes mounts /blob on mux when a store is installed.
func MountBlobRoutes(mux *http.ServeMux) {
	if blobStore != nil {
		blobStore.Mount(mux)
	}
}

const (
	blobRefPrefix      = "blob:"
	blobOutputSentinel = "blob"
	// blobInlineImageMax is the size up to which an image blob is also
	// returned as image content, so the agent can look at it without a
	// round trip; the reference travels along either way.
	blobInlineImageMax = 5 << 20
)

// blobRef recognises the blob:<id> form. ok is false for anything else — a
// plain path keeps its own validation in the caller. An error means the
// argument meant a blob and cannot be honoured.
func blobRef(arg string) (id string, ok bool, err error) {
	trimmed := strings.TrimSpace(arg)
	if !strings.HasPrefix(strings.ToLower(trimmed), blobRefPrefix) {
		return "", false, nil
	}
	id = strings.TrimSpace(trimmed[len(blobRefPrefix):])
	if !blob.ValidID(id) {
		return "", true, fmt.Errorf("%q is not a blob reference: expected blob:<32 hex characters> as returned by POST /blob", arg)
	}
	if blobStore == nil {
		return "", true, errors.New("no blob store: this mux serves stdio without its HTTP listener, pass a local path instead")
	}
	return id, true, nil
}

// blobInputFile resolves a file argument. For blob:<id> it returns the path
// of the stored file and its description; for anything else the argument
// unchanged and nil, and the caller validates the path as before. what names
// the argument in errors.
func blobInputFile(arg, what string) (string, *blob.Info, error) {
	id, ok, err := blobRef(arg)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", what, err)
	}
	if !ok {
		return arg, nil, nil
	}
	p, info, err := blobStore.Path(id)
	if err != nil {
		return "", nil, fmt.Errorf("%s: blob %s not found — expired (blobs live %s), deleted, or never uploaded", what, id, blobStore.TTL())
	}
	if info.Kind != blob.KindFile {
		return "", nil, fmt.Errorf("%s: blob %s is a directory tree, not a file", what, id)
	}
	return p, info, nil
}

// blobInputTree resolves a directory argument the same way; the blob has to
// be a tree (an archive uploaded with ?tree=1).
func blobInputTree(arg, what string) (string, *blob.Info, error) {
	id, ok, err := blobRef(arg)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", what, err)
	}
	if !ok {
		return arg, nil, nil
	}
	p, info, err := blobStore.Path(id)
	if err != nil {
		return "", nil, fmt.Errorf("%s: blob %s not found — expired (blobs live %s), deleted, or never uploaded", what, id, blobStore.TTL())
	}
	if info.Kind != blob.KindTree {
		return "", nil, fmt.Errorf("%s: blob %s is a single file, not a directory — upload the directory as an archive: tar -C <root> -cz <dir> | curl --data-binary @- '%s?tree=1'",
			what, id, strings.TrimSuffix(blobStore.URL(""), "/"))
	}
	return p, info, nil
}

// consumeBlob deletes a blob a write has used up. nil is fine (a plain path).
func consumeBlob(info *blob.Info) {
	if info != nil && blobStore != nil {
		blobStore.Delete(info.ID)
	}
}

// blobDisplay is how an input blob appears in results instead of the store's
// private path.
func blobDisplay(info *blob.Info) string {
	if info == nil {
		return ""
	}
	return blob.Ref(info.ID)
}

// wantsBlobOutput reports whether an output_file/output_dir argument asks
// for a blob instead of a path.
func wantsBlobOutput(arg string) bool {
	return strings.EqualFold(strings.TrimSpace(arg), blobOutputSentinel)
}

// noBlobStoreResult is the answer when a blob is asked for and none exists.
func noBlobStoreResult(what string) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf("%s: no blob store — this mux serves stdio without its HTTP listener; pass an absolute path instead", what))
}

// storeResponseAsBlob streams a response body into a new blob and returns
// the reference. note is an extra line (what was uploaded, for instance).
// requireBody makes an empty body an error — right for audio a caller paid
// for, wrong for a POST that answered 201 with nothing to say. Without a
// store (a stdio instance) the body goes to mux's output directory instead,
// as it did before the store existed: stdio means the agent shares the disk.
func storeResponseAsBlob(resp *http.Response, contentType, name, source, note string, requireBody bool) (*mcp.CallToolResult, error) {
	if blobStore == nil {
		ext := filepath.Ext(name)
		if ext == "" {
			ext, _ = binaryResponseExt(contentType)
		}
		return saveResponseToGeneratedFile(resp, contentType, defaultOutputDir(), source+"-*"+ext, requireBody)
	}
	info, err := blobStore.Put(resp.Body, blob.Meta{Name: name, ContentType: contentType, Source: source})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("store response as blob: %v", err)), nil
	}
	if info.Size == 0 {
		blobStore.Delete(info.ID)
		if requireBody {
			return mcp.NewToolResultError(fmt.Sprintf("HTTP %d %s: empty response body, nothing stored", resp.StatusCode, resp.Status)), nil
		}
		result := fmt.Sprintf("HTTP %d %s\n\nEmpty response body, nothing stored", resp.StatusCode, resp.Status)
		if note != "" {
			result += "\n\n" + note
		}
		return mcp.NewToolResultText(result), nil
	}
	result := fmt.Sprintf("HTTP %d %s\n\nStored as %s\nContent-Type: %s\nSize: %d bytes",
		resp.StatusCode, resp.Status, blobStore.Reference(info), contentType, info.Size)
	if note != "" {
		result += "\n\n" + note
	}
	return mcp.NewToolResultText(result), nil
}

// defaultOutputDir is where a stdio instance without a store writes what it
// cannot inline — the directory mux used for that before the store existed.
func defaultOutputDir() string { return filepath.Join(config.Dir(), "output") }

// blobMaxBytes is the per-blob cap a tool that writes into a blobTempPath
// has to respect itself, 0 without a store.
func blobMaxBytes() int64 {
	if blobStore == nil {
		return 0
	}
	return blobStore.MaxBlob()
}

// storeBytesAsBlob stores bytes a tool already holds (a decoded attachment,
// a generated image).
func storeBytesAsBlob(data []byte, meta blob.Meta) (*blob.Info, error) {
	if blobStore == nil {
		return nil, errors.New("no blob store: this mux serves stdio without its HTTP listener")
	}
	return blobStore.Put(bytes.NewReader(data), meta)
}

// blobReference renders a stored blob for a result.
func blobReference(info *blob.Info) string {
	if blobStore == nil || info == nil {
		return ""
	}
	return blobStore.Reference(info)
}

// imageBlobResult returns an image both ways: as a blob reference in the
// text and, up to blobInlineImageMax, as image content to look at.
func imageBlobResult(meta string, data []byte, contentType string, info *blob.Info) *mcp.CallToolResult {
	text := meta
	if info != nil {
		text += "\n" + blobReference(info)
	}
	if int64(len(data)) <= blobInlineImageMax {
		return mcp.NewToolResultImage(text, base64.StdEncoding.EncodeToString(data), contentType)
	}
	return mcp.NewToolResultText(text)
}

// readBodyLimited reads up to max bytes of a response body to inline. A
// longer body is not cut off in silence: the whole of it goes into a blob,
// the first max bytes come back as the preview, and marker names the blob
// (MUX-35 — every connector used to truncate at its own limit and say
// nothing). Without a store the marker says how to get the rest.
func readBodyLimited(resp *http.Response, max int64, name, source string) (data []byte, marker string, err error) {
	data, err = io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) <= max {
		return data, "", nil
	}
	preview := data[:max]
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// An error body is for reading, not for keeping: it is cut, and
		// says so, but stores nothing.
		return preview, fmt.Sprintf("\n\n[error response truncated at %d bytes]", max), nil
	}
	info, err := spillBody(resp, data, name, source)
	switch {
	case blobStore == nil:
		return preview, fmt.Sprintf("\n\n[response truncated at %d bytes — pass output_file to write the full response to disk]", max), nil
	case err != nil:
		return preview, fmt.Sprintf("\n\n[response truncated at %d bytes; storing the rest failed: %v]", max, err), nil
	}
	return preview, fmt.Sprintf("\n\n[response truncated at %d bytes — the full body (%s) is %s]", max, blob.FormatSize(info.Size), blobStore.Reference(info)), nil
}

// spillBody stores head plus the rest of resp.Body as one blob.
func spillBody(resp *http.Response, head []byte, name, source string) (*blob.Info, error) {
	if blobStore == nil {
		return nil, errors.New("no blob store")
	}
	w, err := blobStore.Create(blob.Meta{Name: name, ContentType: resp.Header.Get("Content-Type"), Source: source})
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(head); err != nil {
		w.Abort()
		return nil, err
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		w.Abort()
		return nil, err
	}
	return w.Close()
}

// blobTempPath hands a tool that writes a file itself a destination inside
// the store, to be adopted with blobAdopt afterwards.
func blobTempPath(what string) (string, error) {
	if blobStore == nil {
		return "", fmt.Errorf("%s: no blob store — this mux serves stdio without its HTTP listener; pass an absolute path instead", what)
	}
	return blobStore.TempPath()
}

// blobAdopt turns a file written to a blobTempPath into a blob.
func blobAdopt(path string, meta blob.Meta) (*blob.Info, error) {
	if blobStore == nil {
		return nil, errors.New("no blob store")
	}
	return blobStore.Adopt(path, meta)
}
