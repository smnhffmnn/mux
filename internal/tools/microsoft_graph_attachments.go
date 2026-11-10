package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/blob"
	"github.com/smnhffmnn/mux/internal/config"
)

// Attachments on drafts (MUX-58). The three draft tools take an attachments
// argument: files the agent uploaded to mux as blobs (blob:<id>), or paths
// where agent and mux share a disk. Graph has no way to create a message
// with files in one request, so the draft is created first and every file
// becomes one attachment on it afterwards:
//
//   - under 3 MB, as a fileAttachment with the bytes base64-encoded in one
//     POST /me/messages/{id}/attachments;
//   - from 3 MB up to 150 MB, through an upload session — POST
//     …/attachments/createUploadSession, then the bytes in ranges of a few
//     MB with PUT to the pre-authenticated URL Graph hands out.
//
// The files are validated before the draft is created, so a wrong list
// leaves nothing behind. A file that fails afterwards is named in the
// result, the draft stays with what did get attached, and the result is an
// error, so the draft is not sent by accident. A blob is deleted once its
// attachment is on the draft (the rule of every write that consumes a blob);
// the blob of a failed file is kept for the retry.

const (
	// graphAttachmentInlineMax is the size up to which Graph takes a
	// fileAttachment with contentBytes in one request — "under 3 MB"; a
	// larger file needs an upload session, which in turn refuses a smaller
	// one.
	graphAttachmentInlineMax = 3 << 20
	// graphAttachmentMax is what an upload session carries.
	graphAttachmentMax = 150 << 20
	// graphUploadRange is the bytes per PUT of an upload session: Graph asks
	// for ranges under 4 MB; a multiple of 320 KiB, the alignment OneDrive's
	// sessions require, costs nothing here.
	graphUploadRange = 12 * 320 * 1024
	// graphUploadBodyMax bounds what is read of a session's answers.
	graphUploadBodyMax = 64 << 10
)

// graphAttachmentFile is one validated entry of the attachments argument.
type graphAttachmentFile struct {
	path        string
	name        string
	contentType string
	size        int64
	blob        *blob.Info // set for a blob:<id> entry; deleted once the attachment is on the draft
	given       string     // the entry as the agent wrote it, for the result
}

// graphAttachmentResult is one attachment the call put on the draft.
type graphAttachmentResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"contentType"`
	File        string `json:"file"`
	Note        string `json:"note,omitempty"` // set when the attachment is on the draft but its id could not be read back
}

// graphAttachmentError is one file the call could not attach.
type graphAttachmentError struct {
	File  string `json:"file"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

// graphAttachmentsDescription documents the argument on all three draft tools.
const graphAttachmentsDescription = "Files to attach, one attachment each: blob:<id> of a blob uploaded to mux (POST /blob, named with X-Blob-Name or ?name=), or an absolute path where you and mux share a disk (supports ~). An entry may be an object {\"file\": \"blob:<id>\", \"name\": \"report.xlsx\"} to attach the file under another name (a blob uploaded without a name needs this). Names are unique within the call (case-insensitive); each blob or path may appear once. Under 3 MB a file goes in one request, up to 150 MB through an upload session; Exchange's message size limit (35 MB by default) applies when the draft is sent. The result lists what was attached (id, name, size); a file that fails is named in attachment_errors and the result is an error, the draft stays with the rest — do not send it as is. A blob is deleted once its attachment is on the draft; the blob of a failed file is kept."

// graphAttachmentsOption is the schema of the attachments argument: strings,
// or objects with file and an optional name.
func graphAttachmentsOption() mcp.ToolOption {
	return mcp.WithArray("attachments",
		mcp.Description(graphAttachmentsDescription),
		mcp.Items(map[string]any{
			"anyOf": []any{
				map[string]any{"type": "string", "description": "blob:<id> or an absolute path"},
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{"type": "string", "description": "blob:<id> or an absolute path"},
						"name": map[string]any{"type": "string", "description": "the attachment's file name (default: the blob's name, or the path's file name)"},
					},
					"required":             []string{"file"},
					"additionalProperties": false,
				},
			},
		}),
	)
}

