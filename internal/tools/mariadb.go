package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// mariadbDialerSeq generates unique network names for mysql.RegisterDialContext.
var mariadbDialerSeq atomic.Int64

type MariaDB struct {
	db       *sql.DB
	readOnly bool
}

func NewMariaDB(conn config.Connection, dialer Dialer) (*MariaDB, error) {
	networkName := "tcp"
	if dialer != nil {
		d := dialer // capture for closure
		networkName = fmt.Sprintf("wg-mux-%d", mariadbDialerSeq.Add(1))
		mysql.RegisterDialContext(networkName, func(ctx context.Context, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		})
	}

	dsn := fmt.Sprintf("%s:%s@%s(%s:%d)/%s?parseTime=true&timeout=10s",
		conn.User, conn.Password, networkName, conn.Host, conn.Port, conn.Database)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mariadb: %w", err)
	}

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &MariaDB{db: db, readOnly: conn.ReadOnly}, nil
}

func (m *MariaDB) Tools() []ToolDef {
	return []ToolDef{
		{
			Tool: mcp.NewTool("query",
				mcp.WithDescription("Execute a read-only SQL query against MariaDB and return results as JSON. Use for SELECT, SHOW, DESCRIBE, EXPLAIN."),
				mcp.WithString("query", mcp.Required(), mcp.Description("SQL SELECT query to execute")),
			),
			Handler: m.handleQuery,
		},
		{
			Tool: mcp.NewTool("execute",
				mcp.WithDescription("Execute a write SQL statement against MariaDB (INSERT, UPDATE, DELETE). Returns affected rows count."),
				mcp.WithString("query", mcp.Required(), mcp.Description("SQL statement to execute")),
			),
			Handler: m.handleExecute,
		},
		{
			Tool: mcp.NewTool("list_tables",
				mcp.WithDescription("List all tables in the configured MariaDB database."),
			),
			Handler: m.handleListTables,
		},
		{
			Tool: mcp.NewTool("describe_table",
				mcp.WithDescription("Show the schema of a MariaDB table including columns, types, nullability, keys, defaults, and extra info."),
				mcp.WithString("table", mcp.Required(), mcp.Description("Table name to describe")),
			),
			Handler: m.handleDescribeTable,
		},
	}
}

func (m *MariaDB) handleQuery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	if !isReadQuery(query) {
		return mcp.NewToolResultError("only SELECT/SHOW/DESCRIBE/EXPLAIN queries allowed; use execute for modifications"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query error: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (m *MariaDB) handleExecute(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	if m.readOnly {
		return mcp.NewToolResultError("MariaDB is configured in read-only mode"), nil
	}

	if isReadQuery(query) {
		return mcp.NewToolResultError("use query for SELECT/SHOW/DESCRIBE/EXPLAIN statements"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result, err := m.db.ExecContext(ctx, query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("execute error: %v", err)), nil
	}

	affected, _ := result.RowsAffected()
	lastID, _ := result.LastInsertId()

	return mcp.NewToolResultText(fmt.Sprintf("Rows affected: %d, Last insert ID: %d", affected, lastID)), nil
}

func (m *MariaDB) handleListTables(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, "SHOW TABLES")
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

func (m *MariaDB) handleDescribeTable(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	table, err := req.RequireString("table")
	if err != nil {
		return mcp.NewToolResultError("table parameter is required"), nil
	}

	if strings.ContainsAny(table, "`;'\"\\") {
		return mcp.NewToolResultError("invalid table name"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, "DESCRIBE `"+table+"`")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("error describing table: %v", err)), nil
	}
	defer rows.Close()

	return rowsToJSON(rows)
}

func (m *MariaDB) Close() error {
	return m.db.Close()
}

func isReadQuery(q string) bool {
	trimmed := strings.TrimSpace(strings.ToUpper(q))
	return strings.HasPrefix(trimmed, "SELECT") ||
		strings.HasPrefix(trimmed, "SHOW") ||
		strings.HasPrefix(trimmed, "DESCRIBE") ||
		strings.HasPrefix(trimmed, "EXPLAIN") ||
		strings.HasPrefix(trimmed, "WITH") // CTEs
}
