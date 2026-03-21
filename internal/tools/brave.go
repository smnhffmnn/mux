package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

const braveMaxBody = 256 * 1024 // 256 KB

// rateLimitInfo holds parsed rate-limit headers from Brave API responses.
type rateLimitInfo struct {
	Limit     int
	Remaining int
	Reset     int64 // unix timestamp
}

// Brave wraps the Brave Search REST API as MCP tools.
type Brave struct {
	client       *http.Client
	baseURL      string
	apiKey       string
	monthlyLimit int

	requestCount atomic.Int64
	monthStart   time.Time // 1st of the current tracking month

	rateMu    sync.Mutex
	rateLimit rateLimitInfo
}

// NewBrave creates a Brave Search connection from config.
func NewBrave(conn config.Connection, dialer Dialer) (*Brave, error) {
	if conn.Token == "" {
		return nil, fmt.Errorf("brave: API key is required")
	}

	baseURL := strings.TrimRight(conn.URL, "/")
	if baseURL == "" {
		baseURL = "https://api.search.brave.com"
	}

	transport := &http.Transport{
		ResponseHeaderTimeout: 30 * time.Second,
		TLSClientConfig:       &tls.Config{},
	}
	if dialer != nil {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		}
	}

	now := time.Now()
	return &Brave{
		client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
		baseURL:      baseURL,
		apiKey:       conn.Token,
		monthlyLimit: conn.MonthlyLimit,
		monthStart:   time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()),
	}, nil
}

// Tools returns the MCP tools for the Brave Search connection.
func (b *Brave) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("web_search",
				mcp.WithDescription("Search the web using Brave Search. Returns web results with titles, URLs, and descriptions."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Search query")),
				mcp.WithNumber("count", mcp.Description("Number of results to return (default: 10, max: 20)")),
				mcp.WithString("country", mcp.Description("Country code for search results (e.g. DE, US, NL)")),
				mcp.WithString("search_lang", mcp.Description("Language code for search (e.g. de, en, nl)")),
				mcp.WithBoolean("freshness", mcp.Description("Prefer recent results")),
			),
			Handler: b.handleWebSearch,
		},
		{
			Tool: mcp.NewTool("local_search",
				mcp.WithDescription("Search for local businesses and places using Brave Search."),
				mcp.WithString("query", mcp.Required(), mcp.Description("Search query for local businesses/places")),
				mcp.WithNumber("count", mcp.Description("Number of results (default: 5)")),
			),
			Handler: b.handleLocalSearch,
		},
		{
			Tool: mcp.NewTool("usage",
				mcp.WithDescription("Show Brave Search usage stats for the current month (client-side counting, resets on mux restart)."),
			),
			Handler: b.handleUsage,
		},
	}
}

func (b *Brave) trackRequest(resp *http.Response) {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// Parse rate-limit headers before acquiring lock
	var rl rateLimitInfo
	hasHeaders := false
	if v := resp.Header.Get("X-RateLimit-Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Limit = n
			hasHeaders = true
		}
	}
	if v := resp.Header.Get("X-RateLimit-Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.Remaining = n
			hasHeaders = true
		}
	}
	if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.Reset = n
			hasHeaders = true
		}
	}

	// Single lock for month rollover and rate-limit update
	b.rateMu.Lock()
	if monthStart.After(b.monthStart) {
		b.requestCount.Store(0)
		b.monthStart = monthStart
	}
	if hasHeaders {
		b.rateLimit = rl
	}
	b.rateMu.Unlock()

	b.requestCount.Add(1)

	if resp.StatusCode == http.StatusTooManyRequests {
		log.Printf("[brave] Rate limited (HTTP 429)")
	}
}

func (b *Brave) doGet(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	b.trackRequest(resp)

	data, err := io.ReadAll(io.LimitReader(resp.Body, braveMaxBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

func (b *Brave) handleWebSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	params := url.Values{}
	params.Set("q", query)

	if count, ok := req.GetArguments()["count"].(float64); ok && count > 0 {
		params.Set("count", fmt.Sprintf("%d", int(count)))
	}
	if country, ok := req.GetArguments()["country"].(string); ok && country != "" {
		params.Set("country", country)
	}
	if lang, ok := req.GetArguments()["search_lang"].(string); ok && lang != "" {
		params.Set("search_lang", lang)
	}
	if freshness, ok := req.GetArguments()["freshness"].(bool); ok && freshness {
		params.Set("freshness", "pd") // past day
	}

	data, status, err := b.doGet(ctx, "/res/v1/web/search?"+params.Encode())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Brave Search API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (b *Brave) handleLocalSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, _ := req.RequireString("query")
	if query == "" {
		return mcp.NewToolResultError("query is required"), nil
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("result_filter", "locations")

	if count, ok := req.GetArguments()["count"].(float64); ok && count > 0 {
		params.Set("count", fmt.Sprintf("%d", int(count)))
	}

	data, status, err := b.doGet(ctx, "/res/v1/web/search?"+params.Encode())
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Brave Search API error (HTTP %d): %s", status, string(data))), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

func (b *Brave) handleUsage(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	count := b.requestCount.Load()

	// Read monthStart and rateLimit under lock (written concurrently by trackRequest)
	b.rateMu.Lock()
	ms := b.monthStart
	rl := b.rateLimit
	b.rateMu.Unlock()

	result := map[string]any{
		"requestsThisMonth": count,
		"monthStart":        ms.Format("2006-01-02"),
		"note":              "Client-side counter, resets on mux restart",
	}

	if b.monthlyLimit > 0 {
		pctUsed := float64(count) / float64(b.monthlyLimit) * 100
		result["monthlyLimit"] = b.monthlyLimit
		result["percentUsed"] = fmt.Sprintf("%.1f%%", pctUsed)

		pctRemaining := 100 - pctUsed
		if pctRemaining < 10 {
			result["warning"] = fmt.Sprintf("High usage: %.1f%% of monthly limit consumed (%d/%d)", pctUsed, count, b.monthlyLimit)
		}
	}

	if rl.Limit > 0 {
		result["rateLimit"] = map[string]any{
			"limit":     rl.Limit,
			"remaining": rl.Remaining,
			"reset":     rl.Reset,
		}
	}

	out, _ := json.MarshalIndent(result, "", "  ")
	return mcp.NewToolResultText(string(out)), nil
}
