package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Tool parameter descriptions shared by every connector that can send a file.
// Uploads are the counterpart of output_file: the model names a path, mux
// moves the bytes, and the file never passes through the conversation.
const (
	jsonBodyDesc = "JSON request body (optional). Not allowed together with file_path — " +
		"the text parts of an upload go into form_fields."
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

// parseUpload reads file_path, file_field and form_fields from a tool call.
// It returns nil when file_path is absent (plain JSON mode) and an error when
// the arguments contradict each other or the file is unusable. Everything is
// checked before a request goes out, so a bad path never costs a round trip.
func parseUpload(req mcp.CallToolRequest, defaultField string) (*multipartUpload, error) {
	filePath := req.GetString("file_path", "")
	fileField := req.GetString("file_field", "")
	formFields := req.GetString("form_fields", "")

	if filePath == "" {
		switch {
		case formFields != "":
			return nil, errors.New("form_fields requires file_path (multipart mode); for a JSON request use body")
		case fileField != "":
			return nil, errors.New("file_field requires file_path")
		}
		return nil, nil
	}
	if req.GetString("body", "") != "" {
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
	f, err := os.Open(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file_path not found: %s (resolved on the machine running mux)", clean)
		}
		return nil, fmt.Errorf("file_path is not readable: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("file_path is not usable: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("file_path is not a regular file: %s", clean)
	}

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
		return nil, fmt.Errorf("form_fields must be a JSON object: %v", err)
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
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
		escapeQuotes(u.fileField), escapeQuotes(filepath.Base(u.filePath))))
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

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
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
	upload, err := parseUpload(req, defaultFileField)
	if err != nil {
		return nil, nil, err
	}
	if upload != nil {
		if method == http.MethodGet {
			return nil, nil, errors.New("file_path is not supported on GET requests")
		}
		httpReq, err := upload.newRequest(ctx, method, url)
		if err != nil {
			return nil, nil, err
		}
		return httpReq, upload, nil
	}

	var bodyReader io.Reader
	if body := req.GetString("body", ""); body != "" {
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

// clientFor returns the client to send with: base for JSON calls, a copy
// without the overall Timeout for uploads. A budget sized for JSON round trips
// would cut off a large file on a slow link; the upload stays bounded by the
// transport's ResponseHeaderTimeout (the server has to answer once the body is
// in) and by the caller's context.
func clientFor(base *http.Client, upload *multipartUpload) *http.Client {
	if upload == nil {
		return base
	}
	c := *base
	c.Timeout = 0
	return &c
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
