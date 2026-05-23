//go:build live
// +build live

// Live integration test for cdp_evaluate. Runs only with:
//
//	HYPERBROWSER_API_KEY=hb_xxx go test -tags=live ./internal/tools/ -run TestLive -v -timeout=20m
//
// Burns Hyperbrowser credits — gated behind the build tag and env var.
// Per agent-mailbox/mux-cdp-evaluate/001+003, Simon's mandate for this
// test run is explicit: token-burn is OK, real-session acceptance is the
// release gate.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/smnhffmnn/mux/internal/config"
)

func liveAPIKey(t *testing.T) string {
	t.Helper()
	k := os.Getenv("HYPERBROWSER_API_KEY")
	if k == "" {
		t.Skip("HYPERBROWSER_API_KEY not set")
	}
	return k
}

func newLiveHyperbrowser(t *testing.T) *Hyperbrowser {
	t.Helper()
	hb, err := NewHyperbrowser(config.Connection{Token: liveAPIKey(t)}, nil)
	if err != nil {
		t.Fatalf("NewHyperbrowser: %v", err)
	}
	return hb
}

// createSession spins up a stealth+DE-proxy browser session via the public
// Hyperbrowser API. Returns the session id.
func createSession(t *testing.T, hb *Hyperbrowser, ctx context.Context, proxy bool) string {
	t.Helper()
	body := map[string]any{
		"useStealth":    true,
		"acceptCookies": true,
	}
	if proxy {
		body["useProxy"] = true
		body["proxyCountry"] = "DE"
		body["solveCaptchas"] = true
	}
	body["timeoutMinutes"] = 10

	data, status, err := hb.doJSON(ctx, http.MethodPost, "/api/session", body)
	if err != nil {
		t.Fatalf("session_create: %v", err)
	}
	if status < 200 || status >= 300 {
		t.Fatalf("session_create HTTP %d: %s", status, data)
	}
	var resp struct {
		ID         string `json:"id"`
		WSEndpoint string `json:"wsEndpoint"`
		LiveURL    string `json:"liveUrl"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("session_create parse: %v — body=%s", err, data)
	}
	if resp.ID == "" {
		t.Fatalf("session_create: empty id (body=%s)", data)
	}
	t.Logf("session created: id=%s liveUrl=%s wsEndpoint=%s", resp.ID, resp.LiveURL, resp.WSEndpoint)
	return resp.ID
}

func stopSession(t *testing.T, hb *Hyperbrowser, ctx context.Context, sessionID string) {
	t.Helper()
	data, status, err := hb.doJSON(ctx, http.MethodPut, "/api/session/"+sessionID+"/stop", nil)
	if err != nil {
		t.Logf("session_stop error (ignored): %v", err)
		return
	}
	t.Logf("session_stop HTTP %d body=%s", status, truncate(string(data), 200))
}

// startBrowserUseAgent kicks off a Browser-Use task and returns the jobId.
func startBrowserUseAgent(t *testing.T, hb *Hyperbrowser, ctx context.Context, sessionID, task string) string {
	t.Helper()
	body := map[string]any{
		"task":            task,
		"sessionId":       sessionID,
		"keepBrowserOpen": true,
		"llm":             "gemini-2.0-flash",
		"maxSteps":        15,
	}
	data, status, err := hb.doJSON(ctx, http.MethodPost, "/api/task/browser-use", body)
	if err != nil {
		t.Fatalf("agent_start: %v", err)
	}
	if status < 200 || status >= 300 {
		t.Fatalf("agent_start HTTP %d: %s", status, data)
	}
	var resp struct {
		JobID   string `json:"jobId"`
		LiveURL string `json:"liveUrl"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("agent_start parse: %v — body=%s", err, data)
	}
	t.Logf("agent started: jobId=%s liveUrl=%s task=%q", resp.JobID, resp.LiveURL, task)
	return resp.JobID
}

func waitForAgent(t *testing.T, hb *Hyperbrowser, ctx context.Context, jobID string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("agent_status: timed out after 8 min")
		}
		data, _, err := hb.doJSONLimit(ctx, http.MethodGet, "/api/task/browser-use/"+jobID, nil, hyperbrowserAgentMaxBody)
		if err != nil {
			t.Fatalf("agent_status: %v", err)
		}
		var parsed struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("agent_status parse: %v — body=%s", err, truncate(string(data), 500))
		}
		switch parsed.Status {
		case "completed":
			t.Logf("agent completed (jobId=%s)", jobID)
			return
		case "failed":
			t.Fatalf("agent failed: %s", parsed.Error)
		default:
			t.Logf("agent status=%s, sleeping", parsed.Status)
			time.Sleep(5 * time.Second)
		}
	}
}

