package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
)

// Dialer is the interface for custom network dialers (e.g. WireGuard tunnels).
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// ToolDef pairs a tool definition with its handler.
type ToolDef struct {
	Tool    mcp.Tool
	Handler server.ToolHandlerFunc
}

// RegisterConnection creates and registers tools for a connection on the MCP server.
// Returns the tool names registered, an optional io.Closer for resource cleanup
// (non-nil for database types with connection pools), or an error.
// The dialer parameter is optional (nil = direct TCP connection).
//
// Known connection types are defined in config.AllTypes (internal/config/types.go),
// which is the single source of truth. This switch handles the implementation-specific
// creation of tool handlers for each type.
func RegisterConnection(s *server.MCPServer, conn config.Connection, dialer Dialer) ([]string, io.Closer, error) {
	if !conn.Enabled() {
		return nil, nil, fmt.Errorf("connection %q not configured", conn.Name)
	}

	// Validate against the canonical type registry before attempting handler creation.
	if !config.ValidType(conn.Type) {
		return nil, nil, fmt.Errorf("unknown connection type %q", conn.Type)
	}

	var toolDefs []ToolDef
	var closer io.Closer
	var err error

	switch conn.Type {
	case "mariadb":
		var mdb *MariaDB
		mdb, err = NewMariaDB(conn, dialer)
		if err == nil {
			toolDefs = mdb.Tools()
			closer = mdb
		}
	case "clickhouse":
		var ch *ClickHouse
		ch, err = NewClickHouse(conn, dialer)
		if err == nil {
			toolDefs = ch.Tools()
			closer = ch
		}
	case "postgresql":
		var pg *PostgreSQL
		pg, err = NewPostgreSQL(conn, dialer)
		if err == nil {
			toolDefs = pg.Tools()
			closer = pg
		}
	case "http":
		var h *HTTP
		h, err = NewHTTP(conn, dialer)
		if err == nil {
			toolDefs = h.Tools()
		}
	case "firecrawl":
		var fc *Firecrawl
		fc, err = NewFirecrawl(conn, dialer)
		if err == nil {
			toolDefs = fc.Tools()
		}
	case "brave":
		var br *Brave
		br, err = NewBrave(conn, dialer)
		if err == nil {
			toolDefs = br.Tools()
		}
	case "openai":
		var oa *OpenAI
		oa, err = NewOpenAI(conn, dialer)
		if err == nil {
			toolDefs = oa.Tools()
		}
	case "elevenlabs":
		var el *ElevenLabs
		el, err = NewElevenLabs(conn, dialer)
		if err == nil {
			toolDefs = el.Tools()
		}
	case "recraft":
		var rc *Recraft
		rc, err = NewRecraft(conn, dialer)
		if err == nil {
			toolDefs = rc.Tools()
		}
	case "ideogram":
		var ig *Ideogram
		ig, err = NewIdeogram(conn, dialer)
		if err == nil {
			toolDefs = ig.Tools()
		}
	case "microsoft-graph":
		var mg *MicrosoftGraph
		mg, err = NewMicrosoftGraph(conn, dialer)
		if err == nil {
			toolDefs = mg.Tools()
		}
	case "google-tagmanager":
		var gtm *GoogleTagManager
		gtm, err = NewGoogleTagManager(conn, dialer)
		if err == nil {
			toolDefs = gtm.Tools()
		}
	case "asana":
		var a *Asana
		a, err = NewAsana(conn, dialer)
		if err == nil {
			toolDefs = a.Tools()
		}
	default:
		if config.IsProxyType(conn.Type) {
			return nil, nil, fmt.Errorf("proxy connections must be registered via the proxy package")
		}
		return nil, nil, fmt.Errorf("unknown connection type %q", conn.Type)
	}

	if err != nil {
		return nil, nil, err
	}

	var names []string
	for _, t := range toolDefs {
		// Prefix tool name with connection name
		prefixedName := conn.Name + "_" + t.Tool.Name
		t.Tool.Name = prefixedName
		s.AddTool(t.Tool, t.Handler)
		log.Printf("[mux] Registered: %s", prefixedName)
		names = append(names, prefixedName)
	}
	return names, closer, nil
}

// DefaultInstructions returns built-in instructions for connection types that have them.
// Returns empty string for types without default instructions.
func DefaultInstructions(connType string) string {
	switch connType {
	case "openai":
		return DefaultOpenAIInstructions
	case "elevenlabs":
		return DefaultElevenLabsInstructions
	case "recraft":
		return DefaultRecraftInstructions
	case "ideogram":
		return DefaultIdeogramInstructions
	case "asana":
		return DefaultAsanaInstructions
	case "asana-mcp":
		return DefaultAsanaMCPInstructions
	default:
		return ""
	}
}

// rowsToJSON converts sql.Rows into a JSON array of objects.
func rowsToJSON(rows *sql.Rows) (*mcp.CallToolResult, error) {
	cols, err := rows.Columns()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("columns error: %v", err)), nil
	}

	var results []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("scan error: %v", err)), nil
		}

		row := make(map[string]any)
		for i, col := range cols {
			v := values[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[col] = v
		}
		results = append(results, row)
	}

	if results == nil {
		results = []map[string]any{}
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}
