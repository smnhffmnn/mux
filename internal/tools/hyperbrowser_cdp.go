package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	hyperbrowserCDPDefaultTimeout = 30 * time.Second
	hyperbrowserCDPMaxTimeout     = 5 * time.Minute
	// hyperbrowserCDPReadLimit caps a single CDP frame. Runtime.evaluate can
	// return surprisingly large JSON (e.g. outerHTML across many DOM nodes);
	// 50 MB is well above realistic responses and below typical mux deployments'
	// memory pressure threshold.
	hyperbrowserCDPReadLimit = 50 * 1024 * 1024
)

// cdpMessage is a CDP request or response frame on the WebSocket.
type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *cdpProtoError  `json:"error,omitempty"`
}

type cdpProtoError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

// cdpEvaluateResult is the public response shape returned to the MCP caller.
type cdpEvaluateResult struct {
	Result          json.RawMessage   `json:"result,omitempty"`
	Type            string            `json:"type,omitempty"`
	ExecutionTimeMs int64             `json:"execution_time_ms"`
	Target          *cdpTargetSummary `json:"target,omitempty"`
	Error           *cdpEvaluateError `json:"error,omitempty"`
}

type cdpEvaluateError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Stack   string `json:"stack,omitempty"`
}

type cdpTargetSummary struct {
	TargetID string `json:"target_id"`
	URL      string `json:"url"`
	Title    string `json:"title,omitempty"`
}

type cdpTargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Attached bool   `json:"attached"`
}

// hyperbrowserSessionDetails is the trimmed shape of GET /api/session/{id}.
type hyperbrowserSessionDetails struct {
	ID         string `json:"id"`
	WSEndpoint string `json:"wsEndpoint"`
	Status     string `json:"status"`
}

// cdpSession is a long-lived CDP WebSocket bound to one Hyperbrowser session.
//
// Hyperbrowser only accepts ONE CDP WebSocket connection per session
// lifetime — once we disconnect, fresh tokens still get 401 on reconnect
// and shortly after the browser GUID is gone (404). Empirically verified;
// see TestLiveCDPDiagnostic_RepeatedConnect vs _PersistentConnect.
//
// The connection therefore stays open until handleSessionStop tears it
// down (or the WS reader observes a remote close and self-closes).
type cdpSession struct {
	hyperbrowserID string
	client         *cdpClient
	cancelReader   context.CancelFunc
	readerDone     chan struct{}
	closed         atomic.Bool
}

// handleCDPEvaluate evaluates a JavaScript expression in the page context of
// an active Hyperbrowser session via the Chrome DevTools Protocol.
func (h *Hyperbrowser) handleCDPEvaluate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sessionID, _ := req.RequireString("session_id")
	if sessionID == "" {
		return mcp.NewToolResultError("session_id is required"), nil
	}
	if !validJobID.MatchString(sessionID) {
		return mcp.NewToolResultError("session_id must contain only letters, digits, dashes, and underscores"), nil
	}
	expression, _ := req.RequireString("expression")
	if expression == "" {
		return mcp.NewToolResultError("expression is required"), nil
	}

	awaitPromise := true
	if v, ok := req.GetArguments()["await_promise"].(bool); ok {
		awaitPromise = v
	}
	returnByValue := true
	if v, ok := req.GetArguments()["return_by_value"].(bool); ok {
		returnByValue = v
	}

	evalTimeout := hyperbrowserCDPDefaultTimeout
	if v, ok := req.GetArguments()["timeout_ms"].(float64); ok && v > 0 {
		evalTimeout = time.Duration(v) * time.Millisecond
		if evalTimeout > hyperbrowserCDPMaxTimeout {
			evalTimeout = hyperbrowserCDPMaxTimeout
		}
	}
	urlPattern := strings.TrimSpace(req.GetString("target_url_pattern", ""))

	// Per-call ctx for the whole flow — generous, since the eval-internal
	// timeout is already enforced server-side via Runtime.evaluate's
	// "timeout" param.
	callCtx, cancel := context.WithTimeout(ctx, evalTimeout+30*time.Second)
	defer cancel()

	start := time.Now()
	res, err := h.evaluateInSession(callCtx, sessionID, urlPattern, expression, awaitPromise, returnByValue, evalTimeout)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		msg := fmt.Sprintf("cdp_evaluate failed: %v (expression=%s)", err, summariseExpression(expression))
		return mcp.NewToolResultError(msg), nil
	}
	res.ExecutionTimeMs = elapsed

	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal cdp_evaluate result: %v", err)), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