// evalAndDump invokes the persistent-connection evaluate flow directly
// (bypassing the MCP request layer) and dumps the result.
func evalAndDump(t *testing.T, hb *Hyperbrowser, ctx context.Context, sessionID, expression, pattern string) *cdpEvaluateResult {
	t.Helper()
	evalCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res, err := hb.evaluateInSession(evalCtx, sessionID, pattern, expression, true, true, 30*time.Second)
	if err != nil {
		t.Fatalf("evaluateInSession: %v", err)
	}
	pretty, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("cdp_evaluate(%q, pattern=%q):\n%s", summariseExpression(expression), pattern, pretty)
	return res
}

// TestLiveCDPEvaluate_AgainstExampleCom is the primary acceptance test
// from briefing 001_konzept-agent.md: session → agent → 4× evaluate → stop.
func TestLiveCDPEvaluate_AgainstExampleCom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	hb := newLiveHyperbrowser(t)

	sessionID := createSession(t, hb, ctx, false /* no proxy */)
	defer stopSession(t, hb, ctx, sessionID)

	jobID := startBrowserUseAgent(t, hb, ctx, sessionID,
		"Go to https://example.com and stop. Do not click anything.")
	waitForAgent(t, hb, ctx, jobID)

	// 1) document.title
	r1 := evalAndDump(t, hb, ctx, sessionID, `document.title`, "")
	if r1.Error != nil {
		t.Errorf("step 1: unexpected error: %+v", r1.Error)
	}
	if r1.Type != "string" {
		t.Errorf("step 1: type=%q, want string", r1.Type)
	}
	if !strings.Contains(string(r1.Result), "Example") {
		t.Errorf("step 1: result=%s, want substring \"Example\"", r1.Result)
	}

	// 2) Array of hrefs.
	r2 := evalAndDump(t, hb, ctx, sessionID,
		`Array.from(document.querySelectorAll('a')).map(a => a.href)`, "")
	if r2.Error != nil {
		t.Errorf("step 2: unexpected error: %+v", r2.Error)
	}
	if r2.Type != "array" {
		t.Errorf("step 2: type=%q, want array", r2.Type)
	}
	if !strings.Contains(string(r2.Result), "iana.org") {
		t.Errorf("step 2: expected iana.org in result, got %s", r2.Result)
	}

	// 3) empty-match expression — should succeed with empty array.
	r3 := evalAndDump(t, hb, ctx, sessionID,
		`Array.from(document.querySelectorAll('a[href*="/expose/"]')).map(a => a.href)`, "")
	if r3.Error != nil {
		t.Errorf("step 3: unexpected error: %+v", r3.Error)
	}
	if r3.Type != "array" {
		t.Errorf("step 3: type=%q, want array", r3.Type)
	}
	if string(r3.Result) != "[]" {
		t.Errorf("step 3: expected empty array, got %s", r3.Result)
	}

	// 4) Provoke a ReferenceError.
	r4 := evalAndDump(t, hb, ctx, sessionID, `nonexistent.method()`, "")
	if r4.Error == nil {
		t.Errorf("step 4: expected error, got success result=%s type=%s", r4.Result, r4.Type)
	} else {
		if r4.Error.Type == "" {
			t.Errorf("step 4: error.type should be populated, got %+v", r4.Error)
		}
		if !strings.Contains(r4.Error.Message, "nonexistent") && !strings.Contains(r4.Error.Message, "not defined") {
			t.Errorf("step 4: error.message=%q, want mention of nonexistent/not-defined", r4.Error.Message)
		}
	}
}

// TestLiveCDPDiagnostic_PersistentConnect probes whether multiple
// Runtime.evaluate calls work on a SINGLE persistent WebSocket connection.
// If yes, the "per-call connection" design must move to "persistent per
// session".
func TestLiveCDPDiagnostic_PersistentConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	hb := newLiveHyperbrowser(t)
	sessionID := createSession(t, hb, ctx, false)
	defer stopSession(t, hb, ctx, sessionID)

	time.Sleep(5 * time.Second)

	ws, err := hb.fetchSessionWSEndpoint(ctx, sessionID)
	if err != nil {
		t.Fatalf("fetch wsEndpoint: %v", err)
	}

	// Open one WS, run multiple evaluates on it, close at the end.
	conn, _, err := websocket.Dial(ctx, ws, &websocket.DialOptions{HTTPClient: hb.client})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadLimit(hyperbrowserCDPReadLimit)
	defer conn.Close(websocket.StatusNormalClosure, "")

	client := &cdpClient{conn: conn, pending: map[int]chan cdpMessage{}}
	readerCtx, cancelReader := context.WithCancel(ctx)
	defer cancelReader()
	go client.readLoop(readerCtx)

	// Attach to first page target once.
	var gt struct {
		TargetInfos []cdpTargetInfo `json:"targetInfos"`
	}
	if err := client.call(ctx, "", "Target.getTargets", nil, &gt); err != nil {
		t.Fatalf("getTargets: %v", err)
	}
	chosen, _ := pickPageTarget(gt.TargetInfos, "")
	if chosen == nil {
		t.Fatalf("no page target available")
	}
	var attach struct {
		SessionID string `json:"sessionId"`
	}
	if err := client.call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": chosen.TargetID, "flatten": true}, &attach); err != nil {
		t.Fatalf("attach: %v", err)
	}

	// Three evaluates on the SAME connection.
	for i, expr := range []string{`1+1`, `"hello"`, `Math.PI`} {
		var resp struct {
			Result struct {
				Type  string          `json:"type"`
				Value json.RawMessage `json:"value"`
			} `json:"result"`
		}
		err := client.call(ctx, attach.SessionID, "Runtime.evaluate", map[string]any{
			"expression":    expr,
			"returnByValue": true,
		}, &resp)
		if err != nil {
			t.Errorf("eval %d (%s): %v", i+1, expr, err)
			continue
		}
		t.Logf("eval %d (%s): type=%s value=%s", i+1, expr, resp.Result.Type, resp.Result.Value)
	}
}