// parseGraphAttachments reads and validates the attachments argument, all of
// it, before anything is sent: every file exists, is regular, readable, has a
// name, is not empty and not over the session's cap; names are unique within
// the call. nil without the argument.
func parseGraphAttachments(req mcp.CallToolRequest) ([]graphAttachmentFile, error) {
	raw, ok := req.GetArguments()["attachments"]
	if !ok || raw == nil {
		return nil, nil
	}
	entries, err := graphAttachmentEntries(raw)
	if err != nil {
		return nil, err
	}
	files := make([]graphAttachmentFile, 0, len(entries))
	seenName := map[string]bool{}
	seenFile := map[string]bool{}
	for _, e := range entries {
		f, err := openGraphAttachment(e.file, e.name)
		if err != nil {
			return nil, err
		}
		if seenName[strings.ToLower(f.name)] {
			return nil, fmt.Errorf("attachments: two files are named %q — give one of them another name", f.name)
		}
		seenName[strings.ToLower(f.name)] = true
		// The same blob under two names would pass the name check and then
		// fail on the second entry: the first attachment consumes (deletes)
		// the blob. The same path twice is the same file twice.
		if seenFile[f.path] {
			return nil, fmt.Errorf("attachments: %s is listed twice — one entry per file", f.given)
		}
		seenFile[f.path] = true
		files = append(files, f)
	}
	return files, nil
}

// graphAttachmentEntry is one entry of the argument before validation.
type graphAttachmentEntry struct {
	file string
	name string
}

// graphAttachmentEntries accepts the array of strings and objects the schema
// describes, and — as every list argument of mux does — the same list as a
// JSON string, or a single entry on its own.
func graphAttachmentEntries(raw any) ([]graphAttachmentEntry, error) {
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return nil, nil
		}
		if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
			var parsed any
			if err := json.Unmarshal([]byte(s), &parsed); err != nil {
				return nil, fmt.Errorf("attachments: not a JSON list: %w", err)
			}
			return graphAttachmentEntries(parsed)
		}
		return []graphAttachmentEntry{{file: s}}, nil
	case []string:
		out := make([]graphAttachmentEntry, 0, len(v))
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, graphAttachmentEntry{file: s})
			}
		}
		return out, nil
	case map[string]any:
		e, err := graphAttachmentObject(v, 0)
		if err != nil {
			return nil, err
		}
		return []graphAttachmentEntry{e}, nil
	case []any:
		out := make([]graphAttachmentEntry, 0, len(v))
		for i, item := range v {
			switch it := item.(type) {
			case string:
				if s := strings.TrimSpace(it); s != "" {
					out = append(out, graphAttachmentEntry{file: s})
				}
			case map[string]any:
				e, err := graphAttachmentObject(it, i)
				if err != nil {
					return nil, err
				}
				out = append(out, e)
			default:
				return nil, fmt.Errorf("attachments[%d] must be a string (blob:<id> or path) or an object {file, name}, got %T", i, item)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("attachments must be a list of blob:<id> references or paths, got %T", raw)
	}
}

func graphAttachmentObject(obj map[string]any, i int) (graphAttachmentEntry, error) {
	var e graphAttachmentEntry
	for k, v := range obj {
		switch k {
		case "file", "name":
			s, ok := v.(string)
			if !ok {
				return e, fmt.Errorf("attachments[%d].%s must be a string, got %T", i, k, v)
			}
			if k == "file" {
				e.file = strings.TrimSpace(s)
			} else {
				e.name = strings.TrimSpace(s)
			}
		default:
			return e, fmt.Errorf("attachments[%d]: unknown key %q — an entry is {\"file\": …, \"name\": …}", i, k)
		}
	}
	if e.file == "" {
		return e, fmt.Errorf("attachments[%d]: file is required (blob:<id> or an absolute path)", i)
	}
	return e, nil
}

