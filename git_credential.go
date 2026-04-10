package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/smnhffmnn/mux/internal/vault"
)

// runGitCredential implements the git credential helper protocol.
// Called as: mux git-credential <operation>
// Only "get" is handled; "store" and "erase" are silently ignored.
//
// Protocol: https://git-scm.com/docs/gitcredentials#_custom_helpers
func runGitCredential(args []string) {
	if len(args) == 0 {
		os.Exit(0)
	}

	op := args[0]
	if op != "get" {
		os.Exit(0)
	}

	// Parse git credential protocol from stdin
	host := ""
	protocol := ""
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break // blank line terminates input
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "host":
			host = value
		case "protocol":
			protocol = value
		}
	}

	// If stdin had an I/O error, host/protocol may be incomplete — bail out.
	if scanner.Err() != nil {
		os.Exit(0)
	}

	// Only handle HTTPS
	if protocol != "https" || host == "" {
		os.Exit(0)
	}

	// Strip port if present (e.g. "gitlab.com:443" → "gitlab.com")
	if idx := strings.Index(host, ":"); idx >= 0 {
		host = host[:idx]
	}

	// Connect to mux daemon via Unix socket with timeout.
	// Defense-in-depth: even if the server misbehaves, git won't hang.
	conn, err := net.DialTimeout("unix", vault.SocketPath(), 5*time.Second)
	if err != nil {
		// Daemon not running or socket not available — silent exit
		os.Exit(0)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send request
	req := struct {
		Host string `json:"host"`
	}{Host: host}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		os.Exit(0)
	}

	// Read response
	var resp struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Error    string `json:"error"`
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		os.Exit(0)
	}

	if resp.Error != "" || resp.Password == "" {
		// Vault sealed, secret not found, or other error — silent exit.
		// Git will report an auth failure, which is the correct behavior.
		os.Exit(0)
	}

	// Output in git credential protocol format (blank line terminates)
	fmt.Printf("username=%s\n", resp.Username)
	fmt.Printf("password=%s\n", resp.Password)
	fmt.Println()
}
