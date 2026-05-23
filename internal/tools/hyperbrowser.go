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
	hyperbrowserPollInterval       = 2 * time.Second
)

// validJobID restricts API job IDs to alphanumerics, dashes, and underscores
// so user-supplied job_id values cannot inject path segments or query strings.
var validJobID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// DefaultHyperbrowserInstructions are used when no custom instructions are set.
const DefaultHyperbrowserInstructions = `Hyperbrowser — stealth headless Chrome with residential proxy rotation and CAPTCHA solving.

Use this connection when Firecrawl fails on Anti-Bot-protected sites (Imperva,
Cloudflare, DataDome) or JS-heavy SPAs that need a real browser. For plain
HTTP scrapes prefer Firecrawl — it is cheaper and faster.

Tools:
- scrape         — Single URL → markdown / html / links / screenshot. Blocks until done (max 90s).
- extract        — Structured data extraction with JSON schema and prompt. Blocks (max 180s).
- crawl          — Multi-page crawl starting from a URL. Async — returns jobId.
- crawl_status   — Poll a crawl job by jobId; returns results once status is completed.

Session options (pass on scrape/extract/crawl):
- use_stealth    — fingerprint masking (default: false; recommended for Anti-Bot sites)
- use_proxy      — residential proxy rotation (PAID plan; default: false)
- solve_captchas — auto-solve CAPTCHAs (PAID plan; default: false)
- accept_cookies — auto-dismiss cookie banners (default: false)
- proxy_country  — 2-letter country code for the proxy (e.g. "DE", "US")

CAPTCHA solving and proxy usage slow requests down — enable only when needed.`

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

	return []ToolDef{
		{
			Tool: mcp.NewTool("scrape",
				mcp.WithDescription("Scrape a single URL with a stealth headless browser. Blocks until the job is complete (max ~90s); returns markdown by default. Use for Anti-Bot-protected sites or JS-heavy SPAs that fail with Firecrawl."),
				mcp.WithString("url", mcp.Required(), mcp.Description("The URL to scrape")),
				mcp.WithString("formats", mcp.Description("Comma-separated output formats: markdown, html, links, screenshot. Default: markdown")),
				mcp.WithBoolean("only_main_content", mcp.Description("Extract only the main content (default: true)")),
				mcp.WithString("session_options", mcp.Description(sessionOptsDesc)),
				mcp.WithString("proxy_country", mcp.Description("Two-letter country code for the proxy (only when use_proxy is set). Example: DE, US, GB.")),
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
// sessionOptions object. Returns nil if no options were provided.
func buildSessionOptions(req mcp.CallToolRequest) map[string]any {
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

	if len(opts) == 0 {
		return nil
	}
	return opts
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
	if so := buildSessionOptions(req); so != nil {
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
	if so := buildSessionOptions(req); so != nil {
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
	if so := buildSessionOptions(req); so != nil {
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