// openGraphAttachment validates one entry the way the other upload paths do
// (YouTrack attach, multipart uploads): blob or absolute path, existing,
// regular, readable — and sizes it for the two ways into Graph.
func openGraphAttachment(given, nameOverride string) (graphAttachmentFile, error) {
	var f graphAttachmentFile
	clean, blobInfo, err := blobInputFile(given, "attachments")
	if err != nil {
		return f, err
	}
	if blobInfo == nil {
		clean = filepath.Clean(config.ExpandHome(given))
		if !filepath.IsAbs(clean) {
			return f, fmt.Errorf("attachments: %q must be blob:<id> or an absolute path", given)
		}
	}
	fi, err := os.Stat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return f, fmt.Errorf("attachments: %s not found (resolved on the machine running mux)", clean)
		}
		return f, fmt.Errorf("attachments: %s is not usable: %w", clean, err)
	}
	if !fi.Mode().IsRegular() {
		return f, fmt.Errorf("attachments: %s is not a regular file", clean)
	}
	name := nameOverride
	if name == "" {
		if blobInfo != nil {
			name = blobInfo.Name
			if name == "" {
				return f, fmt.Errorf("attachments: blob %s has no name — upload it with a name (X-Blob-Name or ?name=), or give one in the entry: {\"file\": \"blob:%s\", \"name\": \"…\"}", blobInfo.ID, blobInfo.ID)
			}
		} else {
			name = filepath.Base(clean)
		}
	}
	if err := validGraphAttachmentName(name); err != nil {
		return f, fmt.Errorf("attachments: %w", err)
	}
	if fi.Size() == 0 {
		return f, fmt.Errorf("attachments: %s is empty — Graph does not take an attachment without bytes", name)
	}
	if fi.Size() > graphAttachmentMax {
		return f, fmt.Errorf("attachments: %s is %s, more than the %s an attachment can carry", name, blob.FormatSize(fi.Size()), blob.FormatSize(graphAttachmentMax))
	}
	fh, err := os.Open(clean)
	if err != nil {
		return f, fmt.Errorf("attachments: %s is not readable: %w", clean, err)
	}
	ct := detectFileContentType(name, fh)
	fh.Close()
	if blobInfo != nil && blobInfo.ContentType != "" && blobInfo.ContentType != "application/octet-stream" {
		ct = blobInfo.ContentType
	}
	display := given
	if blobInfo != nil {
		display = blobDisplay(blobInfo)
	}
	return graphAttachmentFile{path: clean, name: name, contentType: ct, size: fi.Size(), blob: blobInfo, given: display}, nil
}

// validGraphAttachmentName keeps the attachment's file name a file name: no
// path separators, no control characters, not empty.
func validGraphAttachmentName(name string) error {
	if name == "" {
		return errors.New("an attachment's name must not be empty")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("attachment name %q must not contain / or \\", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("attachment name %q contains a control character", name)
		}
	}
	return nil
}

// attachToDraft puts every file on the draft, one attachment each, and
// reports what went on and what did not. It does not stop at the first
// failure: the other files are the agent's intent too.
func (mg *MicrosoftGraph) attachToDraft(ctx context.Context, draftID string, files []graphAttachmentFile) (attached []graphAttachmentResult, failed []graphAttachmentError) {
	attached = make([]graphAttachmentResult, 0, len(files))
	for _, f := range files {
		var (
			id, note string
			err      error
		)
		if f.size < graphAttachmentInlineMax {
			id, err = mg.attachInline(ctx, draftID, f)
		} else {
			id, note, err = mg.attachViaSession(ctx, draftID, f)
		}
		if err != nil {
			failed = append(failed, graphAttachmentError{File: f.given, Name: f.name, Error: err.Error()})
			continue
		}
		consumeBlob(f.blob)
		attached = append(attached, graphAttachmentResult{ID: id, Name: f.name, Size: f.size, ContentType: f.contentType, File: f.given, Note: note})
	}
	return attached, failed
}

// draftResult finishes a draft tool's call: without files it is the plain
// result; with files, the result carries the attachments — and is an error
// when any file failed, so an agent reading only the status does not send a
// draft that is missing a file.
func (mg *MicrosoftGraph) draftResult(ctx context.Context, out map[string]any, draftID string, files []graphAttachmentFile) (*mcp.CallToolResult, error) {
	if len(files) == 0 {
		return jsonResult(out)
	}
	attached, failed := mg.attachToDraft(ctx, draftID, files)
	out["attachments"] = attached
	if len(failed) == 0 {
		return jsonResult(out)
	}
	out["attachment_errors"] = failed
	out["note"] = fmt.Sprintf("the draft exists with %d of %d attachments and was not sent. Do not send it as is. Either add the missing files to it in Outlook, or create the draft again with every file — the blobs of the failed files are kept, the ones already attached were deleted and have to be uploaded again — and delete this draft in Outlook.", len(attached), len(files))
	data, _ := json.MarshalIndent(out, "", "  ")
	return mcp.NewToolResultError(string(data)), nil
}

