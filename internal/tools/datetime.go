package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

var DateTimeTool = ToolDef{
	Tool: mcp.NewTool("get_datetime",
		mcp.WithDescription("Returns the current date and time in ISO 8601 format."),
		mcp.WithNumber("timezone_offset",
			mcp.Description("Optional UTC offset in hours (e.g. 1 for CET, 2 for CEST). Defaults to UTC if not provided."),
		),
	),
	Handler: handleDateTime,
}

func handleDateTime(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	loc := time.UTC

	offset := req.GetFloat("timezone_offset", 0)
	if _, ok := req.GetArguments()["timezone_offset"]; ok {
		hours := int(offset)
		loc = time.FixedZone(fmt.Sprintf("UTC%+d", hours), hours*3600)
	}

	now := time.Now().In(loc)
	return mcp.NewToolResultText(now.Format(time.RFC3339)), nil
}
