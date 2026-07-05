package tools

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

const graphMaxReportBody = 5 * 1024 * 1024 // 5 MB for report CSVs

// sharePointTools returns read-only SharePoint MCP tools.
func (mg *MicrosoftGraph) sharePointTools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("sp_list_sites",
				mcp.WithDescription("Search SharePoint sites. Returns site ID, name, URL."),
				mcp.WithString("query", mcp.Description("Search query (default: * = all sites)")),
			),
			Handler: mg.handleSPListSites,
		},
		{
			Tool: mcp.NewTool("sp_get_site",
				mcp.WithDescription("Get SharePoint site details."),
				mcp.WithString("site_id", mcp.Required(), mcp.Description("Site ID (e.g. contoso.sharepoint.com,guid,guid)")),
			),
			Handler: mg.handleSPGetSite,
		},
		{
			Tool: mcp.NewTool("sp_list_drives",
				mcp.WithDescription("List document libraries (drives) for a SharePoint site with quota info."),
				mcp.WithString("site_id", mcp.Required(), mcp.Description("Site ID")),
			),
			Handler: mg.handleSPListDrives,
		},
		{
			Tool: mcp.NewTool("sp_list_items",
				mcp.WithDescription("List files and folders in a drive or folder. Returns name, size, type, lastModified."),
				mcp.WithString("drive_id", mcp.Required(), mcp.Description("Drive ID")),
				mcp.WithString("item_id", mcp.Description("Folder item ID (omit for root)")),
				mcp.WithString("order_by", mcp.Description("Sort: name, size, lastModifiedDateTime (default: name). Add ' desc' for descending.")),
				mcp.WithNumber("top", mcp.Description("Max items (default 200, max 200)")),
				mcp.WithString("page_token", mcp.Description("Pagination: full nextLink URL from previous response")),
			),
			Handler: mg.handleSPListItems,
		},
		{
			Tool: mcp.NewTool("sp_get_item_versions",
				mcp.WithDescription("Get version history of a file. Shows size per version to identify version bloat."),
				mcp.WithString("drive_id", mcp.Required(), mcp.Description("Drive ID")),
				mcp.WithString("item_id", mcp.Required(), mcp.Description("Item ID")),
			),
			Handler: mg.handleSPGetItemVersions,
		},
		{
			Tool: mcp.NewTool("sp_storage_report",
				mcp.WithDescription("SharePoint storage usage report (admin). All sites sorted by storage used descending. Requires Reports.Read.All scope + Reports Reader admin role."),
				mcp.WithString("period", mcp.Description("D7, D30, D90, D180 (default: D7)")),
			),
			Handler: mg.handleSPStorageReport,
		},
		{
			Tool: mcp.NewTool("sp_search",
				mcp.WithDescription("Search for files/folders in a drive by name. Returns matches with path, size, and parent location. Searches recursively across the entire drive."),
				mcp.WithString("drive_id", mcp.Required(), mcp.Description("Drive ID")),
				mcp.WithString("query", mcp.Required(), mcp.Description("Search query (e.g. '.zip', 'media package')")),
				mcp.WithNumber("top", mcp.Description("Max results (default 200, max 200)")),
			),
			Handler: mg.handleSPSearch,
		},
	}
}

// --- SharePoint Handlers ---

func (mg *MicrosoftGraph) handleSPListSites(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := "*"
	if v, ok := req.GetArguments()["query"].(string); ok && v != "" {
		query = v
	}

	path := "/sites?search=" + url.QueryEscape(query) + "&$select=id,displayName,webUrl,createdDateTime&$top=100"
	return mg.graphGETValues(ctx, path)
}

func (mg *MicrosoftGraph) handleSPGetSite(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	siteID, _ := req.RequireString("site_id")
	if siteID == "" {
		return mcp.NewToolResultError("site_id is required"), nil
	}
	if !validGraphID(siteID) {
		return mcp.NewToolResultError("invalid site_id"), nil
	}
	return mg.graphGET(ctx, "/sites/"+siteID)
}

func (mg *MicrosoftGraph) handleSPListDrives(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	siteID, _ := req.RequireString("site_id")
	if siteID == "" {
		return mcp.NewToolResultError("site_id is required"), nil
	}
	if !validGraphID(siteID) {
		return mcp.NewToolResultError("invalid site_id"), nil
	}
	return mg.graphGETValues(ctx, "/sites/"+siteID+"/drives?$select=id,name,driveType,quota,webUrl")
}