// attachInline adds a file of under 3 MB as one fileAttachment with the bytes
// in the request.
func (mg *MicrosoftGraph) attachInline(ctx context.Context, draftID string, f graphAttachmentFile) (string, error) {
	content, err := os.ReadFile(f.path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", f.name, err)
	}
	if int64(len(content)) != f.size {
		// The file changed between validation and now; the other path would
		// have been right for it, or the size in the result would lie.
		return "", fmt.Errorf("%s changed while attaching: %d bytes now, %d when checked", f.name, len(content), f.size)
	}
	payload := map[string]any{
		"@odata.type":  "#microsoft.graph.fileAttachment",
		"name":         f.name,
		"contentType":  f.contentType,
		"contentBytes": base64.StdEncoding.EncodeToString(content),
	}
	// Through the upload client: this body is up to ~4 MB of base64, and the
	// 30 s the default client allows for a whole request is too short for it
	// on a slow line. The upload client bounds the wait by the transport's
	// header timeout once the body is written, not by an overall deadline.
	data, status, err := mg.doGraphRetrying(ctx, mg.uploadClient, http.MethodPost, "/me/messages/"+url.PathEscape(draftID)+"/attachments", payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("attach %s failed (HTTP %d): %s", f.name, status, graphErrorText(data))
	}
	var att struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &att); err != nil || att.ID == "" {
		return "", fmt.Errorf("attach %s: Graph answered 201 without an attachment id", f.name)
	}
	return att.ID, nil
}

// graphUploadSession is Graph's answer to createUploadSession.
type graphUploadSession struct {
	UploadURL          string   `json:"uploadUrl"`
	ExpirationDateTime string   `json:"expirationDateTime"`
	NextExpectedRanges []string `json:"nextExpectedRanges"`
}

// attachViaSession adds a file of 3 MB and more through an upload session:
// Graph opens one for the attachment and hands out a pre-authenticated URL;
// the bytes go there in order, a range per PUT; the PUT that completes the
// file answers 201 with the attachment's location. note is set when the
// attachment is on the draft but its id could not be read back.
func (mg *MicrosoftGraph) attachViaSession(ctx context.Context, draftID string, f graphAttachmentFile) (id, note string, err error) {
	payload := map[string]any{
		"AttachmentItem": map[string]any{
			"attachmentType": "file",
			"name":           f.name,
			"size":           f.size,
			"contentType":    f.contentType,
		},
	}
	data, status, err := mg.doGraphRetrying(ctx, mg.uploadClient, http.MethodPost, "/me/messages/"+url.PathEscape(draftID)+"/attachments/createUploadSession", payload)
	if err != nil {
		return "", "", err
	}
	if status != http.StatusCreated {
		return "", "", fmt.Errorf("upload session for %s refused (HTTP %d): %s", f.name, status, graphErrorText(data))
	}
	var session graphUploadSession
	if err := json.Unmarshal(data, &session); err != nil || session.UploadURL == "" {
		return "", "", fmt.Errorf("upload session for %s: Graph answered 201 without an uploadUrl", f.name)
	}
	u, err := url.Parse(session.UploadURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", fmt.Errorf("upload session for %s: uploadUrl %q is not an https URL", f.name, session.UploadURL)
	}

	id, err = mg.uploadRanges(ctx, session.UploadURL, f)
	if err != nil {
		mg.cancelUploadSession(session.UploadURL)
		return "", "", err
	}
	if id == "" {
		// The completing PUT carried no Location. The attachment is on the
		// draft all the same — Graph answered 201 — so its id comes from the
		// draft's attachment list; and when that read fails too, the file is
		// still attached, so it is reported attached with the gap named, not
		// as a failure that would make the agent add it a second time.
		id, err = mg.attachmentIDByName(ctx, draftID, f.name)
		if err != nil {
			return "", fmt.Sprintf("attached (Graph completed the upload), but the attachment id could not be read back: %v", err), nil
		}
	}
	return id, "", nil
}