// evaluateInSession is the persistent-connection eval flow:
//
//  1. Get-or-create the cached CDP WS for this Hyperbrowser session.
//  2. Target.getTargets → pick first page (optionally filtered by pattern).
//  3. Target.attachToTarget (flatten) → CDP-session-id.
//  4. Runtime.evaluate routed via the CDP-session-id.
//
// Steps 2–4 are redone on every call so target selection stays correct
// across page navigations triggered between cdp_evaluate calls.
func (h *Hyperbrowser) evaluateInSession(ctx context.Context, hyperbrowserSessionID, urlPattern, expression string, awaitPromise, returnByValue bool, evalTimeout time.Duration) (*cdpEvaluateResult, error) {
	cs, err := h.getOrCreateCDPSession(ctx, hyperbrowserSessionID)
	if err != nil {
		return nil, err
	}

	// On any unrecoverable client error below, evict from the pool so the
	// next call retries with a fresh WS. Hyperbrowser only allows ONE such
	// retry per session — see the cdpSession docstring — but a stale WS
	// (e.g. after server timeout) should still be detected and surfaced.
	bail := func(err error) (*cdpEvaluateResult, error) {
		if errors.Is(err, errCDPClientClosed) {
			h.evictCDPSession(hyperbrowserSessionID, cs)
		}
		return nil, err
	}

	var gt struct {
		TargetInfos []cdpTargetInfo `json:"targetInfos"`
	}
	if err := cs.client.call(ctx, "", "Target.getTargets", nil, &gt); err != nil {
		return bail(fmt.Errorf("Target.getTargets: %w", err))
	}

	chosen, pages := pickPageTarget(gt.TargetInfos, urlPattern)
	if chosen == nil {
		return nil, fmt.Errorf("no matching page target (pattern=%q, page-targets-available=%v)", urlPattern, pages)
	}

	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := cs.client.call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": chosen.TargetID, "flatten": true}, &attach); err != nil {
		return bail(fmt.Errorf("Target.attachToTarget: %w", err))
	}
	if attach.SessionID == "" {
		return nil, fmt.Errorf("Target.attachToTarget returned empty sessionId")
	}
	// Detach when we're done so the per-call attach doesn't leak across
	// subsequent cdp_evaluate calls. Best-effort; errors here are logged
	// via stderr if MUX_CDP_DEBUG is set but otherwise ignored.
	defer func() {
		detachCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = cs.client.call(detachCtx, "", "Target.detachFromTarget",
			map[string]any{"sessionId": attach.SessionID}, nil)
	}()

	var evalResp struct {
		Result struct {
			Type        string          `json:"type"`
			Subtype     string          `json:"subtype,omitempty"`
			Value       json.RawMessage `json:"value,omitempty"`
			Description string          `json:"description,omitempty"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				ClassName   string          `json:"className,omitempty"`
				Description string          `json:"description,omitempty"`
				Value       json.RawMessage `json:"value,omitempty"`
			} `json:"exception,omitempty"`
			StackTrace *struct {
				CallFrames []struct {
					FunctionName string `json:"functionName"`
					URL          string `json:"url"`
					LineNumber   int    `json:"lineNumber"`
					ColumnNumber int    `json:"columnNumber"`
				} `json:"callFrames"`
			} `json:"stackTrace,omitempty"`
		} `json:"exceptionDetails,omitempty"`
	}
	evalParams := map[string]any{
		"expression":    expression,
		"awaitPromise":  awaitPromise,
		"returnByValue": returnByValue,
		"timeout":       evalTimeout.Milliseconds(),
	}
	if err := cs.client.call(ctx, attach.SessionID, "Runtime.evaluate", evalParams, &evalResp); err != nil {
		return bail(fmt.Errorf("Runtime.evaluate: %w", err))
	}

	out := &cdpEvaluateResult{
		Target: &cdpTargetSummary{
			TargetID: chosen.TargetID,
			URL:      chosen.URL,
			Title:    chosen.Title,
		},
	}

	if evalResp.ExceptionDetails != nil {
		ed := evalResp.ExceptionDetails
		out.Error = &cdpEvaluateError{Message: strings.TrimSpace(ed.Text)}
		if ed.Exception != nil {
			out.Error.Type = ed.Exception.ClassName
			desc := strings.TrimSpace(ed.Exception.Description)
			// Exception.description usually carries the full "TypeError: foo
			// is not a function" plus a JS stack; prefer it when present.
			if desc != "" {
				if firstLine, rest, ok := strings.Cut(desc, "\n"); ok {
					out.Error.Message = firstLine
					if out.Error.Stack == "" {
						out.Error.Stack = rest
					}
				} else {
					out.Error.Message = desc
				}
			}
		}
		if out.Error.Stack == "" && ed.StackTrace != nil && len(ed.StackTrace.CallFrames) > 0 {
			var sb strings.Builder
			for _, f := range ed.StackTrace.CallFrames {
				fmt.Fprintf(&sb, "    at %s (%s:%d:%d)\n", f.FunctionName, f.URL, f.LineNumber, f.ColumnNumber)
			}
			out.Error.Stack = strings.TrimRight(sb.String(), "\n")
		}
		return out, nil
	}

	out.Type = detectResultType(evalResp.Result.Value, evalResp.Result.Type, evalResp.Result.Subtype)
	out.Result = evalResp.Result.Value
	return out, nil
}

// getOrCreateCDPSession returns the cached cdpSession for the given
// Hyperbrowser session id, creating the WebSocket on first use.
func (h *Hyperbrowser) getOrCreateCDPSession(ctx context.Context, hyperbrowserSessionID string) (*cdpSession, error) {
	h.cdpMu.Lock()
	if cs, ok := h.cdpSessions[hyperbrowserSessionID]; ok && !cs.closed.Load() {
		h.cdpMu.Unlock()
		return cs, nil
	}
	// If a stale entry sits here, drop it before we attempt a fresh dial.
	if cs, ok := h.cdpSessions[hyperbrowserSessionID]; ok {
		delete(h.cdpSessions, hyperbrowserSessionID)
		// Defensive close of any leftover WS so we don't leak the goroutine.
		go cs.shutdown()
	}
	h.cdpMu.Unlock()

	wsEndpoint, err := h.fetchSessionWSEndpoint(ctx, hyperbrowserSessionID)
	if err != nil {
		return nil, err
	}
	if dbg := os.Getenv("MUX_CDP_DEBUG"); dbg != "" {
		fmt.Fprintf(os.Stderr, "[cdp] dial(session=%s): %s\n", hyperbrowserSessionID, wsEndpoint)
	}
	conn, _, err := websocket.Dial(ctx, wsEndpoint, &websocket.DialOptions{HTTPClient: h.client})
	if err != nil {
		return nil, fmt.Errorf("connect to wsEndpoint: %w", err)
	}
	conn.SetReadLimit(hyperbrowserCDPReadLimit)

	// The WS must outlive the per-call ctx — readerCtx is therefore derived
	// from Background. cancelReader is fired by shutdown() or evict.
	readerCtx, cancelReader := context.WithCancel(context.Background())
	client := &cdpClient{conn: conn, pending: map[int]chan cdpMessage{}}
	cs := &cdpSession{
		hyperbrowserID: hyperbrowserSessionID,
		client:         client,
		cancelReader:   cancelReader,
		readerDone:     make(chan struct{}),
	}
	go func() {
		client.readLoop(readerCtx)
		close(cs.readerDone)
		cs.closed.Store(true)
	}()

	h.cdpMu.Lock()
	// Race: another call might have created an entry in the meantime. If so,
	// our just-opened WS is wasted — close it and use the winner. This is
	// rare; the wasted dial costs a few hundred ms once.
	if existing, ok := h.cdpSessions[hyperbrowserSessionID]; ok && !existing.closed.Load() {
		h.cdpMu.Unlock()
		go cs.shutdown()
		return existing, nil
	}
	h.cdpSessions[hyperbrowserSessionID] = cs
	h.cdpMu.Unlock()
	return cs, nil
}

// evictCDPSession removes the entry from the pool and shuts it down. Used
// when a call detected a closed/broken WS.
func (h *Hyperbrowser) evictCDPSession(hyperbrowserSessionID string, cs *cdpSession) {
	h.cdpMu.Lock()
	if h.cdpSessions[hyperbrowserSessionID] == cs {
		delete(h.cdpSessions, hyperbrowserSessionID)
	}
	h.cdpMu.Unlock()
	cs.shutdown()
}

// closeCDPSession is called from handleSessionStop. Drops the cached entry
// and tears down the WS cleanly before the Hyperbrowser session is killed.
func (h *Hyperbrowser) closeCDPSession(hyperbrowserSessionID string) {
	h.cdpMu.Lock()
	cs, ok := h.cdpSessions[hyperbrowserSessionID]
	if ok {
		delete(h.cdpSessions, hyperbrowserSessionID)
	}
	h.cdpMu.Unlock()
	if ok {
		cs.shutdown()
	}
}

func (cs *cdpSession) shutdown() {
	// Try a graceful close first; if it errors (already-closed conn), the
	// next Close call is a no-op.
	_ = cs.client.conn.Close(websocket.StatusNormalClosure, "")
	cs.cancelReader()
	<-cs.readerDone
}

// fetchSessionWSEndpoint looks up the active session and returns its
// CDP wsEndpoint. Returns a clear error if the session is stopped or unknown.
func (h *Hyperbrowser) fetchSessionWSEndpoint(ctx context.Context, sessionID string) (string, error) {
	apiCtx, cancel := context.WithTimeout(ctx, hyperbrowserSessionTimeout)
	defer cancel()

	data, status, err := h.doJSON(apiCtx, http.MethodGet, "/api/session/"+sessionID, nil)
	if err != nil {
		return "", fmt.Errorf("lookup session: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("session lookup failed (HTTP %d): %s", status, truncate(string(data), 300))
	}
	var sd hyperbrowserSessionDetails
	if err := json.Unmarshal(data, &sd); err != nil {
		return "", fmt.Errorf("parse session response: %w", err)
	}
	if sd.WSEndpoint == "" {
		return "", fmt.Errorf("session %s has no wsEndpoint (status=%q) — session may be stopped or still launching", sessionID, sd.Status)
	}
	return sd.WSEndpoint, nil
}

// pickPageTarget picks the first page target matching urlPattern (substring
// match against target.URL). If urlPattern is empty, the first page target
// wins. Returns nil + the slice of page URLs we considered so callers can
// build a useful error message.
func pickPageTarget(targets []cdpTargetInfo, urlPattern string) (*cdpTargetInfo, []string) {
	var pages []string
	for i := range targets {
		if targets[i].Type != "page" {
			continue
		}
		pages = append(pages, targets[i].URL)
		if urlPattern != "" && !strings.Contains(targets[i].URL, urlPattern) {
			continue
		}
		return &targets[i], pages
	}
	return nil, pages
}

// mapCDPResultType collapses the CDP {type, subtype} pair into the
// flatter shape we advertise. Kept for the no-value-available path
// (e.g. when returnByValue is false).
func mapCDPResultType(t, subtype string) string {
	if t == "object" {
		switch subtype {
		case "array":
			return "array"
		case "null":
			return "null"
		}
	}
	return t
}

// detectResultType picks the most useful type label by combining the CDP
// RemoteObject {type, subtype} fields with a peek at the serialized value.
//
// Empirical: when Runtime.evaluate is called with returnByValue=true,
// Hyperbrowser's Chrome leaves subtype empty for arrays — so the {type:
// "object", subtype: ""} pair alone can't distinguish {} from []. We peek
// at the first non-whitespace byte of value to disambiguate.
func detectResultType(value json.RawMessage, cdpType, cdpSubtype string) string {
	// Authoritative subtype wins.
	switch cdpSubtype {
	case "array":
		return "array"
	case "null":
		return "null"
	}
	// Scalars and well-defined non-object types: trust CDP.
	switch cdpType {
	case "string", "number", "boolean", "undefined", "function", "bigint", "symbol":
		return cdpType
	}
	// Object type with no usable subtype — infer from the JSON value's
	// first byte. This is safe because returnByValue serialises the value
	// as JSON and we have access to it.
	v := bytes.TrimSpace([]byte(value))
	if len(v) == 0 {
		return cdpType
	}
	switch v[0] {
	case '[':
		return "array"
	case '{':
		return "object"
	case 'n':
		return "null"
	case 't', 'f':
		return "boolean"
	case '"':
		return "string"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "number"
	}
	return cdpType
}

// summariseExpression keeps log/error output bounded. The expression can
// contain secrets (e.g. evaluate(token)) — we truncate aggressively.
func summariseExpression(expr string) string {
	const max = 80
	expr = strings.Join(strings.Fields(expr), " ")
	if len(expr) <= max {
		return expr
	}
	return expr[:max] + "…"
}

// --- CDP client ---

// errCDPClientClosed signals that the WebSocket reader died and the cached
// session should be evicted by the caller.
var errCDPClientClosed = errors.New("cdp client closed")

// cdpClient multiplexes request/response pairs on a single WebSocket. The
// reader goroutine drains messages and dispatches them to per-call channels;
// callers do not read from the socket directly.
type cdpClient struct {
	conn    *websocket.Conn
	nextID  int64
	mu      sync.Mutex
	pending map[int]chan cdpMessage
	closed  atomic.Bool
}

func (c *cdpClient) readLoop(ctx context.Context) {
	for {
		var msg cdpMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			c.closed.Store(true)
			// Fail all pending calls so callers don't deadlock.
			c.mu.Lock()
			for id, ch := range c.pending {
				select {
				case ch <- cdpMessage{ID: id, Error: &cdpProtoError{Message: "websocket closed: " + err.Error()}}:
				default:
				}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		if msg.ID == 0 {
			// Unsolicited CDP event — ignore.
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[msg.ID]
		if ok {
			delete(c.pending, msg.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- msg
		}
	}
}

// call sends a CDP request and waits for its matching response. sessionID
// empty = browser-level call; non-empty = routed to a flat-attached target.
//
// Returns errCDPClientClosed (wrapped) when the underlying WS has died —
// callers should evict the cached cdpSession on that signal.
func (c *cdpClient) call(ctx context.Context, sessionID, method string, params, result any) error {
	if c.closed.Load() {
		return fmt.Errorf("%s: %w", method, errCDPClientClosed)
	}
	id := int(atomic.AddInt64(&c.nextID, 1))

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
		rawParams = b
	}

	req := cdpMessage{ID: id, Method: method, Params: rawParams, SessionID: sessionID}

	ch := make(chan cdpMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := wsjson.Write(ctx, c.conn, req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		if c.closed.Load() {
			return fmt.Errorf("write %s: %w", method, errCDPClientClosed)
		}
		return fmt.Errorf("write %s: %w", method, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			// "websocket closed: ..." messages come from readLoop bailing.
			if strings.HasPrefix(resp.Error.Message, "websocket closed:") {
				return fmt.Errorf("%s: %w (%s)", method, errCDPClientClosed, resp.Error.Message)
			}
			return fmt.Errorf("CDP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("unmarshal %s response: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s timed out: %w", method, ctx.Err())
	}
}
