package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Tool parameter descriptions shared by every connector that can send a file.
// Uploads are the counterpart of output_file: the model names a path, mux
// moves the bytes, and the file never passes through the conversation.
const (
	// jsonBodyDesc is appended to a per-tool sentence that says whether body is
	// optional or required. Kept separate so the two halves cannot contradict
	// each other: body is optional for http/openai, but required for the
	// recraft/ideogram post tools unless a file is uploaded instead.
	jsonBodyDesc = "Not allowed together with file_path — the text parts of an upload go into form_fields."
	filePathDesc = "Absolute path of a local file to upload (supports ~ for home directory). " +
		"Switches the request to multipart/form-data: the file is streamed from disk as one part and " +
		"form_fields become the other parts. Cannot be combined with body. " +
		"mux reads the file itself, so the path must be visible to the mux process (in a container: mounted)."
	formFieldsDesc = "Additional multipart text fields as a JSON object of scalar values, " +
		`e.g. {"model": "whisper-1", "temperature": 0}. Numbers and booleans are sent as their JSON text; ` +
		"null and nested values are rejected. Requires file_path."
)

// uploadParams returns the three tool parameters that turn a JSON request into
// a multipart upload. defaultField is the form field the file is sent under
// when file_field is not given — it differs per API ("file" for most,
// "image" for Ideogram).
func uploadParams(defaultField string) []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("file_path", mcp.Description(filePathDesc)),
		mcp.WithString("file_field", mcp.Description(
			fmt.Sprintf("Form field name for the file part (default: %q).", defaultField))),
		mcp.WithString("form_fields", mcp.Description(formFieldsDesc)),
	}
}

// multipartUpload is a multipart/form-data body built from one local file and
// optional text fields. The file is never held in memory: the body is the
// pre-rendered part headers, the file itself and the closing boundary,
// concatenated at send time. Because all three are known up front the request
// carries an exact Content-Length (some servers refuse chunked uploads), and
// GetBody can rebuild the body for a 307/308 redirect by reopening the file.
type multipartUpload struct {
	filePath    string            // cleaned absolute path
	fileField   string            // form field the file is sent under
	contentType string            // of the file part
	size        int64             // file size at validation time
	fields      map[string]string // text parts, sent before the file

	head              []byte // text parts + file part header, everything before the file bytes
	tail              []byte // closing boundary
	contentTypeHeader string // multipart/form-data; boundary=…
}

// stringArg reads a tool argument that the input schema declares as a string.
// mcp-go passes the raw argument map to the handler without validating it
// against the schema, and GetString answers with the default for any other
// type — an argument sent as an object or a number would be dropped without a
// word, and a request the caller believes carries it would go out without it.
func stringArg(req mcp.CallToolRequest, key string) (string, error) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %T", key, v)
	}
	return s, nil
}

// requireBodyOrFile answers whether a tool that needs either a JSON body or an
// upload was given one. It goes through stringArg so an argument of the wrong
// type is reported as such instead of being counted as absent.
func requireBodyOrFile(req mcp.CallToolRequest) error {
	body, err := stringArg(req, "body")
	if err != nil {
		return err
	}
	filePath, err := stringArg(req, "file_path")
	if err != nil {
		return err
	}
	if body == "" && filePath == "" {
		return errors.New("body or file_path is required")
	}
	return nil
}

// formFieldsArg returns form_fields as JSON text. The schema declares a
// string, but the parameter description shows an object literal, so a client
// may well send a real JSON object: it is re-encoded here rather than
// dropped. Re-encoding runs over the already decoded value, so a number keeps
// its value but not necessarily its spelling (0.30 arrives as 0.3); the string
// form is passed through untouched.
func formFieldsArg(req mcp.CallToolRequest) (string, error) {
	v, ok := req.GetArguments()["form_fields"]
	if !ok || v == nil {
		return "", nil
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case map[string]any:
		raw, err := json.Marshal(t)
		if err != nil {
			return "", fmt.Errorf("form_fields is not encodable as JSON: %w", err)
		}
		return string(raw), nil
	default:
		return "", fmt.Errorf("form_fields must be a JSON object or a JSON string, got %T", v)
	}
}

