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
// explanation is the useful part. truncHint names the way to the full body
// when the tool has one (an output_file argument); empty otherwise.
func rawHTTPResult(resp *http.Response, inlineMax int, truncHint string) (*mcp.CallToolResult, error) {
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
		text += "\n\n[truncated at " + strconv.Itoa(inlineMax) + " bytes"
		if truncHint != "" {
			text += " — " + truncHint
		}
		text += "]"
	}
	if resp.StatusCode >= 400 {
		return mcp.NewToolResultError(text), nil
	}
	return mcp.NewToolResultText(text), nil
}