func (mg *MicrosoftGraph) handleSPListItems(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	driveID, _ := req.RequireString("drive_id")
	if driveID == "" {
		return mcp.NewToolResultError("drive_id is required"), nil
	}
	if !validGraphID(driveID) {
		return mcp.NewToolResultError("invalid drive_id"), nil
	}

	// Pagination: use page_token directly if provided
	if pageToken, ok := req.GetArguments()["page_token"].(string); ok && pageToken != "" {
		after, ok := strings.CutPrefix(pageToken, graphBaseURL)
		if !ok {
			return mcp.NewToolResultError("invalid page_token — must be a full Graph API URL"), nil
		}
		return mg.graphGETValuesPaged(ctx, after)
	}

	itemID := ""
	if v, ok := req.GetArguments()["item_id"].(string); ok && v != "" {
		if !validGraphID(v) {
			return mcp.NewToolResultError("invalid item_id"), nil
		}
		itemID = v
	}

	orderBy := "name"
	if v, ok := req.GetArguments()["order_by"].(string); ok && v != "" {
		orderBy = sanitizeOrderBy(v)
	}

	top := 200
	if v, ok := req.GetArguments()["top"].(float64); ok && v > 0 {
		top = int(v)
		if top > 200 {
			top = 200
		}
	}

	var basePath string
	if itemID != "" {
		basePath = "/drives/" + driveID + "/items/" + itemID + "/children"
	} else {
		basePath = "/drives/" + driveID + "/root/children"
	}

	fields := "id,name,size,file,folder,lastModifiedDateTime,createdDateTime,createdBy,webUrl"
	path := fmt.Sprintf("%s?$select=%s&$orderby=%s&$top=%d", basePath, fields, url.QueryEscape(orderBy), top)
	return mg.graphGETValuesPaged(ctx, path)
}

func (mg *MicrosoftGraph) handleSPGetItemVersions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	driveID, _ := req.RequireString("drive_id")
	itemID, _ := req.RequireString("item_id")
	if driveID == "" || itemID == "" {
		return mcp.NewToolResultError("drive_id and item_id are required"), nil
	}
	if !validGraphID(driveID) || !validGraphID(itemID) {
		return mcp.NewToolResultError("invalid drive_id or item_id"), nil
	}

	path := "/drives/" + driveID + "/items/" + itemID + "/versions?$select=id,size,lastModifiedDateTime,lastModifiedBy"
	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}

	var resp struct {
		Value []any `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse: " + err.Error()), nil
	}

	// Calculate total version size
	var totalSize float64
	for _, v := range resp.Value {
		if m, ok := v.(map[string]any); ok {
			if s, ok := m["size"].(float64); ok {
				totalSize += s
			}
		}
	}

	return jsonResult(map[string]any{
		"versions":       resp.Value,
		"versionCount":   len(resp.Value),
		"totalSizeBytes": int64(totalSize),
	})
}

func (mg *MicrosoftGraph) handleSPStorageReport(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	period := "D7"
	if v, ok := req.GetArguments()["period"].(string); ok && v != "" {
		period = strings.ToUpper(v)
	}
	switch period {
	case "D7", "D30", "D90", "D180":
	default:
		return mcp.NewToolResultError("invalid period — use D7, D30, D90, or D180"), nil
	}

	path := fmt.Sprintf("/reports/getSharePointSiteUsageDetail(period='%s')", period)
	data, status, err := mg.doGraphReport(ctx, path)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}

	// Parse CSV (report endpoints return CSV, possibly with UTF-8 BOM)
	body := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	reader := csv.NewReader(strings.NewReader(body))
	records, err := reader.ReadAll()
	if err != nil {
		return mcp.NewToolResultError("parse CSV: " + err.Error()), nil
	}
	if len(records) < 2 {
		return mcp.NewToolResultError("empty report"), nil
	}

	headers := records[0]
	numericCols := map[string]bool{
		"File Count": true, "Active File Count": true,
		"Page View Count": true, "Visited Page Count": true,
		"Storage Used (Byte)": true, "Storage Allocated (Byte)": true,
		"Report Period": true,
	}

	var results []map[string]any
	for _, row := range records[1:] {
		obj := make(map[string]any)
		for i, val := range row {
			if i >= len(headers) {
				break
			}
			key := headers[i]
			if numericCols[key] {
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					obj[key] = n
					continue
				}
			}
			obj[key] = val
		}
		results = append(results, obj)
	}

	// Sort by storage used descending — biggest consumers first
	sort.Slice(results, func(i, j int) bool {
		si, _ := results[i]["Storage Used (Byte)"].(int64)
		sj, _ := results[j]["Storage Used (Byte)"].(int64)
		return si > sj
	})

	return jsonResult(results)
}

func (mg *MicrosoftGraph) handleSPSearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	driveID, _ := req.RequireString("drive_id")
	query, _ := req.RequireString("query")
	if driveID == "" || query == "" {
		return mcp.NewToolResultError("drive_id and query are required"), nil
	}
	if !validGraphID(driveID) {
		return mcp.NewToolResultError("invalid drive_id"), nil
	}

	top := 200
	if v, ok := req.GetArguments()["top"].(float64); ok && v > 0 {
		top = int(v)
		if top > 200 {
			top = 200
		}
	}

	// Graph search API: /drives/{id}/root/search(q='{query}')
	// The query goes inside single quotes as part of the OData function call path
	sanitized := strings.ReplaceAll(query, "'", "''") // escape single quotes for OData
	path := fmt.Sprintf("/drives/%s/root/search(q='%s')?$select=id,name,size,file,folder,parentReference,lastModifiedDateTime,webUrl&$top=%d",
		driveID, sanitized, top)
	return mg.graphGETValuesPaged(ctx, path)
}

// --- SharePoint Helpers ---

// graphGET performs a GET and returns the full response as pretty JSON.
func (mg *MicrosoftGraph) graphGET(ctx context.Context, path string) (*mcp.CallToolResult, error) {
	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return mcp.NewToolResultError("parse: " + err.Error()), nil
	}
	return jsonResult(parsed)
}

// graphGETValues performs a GET and returns the "value" array from the OData response.
func (mg *MicrosoftGraph) graphGETValues(ctx context.Context, path string) (*mcp.CallToolResult, error) {
	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}
	var resp struct {
		Value []any `json:"value"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse: " + err.Error()), nil
	}
	return jsonResult(resp.Value)
}

