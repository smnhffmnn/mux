package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

type ClickHouse struct {
	db              *sql.DB
	defaultDatabase string
}

func NewClickHouse(conn config.Connection, dialer Dialer) (*ClickHouse, error) {
	db := OpenClickHouseDB(conn, dialer)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &ClickHouse{db: db, defaultDatabase: conn.Database}, nil
}

// OpenClickHouseDB creates a *sql.DB using clickhouse.OpenDB with explicit
// protocol selection. Port 8123/8443 → HTTP, otherwise native TCP.
func OpenClickHouseDB(conn config.Connection, dialer Dialer) *sql.DB {
	proto := clickhouse.Native
	if conn.Port == 8123 || conn.Port == 8443 {
		proto = clickhouse.HTTP
	}

	opts := &clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", conn.Host, conn.Port)},
		Auth: clickhouse.Auth{
			Database: conn.Database,
			Username: conn.User,
			Password: conn.Password,
		},
		Protocol:    proto,
		DialTimeout: 10 * time.Second,
		TLS:         nil,
	}

	if dialer != nil {
		opts.DialContext = func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		}
	}

	return clickhouse.OpenDB(opts)
}

func (c *ClickHouse) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("query",
				mcp.WithDescription("Execute a read-only SQL query against ClickHouse and return results as JSON. Use fully qualified names (database.table) to query across databases."),
				mcp.WithString("query", mcp.Required(), mcp.Description("SQL SELECT query to execute")),
			),
			Handler: c.handleQuery,
		},
		{
			Tool: mcp.NewTool("list_databases",
				mcp.WithDescription("List all databases on the ClickHouse server."),
			),
			Handler: c.handleListDatabases,
		},
		{
			Tool: mcp.NewTool("list_tables",
				mcp.WithDescription("List tables in a ClickHouse database. If no database is specified, lists tables across all non-system databases."),
				mcp.WithString("database", mcp.Description("Database name to list tables from. Omit to list tables from all databases.")),
			),
			Handler: c.handleListTables,
		},
		{
			Tool: mcp.NewTool("describe_table",
				mcp.WithDescription("Show the schema of a ClickHouse table including columns, types, default expressions, and comments. Supports 'database.table' notation."),
				mcp.WithString("table", mcp.Required(), mcp.Description("Table name, optionally qualified as 'database.table'")),
			),
			Handler: c.handleDescribeTable,
		},
	}
}

func (c *ClickHouse) handleQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	if !isReadQuery(query) {
		return mcp.NewToolResultError("only SELECT queries are allowed"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query error: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (c *ClickHouse) handleListDatabases(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := c.db.QueryContext(ctx,
		"SELECT name, engine, comment FROM system.databases ORDER BY name")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error listing databases: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (c *ClickHouse) handleListTables(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	database := req.GetString("database", "")

	var query string
	if database != "" {
		if strings.ContainsAny(database, "`;'\"\\") {
			return mcp.NewToolResultError("invalid database name"), nil
		}
		query = fmt.Sprintf(
			"SELECT database, name, engine, total_rows, total_bytes FROM system.tables WHERE database = '%s' ORDER BY name",
			database,
		)
	} else {
		query = "SELECT database, name, engine, total_rows, total_bytes FROM system.tables WHERE database NOT IN ('system', 'INFORMATION_SCHEMA', 'information_schema') ORDER BY database, name"
	}

	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error listing tables: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (c *ClickHouse) handleDescribeTable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	table, err := req.RequireString("table")
	if err != nil {
		return mcp.NewToolResultError("table parameter is required"), nil
	}

	if strings.ContainsAny(table, "`;'\"\\") {
		return mcp.NewToolResultError("invalid table name"), nil
	}

	database := c.defaultDatabase
	tableName := table
	if parts := strings.SplitN(table, ".", 2); len(parts) == 2 {
		database = parts[0]
		tableName = parts[1]
	}

	if database == "" {
		return mcp.NewToolResultError("no database specified — use 'database.table' notation or set a default database in config"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	query := fmt.Sprintf(
		"SELECT name, type, default_kind, default_expression, comment FROM system.columns WHERE database = '%s' AND table = '%s' ORDER BY position",
		database, tableName,
	)

	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error describing table: %v", err)), nil
	}
	defer rows.Close()

	cols, _ := rows.Columns()
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
		return mcp.NewToolResultError(fmt.Sprintf("table '%s' not found", table)), nil
	}

	data, _ := json.MarshalIndent(results, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (c *ClickHouse) Close() error {
	return c.db.Close()
}