// parseUpload reads file_path, file_field and form_fields from a tool call.
// It returns nil when file_path is absent (plain JSON mode) and an error when
// the arguments contradict each other or the file is unusable. Everything is
// checked before a request goes out, so a bad path never costs a round trip.
func parseUpload(req mcp.CallToolRequest, defaultField string) (*multipartUpload, error) {
	filePath, err := stringArg(req, "file_path")
	if err != nil {
		return nil, err
	}
	fileField, err := stringArg(req, "file_field")
	if err != nil {
		return nil, err
	}
	formFields, err := formFieldsArg(req)
	if err != nil {
		return nil, err
	}

	if filePath == "" {
		switch {
		case formFields != "":
			return nil, errors.New("form_fields requires file_path (multipart mode); for a JSON request use body")
		case fileField != "":
			return nil, errors.New("file_field requires file_path")
		}
		return nil, nil
	}
	body, err := stringArg(req, "body")
	if err != nil {
		return nil, err
	}
	if body != "" {
		return nil, errors.New("body and file_path are mutually exclusive: a multipart upload carries its text values in form_fields, not in a JSON body")
	}
	if fileField == "" {
		fileField = defaultField
	}

	fields, err := parseFormFields(formFields)
	if err != nil {
		return nil, err
	}

	clean := filepath.Clean(config.ExpandHome(filePath))
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("file_path must be an absolute path: %s", filePath)
	}
	// Stat before Open: opening a FIFO that has no writer blocks forever, and
	// at this point neither the caller's context nor a client timeout is in
	// play — the tool call would simply never return.
	fi, err := os.Stat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file_path not found: %s (resolved on the machine running mux)", clean)
		}
		return nil, fmt.Errorf("file_path is not usable: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("file_path is not a regular file: %s", clean)
	}
	f, err := os.Open(clean)
	if err != nil {
		return nil, fmt.Errorf("file_path is not readable: %w", err)
	}
	defer f.Close()

	u := &multipartUpload{
		filePath:    clean,
		fileField:   fileField,
		contentType: detectFileContentType(clean, f),
		size:        fi.Size(),
		fields:      fields,
	}
	if err := u.render(); err != nil {
		return nil, err
	}
	return u, nil
}

// parseFormFields decodes the form_fields JSON object into string parts.
// Scalars are sent as their JSON text (numbers unchanged, booleans as
// true/false); null and nested values have no multipart representation and
// are rejected rather than guessed at.
func parseFormFields(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("form_fields must be a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("form_fields must be a JSON object, got null")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("form_fields must be a single JSON object")
	}

	fields := make(map[string]string, len(obj))
	for k, v := range obj {
		switch t := v.(type) {
		case string:
			fields[k] = t
		case json.Number:
			fields[k] = t.String()
		case bool:
			fields[k] = strconv.FormatBool(t)
		case nil:
			return nil, fmt.Errorf("form_fields.%s is null: a multipart field needs a value", k)
		default:
			return nil, fmt.Errorf("form_fields.%s must be a string, number or boolean (nested values cannot be sent as form fields)", k)
		}
	}
	return fields, nil
}

// detectFileContentType picks the Content-Type for the file part: by
// extension when the extension is known, otherwise from the first bytes of
// the file, which degrades to application/octet-stream.
func detectFileContentType(path string, f *os.File) string {
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return http.DetectContentType(head[:n])
}

// render pre-computes everything around the file bytes. The text fields go
// first in a stable order, then the file part header; the closing boundary is
// what multipart.Writer.Close would write. The file itself is only touched at
// send time.
func (u *multipartUpload) render() error {
	var head bytes.Buffer
	w := multipart.NewWriter(&head)

	keys := make([]string, 0, len(u.fields))
	for k := range u.fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := w.WriteField(k, u.fields[k]); err != nil {
			return fmt.Errorf("form_fields: %w", err)
		}
	}

	h := make(textproto.MIMEHeader)
	// FileContentDisposition percent-escapes CR and LF as well as quoting, so a
	// field name or file name carrying a newline cannot break out of the part
	// header. Both are caller-controlled, and a file name may legally contain
	// CR/LF on Unix.
	h.Set("Content-Disposition", multipart.FileContentDisposition(u.fileField, filepath.Base(u.filePath)))
	h.Set("Content-Type", u.contentType)
	if _, err := w.CreatePart(h); err != nil {
		return fmt.Errorf("multipart: %w", err)
	}

	var tail bytes.Buffer
	tw := multipart.NewWriter(&tail)
	if err := tw.SetBoundary(w.Boundary()); err != nil {
		return fmt.Errorf("multipart: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("multipart: %w", err)
	}

	u.head = head.Bytes()
	u.tail = tail.Bytes()
	u.contentTypeHeader = w.FormDataContentType()
	return nil
}

// newRequest builds the outgoing request. The body streams head → file → tail
// with an exact Content-Length; GetBody reopens the file so the client can
// replay the body across a 307/308 redirect (without it Go hands the redirect
// response back unfollowed).
func (u *multipartUpload) newRequest(ctx context.Context, method, url string) (*http.Request, error) {
	body, err := u.openBody()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		body.Close()
		return nil, fmt.Errorf("invalid request: %w", err)
	}
	req.ContentLength = int64(len(u.head)) + u.size + int64(len(u.tail))
	req.GetBody = u.openBody
	req.Header.Set("Content-Type", u.contentTypeHeader)
	return req, nil
}