// TestLiveCDPDiagnostic_RepeatedConnect probes whether sequential CDP
// connections to the same wsEndpoint work — to isolate the 401 we saw on
// the second connect in the example.com test.
func TestLiveCDPDiagnostic_RepeatedConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	hb := newLiveHyperbrowser(t)
	sessionID := createSession(t, hb, ctx, false)
	defer stopSession(t, hb, ctx, sessionID)

	// Give Hyperbrowser ~5s to fully boot the browser before we try CDP.
	time.Sleep(5 * time.Second)

	for i := 1; i <= 4; i++ {
		evalCtx, c := context.WithTimeout(ctx, 30*time.Second)
		res, err := hb.evaluateInSession(evalCtx, sessionID, "", "1+1", true, true, 10*time.Second)
		c()
		if err != nil {
			t.Errorf("attempt %d: evaluateInSession: %v", i, err)
			continue
		}
		t.Logf("attempt %d: type=%s result=%s", i, res.Type, res.Result)
		time.Sleep(2 * time.Second)
	}
}

// TestLiveCDPEvaluate_AgainstImmoScout is the bonus test from
// 003_konzept-agent.md — proves the real use case (warm Imperva session +
// CDP DOM read of href attributes). Slower & burns proxy bandwidth.
//
// Skip with -short.
func TestLiveCDPEvaluate_AgainstImmoScout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ImmoScout bonus test in -short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	hb := newLiveHyperbrowser(t)

	sessionID := createSession(t, hb, ctx, true /* proxy DE */)
	defer stopSession(t, hb, ctx, sessionID)

	// Imperva-Detour-Pattern: navigate via homepage/search UI rather than
	// hitting /Suche/... directly.
	task := "Go to https://www.immobilienscout24.de. " +
		"On the homepage, use the search form to search for rental " +
		"apartments in Düsseldorf with at least 4 rooms. Submit the " +
		"search and wait until the result list with apartment cards is " +
		"visible. Then stop. Do not click on any individual listing."
	jobID := startBrowserUseAgent(t, hb, ctx, sessionID, task)
	waitForAgent(t, hb, ctx, jobID)

	res := evalAndDump(t, hb, ctx, sessionID,
		`Array.from(document.querySelectorAll('a[href*="/expose/"]')).slice(0, 3).map(a => a.href)`,
		"immobilienscout24.de",
	)
	if res.Error != nil {
		t.Fatalf("immoscout: cdp_evaluate error: %+v", res.Error)
	}
	if res.Type != "array" {
		t.Errorf("immoscout: type=%q, want array", res.Type)
	}
	// We don't fail hard if the array is empty (Imperva may still block,
	// or the agent may have ended up on a non-results page). But we log
	// loudly so Simon can read the verdict.
	if string(res.Result) == "[]" {
		t.Logf("immoscout: WARNING — querySelectorAll returned empty. Agent likely did not land on a results page. Inspect liveUrl.")
	} else {
		t.Logf("immoscout: SUCCESS — got hrefs: %s", res.Result)
	}
	if res.Target != nil {
		t.Logf("immoscout: target.url=%s target.title=%q", res.Target.URL, res.Target.Title)
	}

	// Sanity: scout_id extraction. Even if the array is empty, the expression
	// itself shouldn't error out.
	r2 := evalAndDump(t, hb, ctx, sessionID,
		`(Array.from(document.querySelectorAll('a[href*="/expose/"]')).slice(0, 5).map(a => {
			const m = a.href.match(/\/expose\/(\d+)/);
			return m ? m[1] : null;
		}).filter(Boolean))`,
		"immobilienscout24.de",
	)
	if r2.Error != nil {
		t.Errorf("immoscout scout_ids: error: %+v", r2.Error)
	}
	fmt.Printf("\n=== ImmoScout scout_ids (informational): %s ===\n", r2.Result)
}
