package tools

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
)

// rawHTTPResult renders a pass-through response as the agent should see it:
// the status line, then the body as is, inlined up to inlineMax bytes. A 4xx
// or 5xx is an error result that still carries the body — the upstream's
// explanation is the useful part.
func rawHTTPResult(resp *http.Response, inlineMax int) (*mcp.CallToolResult, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(inlineMax)+1))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read response: %v", err)), nil
	}
	truncated := false
	if len(data) > inlineMax {
		data = data[:inlineMax]
		truncated = true
	}
	text := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, http.StatusText(resp.StatusCode), string(data))
	if truncated {
		text += "\n\n[truncated at " + strconv.Itoa(inlineMax) + " bytes — pass output_file to save the full response]"
	}
	if resp.StatusCode >= 400 {
		return mcp.NewToolResultError(text), nil
	}
	return mcp.NewToolResultText(text), nil
}
