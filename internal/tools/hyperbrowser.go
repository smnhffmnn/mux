package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const (
	hyperbrowserMaxBody            = 5 * 1024 * 1024 // 5 MB — screenshots & full-page HTML
	hyperbrowserScrapeTimeout      = 90 * time.Second
	hyperbrowserExtractTimeout     = 180 * time.Second
	hyperbrowserCrawlStartTimeout  = 30 * time.Second
	hyperbrowserCrawlStatusTimeout = 30 * time.Second
	hyperbrowserSessionTimeout     = 30 * time.Second // session_create/stop/list, agent_start/status
	hyperbrowserPollInterval       = 2 * time.Second
)

// validJobID restricts API job IDs to alphanumerics, dashes, and underscores
// so user-supplied job_id / session_id values cannot inject path segments
// or query strings into URLs we build.
var validJobID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// DefaultHyperbrowserInstructions are used when no custom instructions are set.
const DefaultHyperbrowserInstructions = `Hyperbrowser — stealth headless Chrome with residential proxy rotation, CAPTCHA solving, and a Browser-Use agent for sites where direct scrape is blocked.

Use when Firecrawl fails on Anti-Bot-protected sites (Imperva, Cloudflare,
DataDome) or JS-heavy SPAs. For plain HTTP scrapes prefer Firecrawl — cheaper
and faster.

Web-scraping tools (stateless):
- scrape         — Single URL → markdown / html / links / screenshot. Blocks (max 90s).
- extract        — Structured data with JSON schema + prompt. Blocks (max 180s).
- crawl          — Multi-page from a URL. Async — returns jobId.
- crawl_status   — Poll a crawl job; returns results once status is completed.

Session lifecycle (persistent browser):
- session_create — Start a persistent browser session. Returns id + liveUrl (VNC).
- session_stop   — Stop a session. ALWAYS call this when done — sessions are billed while active.
- session_list   — List active sessions.

Agent (when direct scrape is blocked):
- browser_use_agent_start  — LLM-driven browser agent that navigates the page like a human (clicks, scrolls, types). Returns jobId + liveUrl.
- browser_use_agent_status — Poll an agent task; returns the steps it took and the final result.

Session options (pass on scrape/extract/crawl):
- use_stealth    — fingerprint masking (recommended for Anti-Bot sites)
- use_proxy      — residential proxy rotation (PAID plan)
- solve_captchas — auto-solve CAPTCHAs (PAID plan)
- accept_cookies — auto-dismiss cookie banners
- proxy_country  — 2-letter country code (e.g. "DE", "US"); implies use_proxy
- session_id     — reuse a session previously created with session_create

When to use the agent: direct scrape (even with stealth+proxy+captchas) is
blocked on sites that profile mouse and behaviour in real time (e.g. Imperva
on ImmoScout24). The agent navigates the site via the actual UI and is much
harder to fingerprint as a bot. Trade-off: ~$0.10 per run + LLM tokens, and
~5–10 minutes per task. Only use when stateless scrape has actually failed.`

