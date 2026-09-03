package tools

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// validateOutputFile expands and checks an output_file argument, returning the
// path to write to. Callers use it *before* firing a request whose result is
// expensive to reproduce — a paid generation must not be thrown away because
// the destination was unusable all along.
//
// The same checks run again in saveResponseToFile, which is the authority:
// O_EXCL there decides the race, this is only the early exit.
func validateOutputFile(outputFile string) (string, error) {
	clean := filepath.Clean(config.ExpandHome(outputFile))
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("output_file must be an absolute path: %s", outputFile)
	}
	if _, err := os.Stat(clean); err == nil {
		return "", fmt.Errorf("output_file already exists (refusing to overwrite): %s", clean)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("output_file is not usable: %w", err)
	}
	return clean, nil
}

// saveResponseToFile streams an HTTP response body to a file and returns metadata.
// Used by connectors when output_file is set on a successful (2xx) response.
// contentType is what the response actually carries — pass the sniffed type when
// the server declared none.
//
// Safety: absolute path required, O_EXCL refuses to overwrite. This blocks
// open-then-clobber on a known path but is not a sandbox — a confused caller
// can still write into anywhere they have permission for, and a hostile
// directory symlink on the parent path could redirect the write.
func saveResponseToFile(resp *http.Response, contentType, outputFile string) (*mcp.CallToolResult, error) {
	return saveResponseToFileWithNote(resp, contentType, outputFile, "")
}

// saveResponseToFileWithNote is saveResponseToFile with an extra line in the
// result — used to report what was uploaded when the response of an upload is
// itself written to disk.
func saveResponseToFileWithNote(resp *http.Response, contentType, outputFile, note string) (*mcp.CallToolResult, error) {
	outputFile = config.ExpandHome(outputFile)
	clean := filepath.Clean(outputFile)
	if !filepath.IsAbs(clean) {
		return mcp.NewToolResultError(fmt.Sprintf("output_file must be an absolute path: %s", outputFile)), nil
	}

	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create directory: %v", err)), nil
	}

	f, err := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return mcp.NewToolResultError(fmt.Sprintf("output_file already exists (refusing to overwrite): %s", clean)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("create file: %v", err)), nil
	}

	n, err := writeAndClose(f, resp.Body)
	if err != nil {
		// A half-written file would block every retry on O_EXCL.
		os.Remove(clean)
		return mcp.NewToolResultError(fmt.Sprintf("write file: %v", err)), nil
	}

	return mcp.NewToolResultText(responseFileResult(resp, contentType, clean, n, note)), nil
}

// saveResponseToGeneratedFile streams an HTTP response body to a fresh file in
// dir, naming it after pattern (an os.CreateTemp pattern). Used when a response
// carries binary payload and the caller named no destination: inlining raw
// bytes into the tool result would flood the agent's context with megabytes of
// garbage, and truncating them would hand back a corrupt file.
//
// mux does not delete these files — the caller needs them after the tool
// returns.
func saveResponseToGeneratedFile(resp *http.Response, contentType, dir, pattern string) (*mcp.CallToolResult, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create output directory: %v", err)), nil
	}

	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("create output file: %v", err)), nil
	}
	name := f.Name()

	n, err := writeAndClose(f, resp.Body)
	if err != nil {
		os.Remove(name)
		return mcp.NewToolResultError(fmt.Sprintf("write output file: %v", err)), nil
	}
	// An empty payload is not a file the caller can use — a silent 0-byte
	// "success" would only surface much later, e.g. as a mute narration track.
	if n == 0 {
		os.Remove(name)
		return mcp.NewToolResultError(fmt.Sprintf("HTTP %d %s: empty response body, nothing written",
			resp.StatusCode, resp.Status)), nil
	}

	return mcp.NewToolResultText(responseFileResult(resp, contentType, name, n,
		"No output_file given — written to the mux output directory. Pass output_file to choose the destination.")), nil
}

