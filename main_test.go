package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/smnhffmnn/mux/internal/config"
)

// freePort asks the kernel for an unused loopback port. startHTTPServer binds
// by number, so another process could grab the port in between; on a
// developer machine or a CI runner that is not a realistic concern.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitListening blocks until addr accepts TCP connections. startHTTPServer
// starts its listener in a goroutine, so the test has to wait for it.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server on %s did not start listening", addr)
}

// TestShutdownEndsOpenNotificationStream reproduces how an MCP client holds a
// session open: a GET /mcp whose handler blocks until the request context is
// done. http.Server.Shutdown alone never cancels that context, so the
// connection never went idle and Shutdown used to wait out its whole deadline
// on every Ctrl-C ("HTTP shutdown error: context deadline exceeded"). With the
// base context cancelled on Shutdown, the stream ends at once and Shutdown
// returns in milliseconds, without an error.
func TestShutdownEndsOpenNotificationStream(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{}
	cfg.Server.Port = port

	servers := startHTTPServer(server.NewMCPServer("mux-test", "0.0.0"), cfg, nil, nil, nil)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitListening(t, addr)

	// Open the notification stream the way an MCP client does. Without a
	// session header mcp-go creates a session of its own, which is all the
	// handler needs to park on r.Context().Done().
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/mcp", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /mcp: status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("GET /mcp: Content-Type %q, want text/event-stream", ct)
	}

	// The body only ends when the handler returns; reading it to the end is
	// how the test sees that happen.
	streamDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		streamDone <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err = servers.Shutdown(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Shutdown returned %v after %v", err, elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("Shutdown took %v, want well under 1s", elapsed)
	}

	// EOF or a reset both mean the handler returned and the connection is
	// gone; what must not happen is the stream staying open.
	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("notification stream still open after Shutdown")
	}
}

// TestShutdownWithoutConnections is the baseline: nothing in flight, Shutdown
// returns immediately and without an error.
func TestShutdownWithoutConnections(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{}
	cfg.Server.Port = port

	servers := startHTTPServer(server.NewMCPServer("mux-test", "0.0.0"), cfg, nil, nil, nil)
	waitListening(t, fmt.Sprintf("127.0.0.1:%d", port))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := servers.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned %v", err)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Shutdown took %v, want well under 1s", elapsed)
	}
}