// Hyperbrowser wraps the Hyperbrowser REST API as MCP tools.
type Hyperbrowser struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewHyperbrowser creates a Hyperbrowser connection from config.
func NewHyperbrowser(conn config.Connection, dialer Dialer) (*Hyperbrowser, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("hyperbrowser: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://api.hyperbrowser.ai"
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Hyperbrowser{
		client: &http.Client{
			Transport: transport,
			// No client-level timeout — per-job ctx deadlines bound polling instead.
			// A short Timeout here would kill long-running waitForJob loops.
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the Hyperbrowser connection.
func (h *Hyperbrowser) Tools() []ToolDef {
	sessionOptsDesc := "Comma-separated session toggles: use_stealth, use_proxy, solve_captchas, accept_cookies. Example: \"use_stealth,accept_cookies\""
	sessionIDDesc := "Reuse an existing browser session (from session_create). Carries cookies and storage forward. If omitted, a fresh session is created for this call."

	return []ToolDef{
		{
			Tool: mcp.NewTool("scrape",
				mcp.WithDescription("Scrape a single URL with a stealth headless browser. Blocks until the job is complete (max ~90s); returns markdown by default. Use for Anti-Bot-protected sites or JS-heavy SPAs that fail with Firecrawl."),
				mcp.WithString("url", mcp.Required(), mcp.Description("The URL to scrape")),
				mcp.WithString("formats", mcp.Description("Comma-separated output formats: markdown, html, links, screenshot. Default: markdown")),
				mcp.WithBoolean("only_main_content", mcp.Description("Extract only the main content (default: true)")),
				mcp.WithString("session_options", mcp.Description(sessionOptsDesc)),
				mcp.WithString("proxy_country", mcp.Description("Two-letter country code for the proxy (only when use_proxy is set). Example: DE, US, GB.")),
				mcp.WithString("session_id", mcp.Description(sessionIDDesc)),
			),
			Handler: h.handleScrape,
		},
		{
			Tool: mcp.NewTool("extract",
				mcp.WithDescription("Extract structured data from one or more URLs using AI. Provide a JSON schema and/or a prompt — the schema defines the output shape, the prompt guides extraction. Blocks until the job is complete (max ~180s)."),
				mcp.WithString("urls", mcp.Required(), mcp.Description("Comma-separated URLs to extract from. Wildcards supported (e.g. https://example.com/products/*)")),
				mcp.WithString("prompt", mcp.Description("Natural-language extraction instructions. Required if schema is omitted.")),
				mcp.WithString("schema", mcp.Description("JSON schema describing the desired output shape. Required if prompt is omitted.")),
				mcp.WithString("system_prompt", mcp.Description("Optional system prompt to override default extraction behaviour.")),
				mcp.WithString("session_options", mcp.Description(sessionOptsDesc)),
				mcp.WithString("proxy_country", mcp.Description("Two-letter country code for the proxy.")),
				mcp.WithString("session_id", mcp.Description(sessionIDDesc)),
			),
			Handler: h.handleExtract,
		},
		{
			Tool: mcp.NewTool("crawl",
				mcp.WithDescription("Start a multi-page crawl from a URL. Returns a jobId immediately — use crawl_status to poll for results. Crawls can take minutes for large maxPages values."),
				mcp.WithString("url", mcp.Required(), mcp.Description("Starting URL for the crawl")),
				mcp.WithNumber("max_pages", mcp.Description("Maximum pages to crawl (default: 10, max: 100)")),
				mcp.WithBoolean("follow_links", mcp.Description("Whether to follow links found on crawled pages (default: true)")),
				mcp.WithBoolean("ignore_sitemap", mcp.Description("Skip the site's sitemap.xml (default: false)")),
				mcp.WithString("formats", mcp.Description("Comma-separated scrape output formats per page: markdown, html, links. Default: markdown")),
				mcp.WithString("session_options", mcp.Description(sessionOptsDesc)),
				mcp.WithString("proxy_country", mcp.Description("Two-letter country code for the proxy.")),
				mcp.WithString("session_id", mcp.Description(sessionIDDesc)),
			),
			Handler: h.handleCrawl,
		},
		{
			Tool: mcp.NewTool("crawl_status",
				mcp.WithDescription("Check the status of a crawl job and retrieve results when completed. Status is one of: pending, running, completed, failed."),
				mcp.WithString("job_id", mcp.Required(), mcp.Description("Crawl job ID returned by the crawl tool")),
			),
			Handler: h.handleCrawlStatus,
		},

		// --- Session lifecycle ---
		{
			Tool: mcp.NewTool("session_create",
				mcp.WithDescription("Start a persistent browser session. Returns the session id and a liveUrl for VNC-style debugging. IMPORTANT: sessions are billed while active — call session_stop when done."),
				mcp.WithString("session_options", mcp.Description(sessionOptsDesc)),
				mcp.WithString("proxy_country", mcp.Description("Two-letter country code for the proxy. Implies use_proxy.")),
				mcp.WithNumber("timeout_minutes", mcp.Description("Auto-stop the session after this many minutes if not stopped explicitly (1–720).")),
				mcp.WithString("profile_id", mcp.Description("Optional ID of a previously-created browser profile to attach to this session.")),
			),
			Handler: h.handleSessionCreate,
		},
		{
			Tool: mcp.NewTool("session_stop",
				mcp.WithDescription("Stop a running browser session. Always call this when finished — sessions are billed while active."),
				mcp.WithString("session_id", mcp.Required(), mcp.Description("Session ID from session_create.")),
			),
			Handler: h.handleSessionStop,
		},
		{
			Tool: mcp.NewTool("session_list",
				mcp.WithDescription("List all active browser sessions on the account. Use to find sessions that are still running and may need to be stopped."),
			),
			Handler: h.handleSessionList,
		},

		// --- Browser-Use Agent (LLM-driven navigation) ---
		{
			Tool: mcp.NewTool("browser_use_agent_start",
				mcp.WithDescription("Start a Browser-Use agent task. An LLM drives a real browser session (clicking, scrolling, typing) — use when stateless scrape is blocked by behaviour profiling (Imperva on ImmoScout, etc.). Returns jobId + liveUrl. Async — poll with browser_use_agent_status. Costs ~$0.10/run + LLM tokens, typically 5–10 minutes per task."),
				mcp.WithString("task", mcp.Required(), mcp.Description("Natural-language instructions for the agent (e.g. \"Go to immoscout24.de, search for 4-room apartments in Düsseldorf, return the URLs of the first 10 results\").")),
				mcp.WithString("llm", mcp.Description("Model to drive the agent. Available: gpt-4o, gpt-4o-mini, gpt-4.1, gpt-4.1-mini, claude-sonnet-4-6, claude-sonnet-4-5, gemini-2.0-flash, gemini-2.5-flash. Default: gemini-2.0-flash (fast, cheap).")),
				mcp.WithString("session_id", mcp.Description("Reuse an existing browser session. Carries cookies/storage forward.")),
				mcp.WithBoolean("keep_browser_open", mcp.Description("Keep the underlying session alive after the task finishes (so you can hand the session_id to subsequent calls). Default: false. If true, REMEMBER to session_stop afterwards.")),
				mcp.WithNumber("max_steps", mcp.Description("Maximum agent steps before giving up (default: 20).")),
				mcp.WithString("session_options", mcp.Description(sessionOptsDesc+" (ignored if session_id is set — the existing session keeps its original options)")),
				mcp.WithString("proxy_country", mcp.Description("Two-letter country code for the proxy (ignored if session_id is set).")),
			),
			Handler: h.handleAgentStart,
		},
		{
			Tool: mcp.NewTool("browser_use_agent_status",
				mcp.WithDescription("Poll a Browser-Use agent task. Returns status, the steps taken (with model decisions, actions, and page state), and the final result when completed. Screenshots are stripped server-side to keep response sizes manageable — open the liveUrl from start to watch live."),
				mcp.WithString("job_id", mcp.Required(), mcp.Description("Job ID from browser_use_agent_start.")),
			),
			Handler: h.handleAgentStatus,
		},
	}
}

// --- shared HTTP helpers ---

// doJSON sends a JSON request, reads the body up to hyperbrowserMaxBody, and
// returns the raw body, status code, and any transport error.
func (h *Hyperbrowser) doJSON(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-api-key", h.apiKey)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, hyperbrowserMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

// startJob posts to a job endpoint and returns the assigned jobId.
func (h *Hyperbrowser) startJob(ctx context.Context, path string, body any) (string, error) {
	data, status, err := h.doJSON(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse jobId response: %w", err)
	}
	if resp.JobID == "" {
		return "", fmt.Errorf("API returned empty jobId: %s", truncate(string(data), 200))
	}
	return resp.JobID, nil
}

// jobStatusBody is the common shape of the job-result endpoints.
type jobStatusBody struct {
	Status string          `json:"status"`
	Error  string          `json:"error"`
	Data   json.RawMessage `json:"data"`
}

// errJobNotReady is returned by getJob when the job is still pending/running.
var errJobNotReady = errors.New("job not ready")

// getJob fetches a job result. Returns the parsed status, the raw body for
// pass-through, or errJobNotReady if status is pending/running.
func (h *Hyperbrowser) getJob(ctx context.Context, path string) ([]byte, *jobStatusBody, error) {
	data, status, err := h.doJSON(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return data, nil, fmt.Errorf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))
	}
	var parsed jobStatusBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		return data, nil, fmt.Errorf("parse job status: %w", err)
	}
	switch parsed.Status {
	case "completed":
		return data, &parsed, nil
	case "failed":
		msg := parsed.Error
		if msg == "" {
			msg = "job failed without error message"
		}
		return data, &parsed, fmt.Errorf("job failed: %s", msg)
	case "pending", "running", "":
		return data, &parsed, errJobNotReady
	default:
		return data, &parsed, fmt.Errorf("unknown job status %q", parsed.Status)
	}
}

// waitForJob polls a job endpoint until it completes, fails, or the context expires.
func (h *Hyperbrowser) waitForJob(ctx context.Context, path string) ([]byte, error) {
	for {
		data, _, err := h.getJob(ctx, path)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, errJobNotReady) {
			return data, err
		}
		select {
		case <-ctx.Done():
			return data, fmt.Errorf("polling timed out before completion: %w", ctx.Err())
		case <-time.After(hyperbrowserPollInterval):
			// continue polling
		}
	}
}

// --- session options ---

// buildSessionOptions converts the flat tool params into the API's
// sessionOptions object. Returns (opts, error) — error is non-nil only
// when an invalid session_id is supplied. Returns nil opts if no options
// were provided.
func buildSessionOptions(req mcp.CallToolRequest) (map[string]any, error) {
	opts := map[string]any{}

	if raw := req.GetString("session_options", ""); raw != "" {
		for _, tok := range strings.Split(raw, ",") {
			switch strings.TrimSpace(strings.ToLower(tok)) {
			case "use_stealth", "stealth":
				opts["useStealth"] = true
			case "use_proxy", "proxy":
				opts["useProxy"] = true
			case "solve_captchas", "captcha":
				opts["solveCaptchas"] = true
			case "accept_cookies", "cookies":
				opts["acceptCookies"] = true
			}
		}
	}

	if country := req.GetString("proxy_country", ""); country != "" {
		opts["proxyCountry"] = strings.ToUpper(country)
		// Country implies proxy use; the API rejects proxyCountry without useProxy.
		if _, ok := opts["useProxy"]; !ok {
			opts["useProxy"] = true
		}
	}

	if sid := req.GetString("session_id", ""); sid != "" {
		if !validJobID.MatchString(sid) {
			return nil, fmt.Errorf("session_id must contain only letters, digits, dashes, and underscores")
		}
		opts["sessionId"] = sid
	}

	if len(opts) == 0 {
		return nil, nil
	}
	return opts, nil
}

// buildScrapeOptions parses the formats + onlyMainContent params.
// Defaults to ["markdown"] when nothing is specified.
func buildScrapeOptions(req mcp.CallToolRequest) map[string]any {
	opts := map[string]any{}
	formats := parseFormats(req.GetString("formats", ""))
	opts["formats"] = formats

	if v, ok := req.GetArguments()["only_main_content"].(bool); ok {
		opts["onlyMainContent"] = v
	}
	return opts
}

func parseFormats(raw string) []string {
	if raw == "" {
		return []string{"markdown"}
	}
	var out []string
	for _, f := range strings.Split(raw, ",") {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{"markdown"}
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// --- handlers ---

func (h *Hyperbrowser) handleScrape(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	url, _ := req.RequireString("url")
	if url == "" {
		return mcp.NewToolResultError("url is required"), nil
	}

	payload := map[string]any{
		"url":           url,
		"scrapeOptions": buildScrapeOptions(req),
	}
	so, err := buildSessionOptions(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if so != nil {
		payload["sessionOptions"] = so
	}

	jobCtx, cancel := context.WithTimeout(ctx, hyperbrowserScrapeTimeout)
	defer cancel()

	jobID, err := h.startJob(jobCtx, "/api/scrape", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	data, err := h.waitForJob(jobCtx, "/api/scrape/"+jobID)
	if err != nil {
		msg := fmt.Sprintf("%v (jobId=%s)", err, jobID)
		if errors.Is(err, context.DeadlineExceeded) {
			msg += "\n\nThe scrape is still running on Hyperbrowser's side. Retry; if it keeps timing out, try different session options (use_stealth, etc.)."
		}
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (h *Hyperbrowser) handleExtract(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	urlsRaw, _ := req.RequireString("urls")
	if urlsRaw == "" {
		return mcp.NewToolResultError("urls is required"), nil
	}
	var urls []string
	for _, u := range strings.Split(urlsRaw, ",") {
		u = strings.TrimSpace(u)
		if u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return mcp.NewToolResultError("urls is required"), nil
	}

	prompt := strings.TrimSpace(req.GetString("prompt", ""))
	schemaRaw := strings.TrimSpace(req.GetString("schema", ""))
	if prompt == "" && schemaRaw == "" {
		return mcp.NewToolResultError("either prompt or schema (or both) must be provided"), nil
	}

	payload := map[string]any{"urls": urls}
	if prompt != "" {
		payload["prompt"] = prompt
	}
	if schemaRaw != "" {
		var schema any
		if err := json.Unmarshal([]byte(schemaRaw), &schema); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("schema is not valid JSON: %v", err)), nil
		}
		payload["schema"] = schema
	}
	if sp := strings.TrimSpace(req.GetString("system_prompt", "")); sp != "" {
		payload["systemPrompt"] = sp
	}
	so, err := buildSessionOptions(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if so != nil {
		payload["sessionOptions"] = so
	}

	jobCtx, cancel := context.WithTimeout(ctx, hyperbrowserExtractTimeout)
	defer cancel()

	jobID, err := h.startJob(jobCtx, "/api/extract", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	data, err := h.waitForJob(jobCtx, "/api/extract/"+jobID)
	if err != nil {
		msg := fmt.Sprintf("%v (jobId=%s)", err, jobID)
		if errors.Is(err, context.DeadlineExceeded) {
			msg += "\n\nThe extract is still running on Hyperbrowser's side. Retry; if it keeps timing out, narrow the schema or reduce the URL list."
		}
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (h *Hyperbrowser) handleCrawl(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	url, _ := req.RequireString("url")
	if url == "" {
		return mcp.NewToolResultError("url is required"), nil
	}

	payload := map[string]any{
		"url":           url,
		"scrapeOptions": buildScrapeOptions(req),
	}
	if v, ok := req.GetArguments()["max_pages"].(float64); ok && v > 0 {
		payload["maxPages"] = int(v)
	}
	if v, ok := req.GetArguments()["follow_links"].(bool); ok {
		payload["followLinks"] = v
	}
	if v, ok := req.GetArguments()["ignore_sitemap"].(bool); ok {
		payload["ignoreSitemap"] = v
	}
	so, err := buildSessionOptions(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if so != nil {
		payload["sessionOptions"] = so
	}

	startCtx, cancel := context.WithTimeout(ctx, hyperbrowserCrawlStartTimeout)
	defer cancel()

	jobID, err := h.startJob(startCtx, "/api/crawl", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result := map[string]any{
		"jobId":  jobID,
		"status": "pending",
		"poll":   fmt.Sprintf("Use crawl_status with job_id=%s to retrieve results.", jobID),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}

func (h *Hyperbrowser) handleCrawlStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	jobID, _ := req.RequireString("job_id")
	if jobID == "" {
		return mcp.NewToolResultError("job_id is required"), nil
	}
	if !validJobID.MatchString(jobID) {
		return mcp.NewToolResultError("job_id must contain only letters, digits, dashes, and underscores"), nil
	}

	statusCtx, cancel := context.WithTimeout(ctx, hyperbrowserCrawlStatusTimeout)
	defer cancel()

	data, parsed, err := h.getJob(statusCtx, "/api/crawl/"+jobID)
	if err != nil {
		if errors.Is(err, errJobNotReady) {
			status := "pending"
			if parsed != nil && parsed.Status != "" {
				status = parsed.Status
			}
			out, _ := json.Marshal(map[string]string{"jobId": jobID, "status": status})
			return mcp.NewToolResultText(string(out)), nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// --- Session lifecycle handlers ---

func (h *Hyperbrowser) handleSessionCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// session_create takes a FLAT body (useStealth, useProxy, …) — not nested
	// under sessionOptions. So we reuse the parser and merge into the payload.
	opts, err := buildSessionOptions(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	payload := map[string]any{}
	for k, v := range opts {
		// session_id only makes sense for re-use endpoints, not for create.
		if k == "sessionId" {
			continue
		}
		payload[k] = v
	}

	if v, ok := req.GetArguments()["timeout_minutes"].(float64); ok && v > 0 {
		payload["timeoutMinutes"] = int(v)
	}
	if pid := strings.TrimSpace(req.GetString("profile_id", "")); pid != "" {
		payload["profile"] = map[string]any{"id": pid}
	}

	apiCtx, cancel := context.WithTimeout(ctx, hyperbrowserSessionTimeout)
	defer cancel()

	data, status, err := h.doJSON(apiCtx, http.MethodPost, "/api/session", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (h *Hyperbrowser) handleSessionStop(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sid, _ := req.RequireString("session_id")
	if sid == "" {
		return mcp.NewToolResultError("session_id is required"), nil
	}
	if !validJobID.MatchString(sid) {
		return mcp.NewToolResultError("session_id must contain only letters, digits, dashes, and underscores"), nil
	}

	apiCtx, cancel := context.WithTimeout(ctx, hyperbrowserSessionTimeout)
	defer cancel()

	// API uses PUT (not DELETE) on /api/session/{id}/stop.
	data, status, err := h.doJSON(apiCtx, http.MethodPut, "/api/session/"+sid+"/stop", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))), nil
	}
	// Empty body is normal for stop — surface a clear confirmation either way.
	body := strings.TrimSpace(string(data))
	if body == "" || body == "{}" {
		body = fmt.Sprintf(`{"sessionId":%q,"stopped":true}`, sid)
	}
	return mcp.NewToolResultText(body), nil
}

func (h *Hyperbrowser) handleSessionList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	apiCtx, cancel := context.WithTimeout(ctx, hyperbrowserSessionTimeout)
	defer cancel()

	data, status, err := h.doJSON(apiCtx, http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// --- Browser-Use Agent handlers ---

func (h *Hyperbrowser) handleAgentStart(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	task, _ := req.RequireString("task")
	if task == "" {
		return mcp.NewToolResultError("task is required"), nil
	}

	payload := map[string]any{"task": task}

	if llm := strings.TrimSpace(req.GetString("llm", "")); llm != "" {
		payload["llm"] = llm
	}
	keepBrowserOpen := false
	if v, ok := req.GetArguments()["keep_browser_open"].(bool); ok {
		payload["keepBrowserOpen"] = v
		keepBrowserOpen = v
	}
	if v, ok := req.GetArguments()["max_steps"].(float64); ok && v > 0 {
		payload["maxSteps"] = int(v)
	}

	so, err := buildSessionOptions(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var explicitSessionID string
	if so != nil {
		// sessionId is top-level on the agent endpoint, not under sessionOptions.
		if sid, ok := so["sessionId"].(string); ok {
			explicitSessionID = sid
			payload["sessionId"] = sid
		}
		// If a session_id is given, the remaining session_options apply to
		// the EXISTING session — which Hyperbrowser ignores. Drop them so
		// we don't send confusing payloads.
		if explicitSessionID != "" {
			so = nil
		} else if len(so) > 0 {
			payload["sessionOptions"] = so
		}
	}

	apiCtx, cancel := context.WithTimeout(ctx, hyperbrowserSessionTimeout)
	defer cancel()

	data, status, err := h.doJSON(apiCtx, http.MethodPost, "/api/task/browser-use", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))), nil
	}

	// Augment the response with a poll hint (analogous to handleCrawl) and,
	// if keep_browser_open is on, a clear reminder that the session will
	// outlive the task and must be stopped explicitly.
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) == nil {
		if jobID, ok := parsed["jobId"].(string); ok && jobID != "" {
			parsed["poll"] = fmt.Sprintf("Use browser_use_agent_status with job_id=%s to poll progress.", jobID)
		}
		if keepBrowserOpen {
			parsed["warning"] = "keep_browser_open=true — the underlying session stays active after the task completes. Call session_stop on the session id (visible via session_list) when done; sessions are billed while active."
		}
		out, _ := json.MarshalIndent(parsed, "", "  ")
		return mcp.NewToolResultText(string(out)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func (h *Hyperbrowser) handleAgentStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	jobID, _ := req.RequireString("job_id")
	if jobID == "" {
		return mcp.NewToolResultError("job_id is required"), nil
	}
	if !validJobID.MatchString(jobID) {
		return mcp.NewToolResultError("job_id must contain only letters, digits, dashes, and underscores"), nil
	}

	apiCtx, cancel := context.WithTimeout(ctx, hyperbrowserSessionTimeout)
	defer cancel()

	data, status, err := h.doJSON(apiCtx, http.MethodGet, "/api/task/browser-use/"+jobID, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status < 200 || status >= 300 {
		return mcp.NewToolResultError(fmt.Sprintf("Hyperbrowser API error (HTTP %d): %s", status, truncate(string(data), 500))), nil
	}

	filtered, fErr := filterAgentStatusResponse(data)
	if fErr != nil {
		// The filter exists to suppress per-step base64 screenshots that would
		// otherwise blow up the agent's token budget. If parsing fails, surface
		// the error explicitly rather than passing the unfiltered (possibly
		// multi-MB) body through — that would defeat the whole purpose.
		return mcp.NewToolResultError(fmt.Sprintf("screenshot filter failed: %v (response was %d bytes, suppressed). Open liveUrl from the start call to inspect the task directly.", fErr, len(data))), nil
	}
	return mcp.NewToolResultText(string(filtered)), nil
}

// filterAgentStatusResponse strips heavy fields (per-step base64 screenshots)
// from the browser-use status payload so the response fits in agent token
// budgets. Keeps: jobId, status, error, liveUrl, metadata (token counts +
// step count), data.finalResult, and per step the model decisions, action,
// and page state (url/title) — everything actionable for an agent reading
// the result.
//
// Returns the original input if it cannot be parsed (caller decides what to
// do with the error).
func filterAgentStatusResponse(raw []byte) ([]byte, error) {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return raw, err
	}

	out := map[string]any{}
	for _, k := range []string{"jobId", "status", "error", "liveUrl"} {
		if v, ok := parsed[k]; ok {
			out[k] = v
		}
	}

	if md, ok := parsed["metadata"].(map[string]any); ok {
		clean := map[string]any{}
		for _, k := range []string{"inputTokens", "outputTokens", "numTaskStepsCompleted"} {
			if v, ok := md[k]; ok {
				clean[k] = v
			}
		}
		if len(clean) > 0 {
			out["metadata"] = clean
		}
	}

	data, ok := parsed["data"].(map[string]any)
	if !ok {
		return json.Marshal(out)
	}

	cleanData := map[string]any{}
	if v, ok := data["finalResult"]; ok {
		cleanData["finalResult"] = v
	}

	if steps, ok := data["steps"].([]any); ok {
		cleanSteps := make([]any, 0, len(steps))
		for _, stepRaw := range steps {
			step, ok := stepRaw.(map[string]any)
			if !ok {
				// Skip non-map step entries entirely — they could carry
				// unknown heavy fields (e.g. raw screenshot blobs) and
				// passing them through would defeat the filter.
				continue
			}
			cleanStep := map[string]any{}

			if mo, ok := step["model_output"].(map[string]any); ok {
				cleanMO := map[string]any{}
				if cs, ok := mo["current_state"].(map[string]any); ok {
					cleanCS := map[string]any{}
					for _, k := range []string{"evaluation_previous_goal", "next_goal"} {
						if v, ok := cs[k]; ok {
							cleanCS[k] = v
						}
					}
					if len(cleanCS) > 0 {
						cleanMO["current_state"] = cleanCS
					}
				}
				if act, ok := mo["action"]; ok {
					cleanMO["action"] = act
				}
				if len(cleanMO) > 0 {
					cleanStep["model_output"] = cleanMO
				}
			}

			if state, ok := step["state"].(map[string]any); ok {
				// NOTE: must NOT include "screenshot" — it's base64 and
				// blows the token budget. The allowlist below is deliberate.
				cleanState := map[string]any{}
				for _, k := range []string{"url", "title"} {
					if v, ok := state[k]; ok {
						cleanState[k] = v
					}
				}
				if len(cleanState) > 0 {
					cleanStep["state"] = cleanState
				}
			}

			cleanSteps = append(cleanSteps, cleanStep)
		}
		cleanData["steps"] = cleanSteps
	}

	out["data"] = cleanData
	return json.Marshal(out)
}
