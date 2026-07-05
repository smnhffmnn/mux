package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const firecrawlMaxBody = 512 * 1024 // 512 KB

// Firecrawl wraps the Firecrawl REST API as MCP tools.
type Firecrawl struct {
	client  *http.Client
	baseURL string
	apiKey  string
}

// NewFirecrawl creates a Firecrawl connection from config.
func NewFirecrawl(conn config.Connection, dialer Dialer) (*Firecrawl, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("firecrawl: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://api.firecrawl.dev"
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 60 * time.Second,
		TLSClientConfig:       &tls.Config{},
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	return &Firecrawl{
		client: &http.Client{
			Transport: transport,
			Timeout:   120 * time.Second,
		},
		baseURL: baseURL,
		apiKey:  conn.Token,
	}, nil
}

// Tools returns the MCP tools for the Firecrawl connection.
func (f *Firecrawl) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("scrape",
				mcp.WithDescription("Scrape a single URL and return its content as clean markdown."),
				mcp.WithString("url", mcp.Required(), mcp.Description("The URL to scrape")),
				mcp.WithString("formats", mcp.Description("Comma-separated output formats: markdown, html, rawHtml, links, screenshot. Default: markdown")),
				mcp.WithBoolean("onlyMainContent", mcp.Description("Extract only the main content, excluding headers, navs, footers. Default: true")),
			),
			Handler: f.handleScrape,
		},
		{
			Tool: mcp.NewTool("search",
				mcp.WithDescription("Search the web using Firecrawl and return results with full page content."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
				mcp.WithNumber("limit", mcp.Description("Maximum number of results (default: 5)")),
			),
			Handler: f.handleSearch,
		},
		{
			Tool: mcp.NewTool("crawl",
				mcp.WithDescription("Start crawling a website from a URL. Returns a crawl job ID for status polling."),
				mcp.WithString("url", mcp.Required(), mcp.Description("Starting URL to crawl")),
				mcp.WithNumber("limit", mcp.Description("Maximum number of pages to crawl (default: 10)")),
				mcp.WithNumber("maxDepth", mcp.Description("Maximum link depth to follow (default: 2)")),
			),
			Handler: f.handleCrawl,
		},
		{
			Tool: mcp.NewTool("crawl_status",
				mcp.WithDescription("Check the status of a crawl job and retrieve results."),
				mcp.WithString("id", mcp.Required(), mcp.Description("Crawl job ID returned by the crawl tool")),
			),
			Handler: f.handleCrawlStatus,
		},
		{
			Tool: mcp.NewTool("map",
				mcp.WithDescription("Discover URLs on a website. Returns a list of URLs found starting from the given URL."),
				mcp.WithString("url", mcp.Required(), mcp.Description("Starting URL to map")),
			),
			Handler: f.handleMap,
		},
		{
			Tool: mcp.NewTool("usage",
				mcp.WithDescription("Show Firecrawl credit usage for the current billing period."),
			),
			Handler: f.handleUsage,
		},
	}
}

func (f *Firecrawl) doJSON(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.apiKey)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, firecrawlMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

func (f *Firecrawl) handleScrape(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	url, _ := req.RequireString("url")
	if url == "" {
		return mcp.NewToolResultError("url is required"), nil
	}

	payload := map[string]any{"url": url}

	if formats, ok := req.GetArguments()["formats"].(string); ok && formats != "" {
		parts := strings.Split(formats, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		payload["formats"] = parts
	} else {
		payload["formats"] = []string{"markdown"}
	}

	if onlyMain, ok := req.GetArguments()["onlyMainContent"].(bool); ok {
		payload["onlyMainContent"] = onlyMain
	}

	data, status, err := f.doJSON(ctx, http.MethodPost, "/v1/scrape", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Firecrawl API error (HTTP %d): %s", status, string(data))), nil
	}

	// Extract markdown from response
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Markdown string `json:"markdown"`
			HTML     string `json:"html"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) == nil && result.Data.Markdown != "" {
		return mcp.NewToolResultText(result.Data.Markdown), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *Firecrawl) handleSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	payload := map[string]any{
		"query":         query,
		"scrapeOptions": map[string]any{"formats": []string{"markdown"}},
	}

	if limit, ok := req.GetArguments()["limit"].(float64); ok && limit > 0 {
		payload["limit"] = int(limit)
	}

	data, status, err := f.doJSON(ctx, http.MethodPost, "/v1/search", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Firecrawl API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *Firecrawl) handleCrawl(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	url, _ := req.RequireString("url")
	if url == "" {
		return mcp.NewToolResultError("url is required"), nil
	}

	payload := map[string]any{"url": url}

	if limit, ok := req.GetArguments()["limit"].(float64); ok && limit > 0 {
		payload["limit"] = int(limit)
	}
	if maxDepth, ok := req.GetArguments()["maxDepth"].(float64); ok && maxDepth > 0 {
		payload["maxDepth"] = int(maxDepth)
	}

	data, status, err := f.doJSON(ctx, http.MethodPost, "/v1/crawl", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Firecrawl API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *Firecrawl) handleCrawlStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, _ := req.RequireString("id")
	if id == "" {
		return mcp.NewToolResultError("id is required"), nil
	}

	data, status, err := f.doJSON(ctx, http.MethodGet, "/v1/crawl/"+id, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Firecrawl API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *Firecrawl) handleMap(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	url, _ := req.RequireString("url")
	if url == "" {
		return mcp.NewToolResultError("url is required"), nil
	}

	payload := map[string]any{"url": url}

	data, status, err := f.doJSON(ctx, http.MethodPost, "/v1/map", payload)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Firecrawl API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (f *Firecrawl) handleUsage(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, status, err := f.doJSON(ctx, http.MethodGet, "/v2/team/credit-usage", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Firecrawl API error (HTTP %d): %s", status, string(data))), nil
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			RemainingCredits   int    `json:"remainingCredits"`
			PlanCredits        int    `json:"planCredits"`
			BillingPeriodStart string `json:"billingPeriodStart"`
			BillingPeriodEnd   string `json:"billingPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("parse response: %v", err)), nil
	}

	used := resp.Data.PlanCredits - resp.Data.RemainingCredits
	var pctUsed float64
	if resp.Data.PlanCredits > 0 {
		pctUsed = float64(used) / float64(resp.Data.PlanCredits) * 100
	}

	result := map[string]any{
		"remainingCredits":   resp.Data.RemainingCredits,
		"planCredits":        resp.Data.PlanCredits,
		"usedCredits":        used,
		"percentUsed":        fmt.Sprintf("%.1f%%", pctUsed),
		"billingPeriodStart": resp.Data.BillingPeriodStart,
		"billingPeriodEnd":   resp.Data.BillingPeriodEnd,
	}

	pctRemaining := 100 - pctUsed
	if pctRemaining < 10 {
		result["warning"] = fmt.Sprintf("Low credits: only %.1f%% remaining (%d credits)", pctRemaining, resp.Data.RemainingCredits)
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}
