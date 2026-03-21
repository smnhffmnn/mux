package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

type PostgreSQL struct {
	db *sql.DB
}

func NewPostgreSQL(conn config.Connection, dialer Dialer) (*PostgreSQL, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable&connect_timeout=10",
		url.QueryEscape(conn.User), url.QueryEscape(conn.Password),
		conn.Host, conn.Port, conn.Database)

	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgresql: %w", err)
	}

	if dialer != nil {
		connector.Dialer(&pqDialerAdapter{dialer: dialer})
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &PostgreSQL{db: db}, nil
}

// NewPQDialerAdapter creates a pq.Dialer that routes through the given Dialer.
func NewPQDialerAdapter(d Dialer) *pqDialerAdapter {
	return &pqDialerAdapter{dialer: d}
}

// pqDialerAdapter adapts our Dialer interface to pq.Dialer.
type pqDialerAdapter struct {
	dialer Dialer
}

func (d *pqDialerAdapter) Dial(network, address string) (net.Conn, error) {
	return d.dialer.DialContext(context.Background(), network, address)
}

func (d *pqDialerAdapter) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.dialer.DialContext(ctx, network, address)
}

func (p *PostgreSQL) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("query",
				mcp.WithDescription("Execute a read-only SQL query against PostgreSQL and return results as JSON. Use for SELECT, SHOW, EXPLAIN."),
				mcp.WithString("query", mcp.Required(), mcp.Description("SQL SELECT query to execute")),
			),
			Handler: p.handleQuery,
		},
		{
			Tool: mcp.NewTool("list_tables",
				mcp.WithDescription("List all tables in the configured PostgreSQL database."),
			),
			Handler: p.handleListTables,
		},
		{
			Tool: mcp.NewTool("describe_table",
				mcp.WithDescription("Show the schema of a PostgreSQL table including columns, types, nullability, and defaults."),
				mcp.WithString("table", mcp.Required(), mcp.Description("Table name to describe")),
			),
			Handler: p.handleDescribeTable,
		},
	}
}

func (p *PostgreSQL) handleQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	if !isReadQuery(query) {
		return mcp.NewToolResultError("only SELECT/SHOW/EXPLAIN/WITH queries allowed"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query error: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (p *PostgreSQL) handleListTables(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := p.db.QueryContext(ctx, `
		SELECT table_schema || '.' || table_name AS table_name
		FROM information_schema.tables
		WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY table_schema, table_name`)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error listing tables: %v", err)), nil
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("scan error: %v", err)), nil
		}
		tables = append(tables, name)
	}

	if tables == nil {
		tables = []string{}
	}

	data, _ := json.Marshal(tables)
	return mcp.NewToolResultText(string(data)), nil
}

func (p *PostgreSQL) handleDescribeTable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	table, err := req.RequireString("table")
	if err != nil {
		return mcp.NewToolResultError("table parameter is required"), nil
	}

	if strings.ContainsAny(table, "`;'\"\\") {
		return mcp.NewToolResultError("invalid table name"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	schema := "public"
	tableName := table
	if parts := strings.SplitN(table, ".", 2); len(parts) == 2 {
		schema = parts[0]
		tableName = parts[1]
	}

	rows, err := p.db.QueryContext(ctx, `
		SELECT column_name, data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position`, schema, tableName)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error describing table: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (p *PostgreSQL) Close() error {
	return p.db.Close()
}