// openBody opens the file and returns the complete multipart body as one
// stream. Closing the body closes the file; the transport does that after the
// request is sent, even on errors.
func (u *multipartUpload) openBody() (io.ReadCloser, error) {
	f, err := os.Open(u.filePath)
	if err != nil {
		return nil, fmt.Errorf("open file_path: %w", err)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(u.head), f, bytes.NewReader(u.tail)), f}, nil
}

// note is the line added to the tool result so the caller sees what went out.
// Safe on a nil upload (JSON mode): returns "".
func (u *multipartUpload) note() string {
	if u == nil {
		return ""
	}
	return fmt.Sprintf("Uploaded %s as %q: %d bytes, %s",
		filepath.Base(u.filePath), u.fileField, u.size, u.contentType)
}

// newBodyRequest builds the request for a tool call that carries either a
// JSON body or a file upload, and returns the upload (nil in JSON mode) so the
// caller can pick the client and annotate the result. Only Content-Type is set
// here; Accept and auth stay with the connector.
func newBodyRequest(ctx context.Context, method, url string, req mcp.CallToolRequest, defaultFileField string) (*http.Request, *multipartUpload, error) {
	// Refuse an upload on GET before parseUpload touches the file system: the
	// get tools do not declare the upload parameters, so a file_path here is a
	// mistake, and answering it with a stat result would turn a read-only
	// connection into a file-existence oracle.
	if method == http.MethodGet {
		if fp, err := stringArg(req, "file_path"); err != nil || fp != "" {
			return nil, nil, errors.New("file_path is not supported on GET requests")
		}
	}

	upload, err := parseUpload(req, defaultFileField)
	if err != nil {
		return nil, nil, err
	}
	if upload != nil {
		httpReq, err := upload.newRequest(ctx, method, url)
		if err != nil {
			return nil, nil, err
		}
		return httpReq, upload, nil
	}

	body, err := stringArg(req, "body")
	if err != nil {
		return nil, nil, err
	}
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid request: %w", err)
	}
	if bodyReader != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	return httpReq, nil, nil
}

// uploadTimeout bounds one upload end to end. It is not sized for any real
// file — 100 MB at 1 MB/s take under two minutes — it exists for a single
// case: a server that accepts the connection and then stops reading. No other
// timer covers that. The transport's ResponseHeaderTimeout starts only once
// the body has been written in full, and the stdio transport has no
// per-request deadline, so without this cap such a call would hold one of the
// worker slots until mux restarts. A var so tests can shorten it.
var uploadTimeout = 30 * time.Minute

// clientFor returns the client to send with: base for JSON calls, a copy with
// the upload budget instead of the JSON one for uploads. The connectors' own
// Timeout (30–60 s) is sized for JSON round trips and would cut off a large
// file on a slow link.
func clientFor(base *http.Client, upload *multipartUpload) *http.Client {
	if upload == nil {
		return base
	}
	c := *base
	c.Timeout = uploadTimeout
	return &c
}

// requestError turns a failed Do into the tool result text. An upload that ran
// into uploadTimeout is named as such — Go's own message reads
// "Client.Timeout exceeded while awaiting headers", which is literally true
// (no headers ever came) and hides that the body was still being sent — and
// the abort is logged, so whoever reads the mux log sees why a long call
// disappeared. A timeout or cancellation that came from the caller's context
// is reported as it is: that was the caller's decision, not this cap.
func requestError(ctx context.Context, err error, req *http.Request, upload *multipartUpload) string {
	if upload != nil && ctx.Err() == nil {
		var uerr *url.Error
		if errors.As(err, &uerr) && uerr.Timeout() {
			msg := fmt.Sprintf("upload aborted after %s: %s accepted the connection but did not finish taking %d bytes of %s (mux upload timeout, not the server's answer)",
				uploadTimeout, req.URL.Host, upload.size, filepath.Base(upload.filePath))
			log.Printf("[upload] %s %s: %s", req.Method, req.URL.Redacted(), msg)
			return msg
		}
	}
	return fmt.Sprintf("request failed: %v", err)
}

// statusLine renders the head of a tool result: the HTTP status, plus the
// upload note when a file went out.
func statusLine(resp *http.Response, upload *multipartUpload) string {
	s := fmt.Sprintf("HTTP %d %s", resp.StatusCode, resp.Status)
	if n := upload.note(); n != "" {
		s += "\n" + n
	}
	return s
}
