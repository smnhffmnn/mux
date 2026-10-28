package main

import (
	"io"
	"log"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// toolBook records which MCP tools and closers (sql.DB pools, sessions)
// belong to which connection, so a reload or a delete removes exactly them.
// One book is shared by startup registration, the vault-unlock retry and the
// reloader of the running mode (App or simpleReloader): a connection
// registered before any reloader existed is still removed when it is reloaded
// or deleted, instead of keeping its tools — and the credentials held by them
// — until the process ends.
type toolBook struct {
	// mu protects tools and closers.
	mu      sync.Mutex
	tools   map[string][]string
	closers map[string][]io.Closer
}

func newToolBook() *toolBook {
	return &toolBook{tools: make(map[string][]string), closers: make(map[string][]io.Closer)}
}

// record sets the tool names of a connection and keeps its closer, if any.
func (b *toolBook) record(conn string, names []string, closer io.Closer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(names) > 0 {
		b.tools[conn] = names
	}
	if closer != nil {
		b.closers[conn] = append(b.closers[conn], closer)
	}
}

// remove deletes the recorded tools of a connection from s and closes its
// closers.
func (b *toolBook) remove(s *server.MCPServer, conn string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if names := b.tools[conn]; len(names) > 0 && s != nil {
		s.DeleteTools(names...)
		log.Printf("[mux] Unregistered %d tools for %s", len(names), conn)
	}
	delete(b.tools, conn)
	for _, c := range b.closers[conn] {
		c.Close()
	}
	delete(b.closers, conn)
}