// reGraphAttachmentLocation reads the attachment id out of the Location a
// completed upload session answers with: …/Attachments('<id>').
var reGraphAttachmentLocation = regexp.MustCompile(`(?i)/attachments\('([^']+)'\)`)

// uploadRanges sends the file to the session's URL in ranges and returns the
// attachment id from the final Location, "" when Graph sent none.
func (mg *MicrosoftGraph) uploadRanges(ctx context.Context, uploadURL string, f graphAttachmentFile) (string, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", f.name, err)
	}
	defer fh.Close()
	// The session was opened for exactly f.size bytes. A file that grew since
	// validation would have its tail dropped (we send only f.size); one that
	// shrank is caught by io.ReadFull below. Catch the grow here instead of
	// completing a silently truncated upload.
	if fi, err := fh.Stat(); err != nil {
		return "", fmt.Errorf("read %s: %w", f.name, err)
	} else if fi.Size() != f.size {
		return "", fmt.Errorf("%s changed while attaching: %d bytes now, %d when checked", f.name, fi.Size(), f.size)
	}

	buf := make([]byte, graphUploadRange)
	var offset int64
	for offset < f.size {
		want := graphUploadRange
		if rest := f.size - offset; rest < int64(want) {
			want = int(rest)
		}
		n, err := io.ReadFull(fh, buf[:want])
		if err != nil {
			return "", fmt.Errorf("read %s at %d: %w (did the file change while attaching?)", f.name, offset, err)
		}
		status, header, body, err := mg.putRange(ctx, uploadURL, buf[:n], offset, f.size)
		if err != nil {
			return "", fmt.Errorf("upload %s at %d: %w", f.name, offset, err)
		}
		offset += int64(n)

		switch status {
		case http.StatusOK, http.StatusAccepted:
			if offset >= f.size {
				return "", fmt.Errorf("upload %s: Graph answered %d after the last range instead of completing the attachment", f.name, status)
			}
			// Graph says where it expects the next bytes; it is where we
			// are, or something went wrong in between.
			var next struct {
				NextExpectedRanges []string `json:"nextExpectedRanges"`
			}
			if json.Unmarshal(body, &next) == nil && len(next.NextExpectedRanges) > 0 {
				start, _, _ := strings.Cut(next.NextExpectedRanges[0], "-")
				if expected, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64); err == nil && expected != offset {
					return "", fmt.Errorf("upload %s: Graph expects the next range at byte %d, mux is at %d", f.name, expected, offset)
				}
			}
		case http.StatusCreated:
			if offset < f.size {
				return "", fmt.Errorf("upload %s: Graph completed the attachment after %d of %d bytes", f.name, offset, f.size)
			}
			if m := reGraphAttachmentLocation.FindStringSubmatch(header.Get("Location")); m != nil {
				if id, err := url.PathUnescape(m[1]); err == nil {
					return id, nil
				}
				return m[1], nil
			}
			return "", nil
		default:
			return "", fmt.Errorf("upload %s at %d failed (HTTP %d): %s", f.name, offset-int64(n), status, graphErrorText(body))
		}
	}
	return "", fmt.Errorf("upload %s: nothing to send", f.name)
}

// putRange sends one byte range to the upload session's URL and returns the
// status, the response headers and the (bounded) body. A throttle (429, or
// 503 with a Retry-After) is retried once: the range was not stored, so
// resending the same bytes at the same offset makes no duplicate. A plain
// 5xx is left to the caller, which fails the file rather than risk a second
// copy on a write that may have gone through.
func (mg *MicrosoftGraph) putRange(ctx context.Context, uploadURL string, chunk []byte, offset, total int64) (int, http.Header, []byte, error) {
	status, header, body, err := mg.doPutRange(ctx, uploadURL, chunk, offset, total)
	if err != nil {
		return status, header, body, err
	}
	delay, retry := graphRetryDelay(status, header)
	if !retry {
		return status, header, body, nil
	}
	select {
	case <-ctx.Done():
		return status, header, body, ctx.Err()
	case <-time.After(delay):
	}
	return mg.doPutRange(ctx, uploadURL, chunk, offset, total)
}