// writeAndClose copies src into f and closes f either way. Errors from Close
// count as write errors: buffered data and ENOSPC surface only there.
func writeAndClose(f *os.File, src io.Reader) (int64, error) {
	n, err := io.Copy(f, src)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

func responseFileResult(resp *http.Response, contentType, path string, n int64, note string) string {
	result := fmt.Sprintf("HTTP %d %s\n\nSaved to: %s\nContent-Type: %s\nSize: %d bytes",
		resp.StatusCode, resp.Status, path, contentType, n)
	if note != "" {
		result += "\n\n" + note
	}
	return result
}

// sniffContentType returns the response's declared Content-Type, or — when the
// server sent none — one detected from the first bytes of the body. Without it
// a header-less audio response would count as text and get inlined.
//
// The peeked bytes are put back, so resp.Body still reads from the start.
func sniffContentType(resp *http.Response) string {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		return ct
	}

	br := bufio.NewReaderSize(resp.Body, 512)
	head, _ := br.Peek(512)
	// Rewire unconditionally: whatever Peek buffered has to stay readable, and a
	// read error surfaces again on the caller's own read.
	resp.Body = struct {
		io.Reader
		io.Closer
	}{br, resp.Body}

	if len(head) == 0 {
		return ""
	}
	return http.DetectContentType(head)
}

// audioFileExts maps audio media types to file extensions. Covers what the
// audio APIs actually emit; other audio types fall back to their subtype.
var audioFileExts = map[string]string{
	"audio/mpeg":  ".mp3",
	"audio/mp3":   ".mp3",
	"audio/mp4":   ".m4a",
	"audio/aac":   ".aac",
	"audio/ogg":   ".ogg",
	"audio/opus":  ".opus",
	"audio/wav":   ".wav",
	"audio/x-wav": ".wav",
	"audio/wave":  ".wav",
	"audio/flac":  ".flac",
	"audio/basic": ".pcm",
	"audio/l16":   ".pcm",
	"audio/pcm":   ".pcm",
}

// binaryResponseExt reports whether a Content-Type carries payload that must
// not be inlined into a tool result, plus the file extension to store it under.
// Textual types (JSON, XML, text/*, and the application/* text formats below)
// and a missing Content-Type are inlined as before; everything else counts as
// binary.
//
// Only audio types get a meaningful extension — images and other binary
// payloads land as ".bin", because no caller needs better yet. Extend
// audioFileExts (or add a sibling table) when one does.
//
// The extension is derived from the media type, so it comes from a remote
// server: only [a-z0-9.-] survives, the rest degrades to ".bin". CreateTemp
// rejects path separators on its own, this keeps the value boring anyway.
func binaryResponseExt(contentType string) (string, bool) {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" {
		// Parsing failed outright (e.g. two types in one header) — fall back to
		// the raw value. A broken *parameter* still yields a usable media type,
		// which is why the parse result wins whenever it is non-empty.
		mediaType, _, _ = strings.Cut(contentType, ";")
		mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	}

	if mediaType == "" || isTextualMediaType(mediaType) {
		return "", false
	}
	if ext, ok := audioFileExts[mediaType]; ok {
		return ext, true
	}

	// Only audio subtypes double as a usable extension (audio/webm → .webm).
	// Everything else — octet-stream and friends — stays ".bin".
	subtype, ok := strings.CutPrefix(mediaType, "audio/")
	if !ok {
		return ".bin", true
	}
	subtype = strings.TrimPrefix(subtype, "x-")
	if subtype == "" {
		return ".bin", true
	}
	for _, r := range subtype {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return ".bin", true
		}
	}
	return "." + subtype, true
}

func isTextualMediaType(mediaType string) bool {
	switch mediaType {
	case "application/json", "application/xml", "application/javascript",
		"application/csv", "application/yaml", "application/x-yaml",
		"application/x-ndjson", "application/ndjson",
		"application/x-www-form-urlencoded":
		return true
	}
	return strings.HasPrefix(mediaType, "text/") ||
		strings.HasSuffix(mediaType, "+json") ||
		strings.HasSuffix(mediaType, "+xml")
}