// graphGETValuesPaged returns "value" array plus nextLink for pagination.
func (mg *MicrosoftGraph) graphGETValuesPaged(ctx context.Context, path string) (*mcp.CallToolResult, error) {
	data, status, err := mg.doGraph(ctx, http.MethodGet, path, nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if status != http.StatusOK {
		return mcp.NewToolResultError(fmt.Sprintf("Graph API error (HTTP %d): %s", status, string(data))), nil
	}
	var resp struct {
		Value    []any  `json:"value"`
		NextLink string `json:"@odata.nextLink"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return mcp.NewToolResultError("parse: " + err.Error()), nil
	}

	result := map[string]any{
		"items": resp.Value,
		"count": len(resp.Value),
	}
	if resp.NextLink != "" {
		result["nextLink"] = resp.NextLink
	}
	return jsonResult(result)
}

// doGraphReport is like doGraph but with a larger body limit for report CSVs.
func (mg *MicrosoftGraph) doGraphReport(ctx context.Context, path string) ([]byte, int, error) {
	if err := mg.ensureAccessToken(ctx); err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBaseURL+path, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	mg.mu.Lock()
	token := mg.accessToken
	mg.mu.Unlock()

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := mg.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, graphMaxReportBody))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return respData, resp.StatusCode, nil
}

// validGraphID checks that a Graph API resource ID doesn't contain path traversal characters.
// Graph IDs (site, drive, item) use alphanumerics, dots, commas, hyphens, and underscores.
func validGraphID(id string) bool {
	if id == "" || strings.Contains(id, "..") {
		return false
	}
	for _, c := range id {
		if c == '/' || c == '\\' || c == '?' || c == '#' || c == '&' {
			return false
		}
	}
	return true
}

// sanitizeOrderBy validates the orderBy parameter against a whitelist of Graph API fields.
func sanitizeOrderBy(input string) string {
	parts := strings.Fields(strings.ToLower(input))
	if len(parts) == 0 {
		return "name"
	}

	fieldMap := map[string]string{
		"name":                 "name",
		"size":                 "size",
		"lastmodifieddatetime": "lastModifiedDateTime",
		"createddatetime":      "createdDateTime",
	}

	proper, ok := fieldMap[parts[0]]
	if !ok {
		return "name"
	}

	if len(parts) > 1 && parts[1] == "desc" {
		return proper + " desc"
	}
	return proper
}