// doPutRange is one PUT of a byte range. The upload URL is pre-authenticated,
// so no Authorization header goes on it.
func (mg *MicrosoftGraph) doPutRange(ctx context.Context, uploadURL string, chunk []byte, offset, total int64) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(chunk))
	if err != nil {
		return 0, nil, nil, err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(chunk))-1, total))
	resp, err := mg.uploadClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, graphUploadBodyMax))
	return resp.StatusCode, resp.Header, body, nil
}

// cancelUploadSession tells Graph to drop a session that will not complete;
// best effort, the session expires on its own otherwise.
func (mg *MicrosoftGraph) cancelUploadSession(uploadURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, uploadURL, nil)
	if err != nil {
		return
	}
	if resp, err := mg.uploadClient.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, graphUploadBodyMax))
		resp.Body.Close()
	}
}

// attachmentIDByName finds an attachment of the draft by its name, following
// Graph's paging in case the draft carries many (a forward of a message with
// several attachments).
func (mg *MicrosoftGraph) attachmentIDByName(ctx context.Context, draftID, name string) (string, error) {
	path := "/me/messages/" + url.PathEscape(draftID) + "/attachments?$select=id,name"
	var last string // newest match: Graph lists in order of creation, the upload is last
	for page := 0; path != "" && page < 20; page++ {
		data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("list attachments (HTTP %d): %s", status, graphErrorText(data))
		}
		var resp struct {
			Value []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return "", fmt.Errorf("parse attachments: %w", err)
		}
		for _, a := range resp.Value {
			if strings.EqualFold(a.Name, name) {
				last = a.ID
			}
		}
		if resp.NextLink == "" {
			break
		}
		after, ok := strings.CutPrefix(resp.NextLink, mg.baseURL)
		if !ok {
			break
		}
		path = after
	}
	if last != "" {
		return last, nil
	}
	return "", fmt.Errorf("no attachment named %q on the draft", name)
}

// doGraphRetrying is doGraph with the attachment writes' retry policy: one
// more try after a throttle, through the given client (nil → the default
// one). Only a throttle is retried (graphRetryDelay) — the attachment POST
// is not idempotent, so an ambiguous 5xx is left to the caller rather than
// risk a second copy.
func (mg *MicrosoftGraph) doGraphRetrying(ctx context.Context, client *http.Client, method, path string, body any) ([]byte, int, error) {
	data, status, header, err := mg.doGraphFull(ctx, client, method, path, body)
	if err != nil {
		return data, status, err
	}
	delay, retry := graphRetryDelay(status, header)
	if !retry {
		return data, status, nil
	}
	select {
	case <-ctx.Done():
		return data, status, ctx.Err()
	case <-time.After(delay):
	}
	data, status, _, err = mg.doGraphFull(ctx, client, method, path, body)
	return data, status, err
}

// graphRetryDelay reports whether a response should be retried once, and how
// long to wait first. Only an explicit throttle qualifies: 429 always, 503
// when it carries a Retry-After — both mean the request was not processed, so
// resending it is safe for a non-idempotent write. A 5xx that is not a
// throttle is ambiguous and is not retried. The wait is the Retry-After
// seconds Graph names, else two seconds, capped at ten.
func graphRetryDelay(status int, header http.Header) (time.Duration, bool) {
	switch {
	case status == http.StatusTooManyRequests:
	case status == http.StatusServiceUnavailable && strings.TrimSpace(header.Get("Retry-After")) != "":
	default:
		return 0, false
	}
	delay := 2 * time.Second
	if secs, err := strconv.Atoi(strings.TrimSpace(header.Get("Retry-After"))); err == nil && secs > 0 {
		delay = time.Duration(secs) * time.Second
	}
	if delay > 10*time.Second {
		delay = 10 * time.Second
	}
	return delay, true
}

// graphErrorText renders a Graph error body for a message: the error's
// message when it is the usual {"error": {"code", "message"}}, the body
// otherwise, never longer than a line or two.
func graphErrorText(data []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		if e.Error.Code != "" {
			return e.Error.Code + ": " + e.Error.Message
		}
		return e.Error.Message
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 500 {
		// Cut on a rune boundary so the message stays valid UTF-8.
		s = strings.ToValidUTF8(s[:500], "") + "…"
	}
	return s
}
