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
	case config.TypeMariaDB:
		var mdb *MariaDB
		mdb, err = NewMariaDB(conn, dialer)
		if err == nil {
			toolDefs = mdb.Tools()
			closer = mdb
		}
	case config.TypeClickHouse:
		var ch *ClickHouse
		ch, err = NewClickHouse(conn, dialer)
		if err == nil {
			toolDefs = ch.Tools()
			closer = ch
		}
	case config.TypePostgreSQL:
		var pg *PostgreSQL
		pg, err = NewPostgreSQL(conn, dialer)
		if err == nil {
			toolDefs = pg.Tools()
			closer = pg
		}
	case config.TypeHTTP:
		var h *HTTP
		h, err = NewHTTP(conn, dialer)
		if err == nil {
			toolDefs = h.Tools()
		}
	case config.TypeFirecrawl:
		var fc *Firecrawl
		fc, err = NewFirecrawl(conn, dialer)
		if err == nil {
			toolDefs = fc.Tools()
		}
	case config.TypeBrave:
		var br *Brave
		br, err = NewBrave(conn, dialer)
		if err == nil {
			toolDefs = br.Tools()
		}
	case config.TypeOpenAI:
		var oa *OpenAI
		oa, err = NewOpenAI(conn, dialer)
		if err == nil {
			toolDefs = oa.Tools()
		}
	case config.TypeElevenLabs:
		var el *ElevenLabs
		el, err = NewElevenLabs(conn, dialer)
		if err == nil {
			toolDefs = el.Tools()
		}
	case config.TypeRecraft:
		var rc *Recraft
		rc, err = NewRecraft(conn, dialer)
		if err == nil {
			toolDefs = rc.Tools()
		}
	case config.TypeIdeogram:
		var ig *Ideogram
		ig, err = NewIdeogram(conn, dialer)
		if err == nil {
			toolDefs = ig.Tools()
		}
	case config.TypeGemini:
		var gm *Gemini
		gm, err = NewGemini(conn, dialer)
		if err == nil {
			toolDefs = gm.Tools()
		}
	case config.TypeFalAI:
		var fa *FalAI
		fa, err = NewFalAI(conn, dialer)
		if err == nil {
			toolDefs = fa.Tools()
		}
	case config.TypeMicrosoftGraph:
		var mg *MicrosoftGraph
		mg, err = NewMicrosoftGraph(conn, dialer)
		if err == nil {
			toolDefs = mg.Tools()
		}
	case config.TypeGoogleTagManager:
		var gtm *GoogleTagManager
		gtm, err = NewGoogleTagManager(conn, dialer)
		if err == nil {
			toolDefs = gtm.Tools()
		}
	case config.TypeAsana:
		var a *Asana
		a, err = NewAsana(conn, dialer)
		if err == nil {
			toolDefs = a.Tools()
		}
	case config.TypeIMAP:
		var im *IMAP
		im, err = NewIMAP(conn, dialer)
		if err == nil {
			toolDefs = im.Tools()
		}
	case config.TypeMeilisearch:
		var ms *Meilisearch
		ms, err = NewMeilisearch(conn, dialer)
		if err == nil {
			toolDefs = ms.Tools()
		}
	case config.TypeYouTrackAgile:
		var ya *YouTrackAgile
		ya, err = NewYouTrackAgile(conn, dialer)
		if err == nil {
			toolDefs = ya.Tools()
		}
	case config.TypeGit:
		// Passive connection type — no MCP tools.
		// Used by the git credential helper (mux git-credential) to look up
		// PATs from the vault. Visible in connection_list for discoverability.
		log.Printf("[mux] Git credential: %s (%s)", conn.Name, conn.Host)
		return nil, nil, nil
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
		s.AddTool(t.Tool, withCallLogging(prefixedName, t.Handler))
		log.Printf("[mux] Registered: %s", prefixedName)
		names = append(names, prefixedName)
	}
	return names, closer, nil
}

// withCallLogging wraps a tool handler to log each invocation.
func withCallLogging(name string, handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		log.Printf("[tool] Call: %s", name)
		result, err := handler(ctx, req)
		if err != nil {
			log.Printf("[tool] Error: %s — %v", name, err)
		}
		return result, err
	}
}

// DefaultInstructions returns built-in instructions for connection types that have them.
// Returns empty string for types without default instructions.
func DefaultInstructions(connType string) string {
	switch connType {
	case config.TypeOpenAI:
		return DefaultOpenAIInstructions
	case config.TypeElevenLabs:
		return DefaultElevenLabsInstructions
	case config.TypeRecraft:
		return DefaultRecraftInstructions
	case config.TypeIdeogram:
		return DefaultIdeogramInstructions
	case config.TypeAsana:
		return DefaultAsanaInstructions
	case config.TypeAsanaMCP:
		return DefaultAsanaMCPInstructions
	case config.TypeGemini:
		return DefaultGeminiInstructions
	case config.TypeIMAP:
		return DefaultIMAPInstructions
	case config.TypeGoogleWorkspace:
		return DefaultGoogleWorkspaceInstructions
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
