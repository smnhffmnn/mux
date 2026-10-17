package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/sshclient"
	"github.com/smnhffmnn/mux/internal/sshclient/sshtest"
)

func TestShellQuote(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"systemctl", "status", "nginx"}, "systemctl status nginx"},
		{[]string{"echo", "hello world"}, "echo 'hello world'"},
		{[]string{"echo", "it's"}, `echo 'it'\''s'`},
		{[]string{"echo", ""}, "echo ''"},
		{[]string{"cat", "/etc/hosts"}, "cat /etc/hosts"},
		{[]string{"grep", "-r", "x; rm -rf /", "."}, "grep -r 'x; rm -rf /' ."},
		{[]string{"echo", "$HOME"}, "echo '$HOME'"},
		{[]string{"echo", "*"}, "echo '*'"},
		{[]string{"ls", "a b|c"}, "ls 'a b|c'"},
		{[]string{"X=1", "reboot"}, "'X=1' reboot"}, // a bare NAME=value would be a shell assignment
		{[]string{"grep", "--color=never", "x"}, "grep '--color=never' x"},
	}
	for _, c := range cases {
		if got := shellQuote(c.argv); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestCommandPolicy(t *testing.T) {
	// Not read-only, no lists: anything goes.
	p, err := newCommandPolicy(config.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	if v := p.decide([]string{"rm", "-rf", "/"}); v != "" {
		t.Errorf("open policy refused: %s", v)
	}

	// Read-only without lists: built-in list, built-in denies.
	p, _ = newCommandPolicy(config.Connection{ReadOnly: true})
	if !p.usingDefaults {
		t.Error("read-only without allow_commands should use the defaults")
	}
	for _, ok := range [][]string{{"cat", "/etc/hosts"}, {"ls", "-la"}, {"systemctl", "status", "nginx"}, {"journalctl", "-u", "nginx", "-n", "50"}, {"docker", "logs", "web"}, {"date"}, {"date", "-u"}} {
		if v := p.decide(ok); v != "" {
			t.Errorf("defaults refused %v: %s", ok, v)
		}
	}
	for _, bad := range [][]string{
		{"rm", "x"}, {"systemctl", "restart", "nginx"}, {"journalctl", "--vacuum-size=1G"}, {"journalctl", "--rotate"},
		{"docker", "rm", "web"}, {"sudo", "cat", "x"},
		{"env", "sh", "-c", "rm -rf /"}, // env would run any program
		{"hostname", "evil"},            // sets the name as root
		{"date", "-s", "2020-01-01"}, {"date", "--set=2020-01-01"},
		{"/bin/cat", "x"}, {"/tmp/evil/cat"}, // the program is matched as written
		{"X=1", "cat", "x"},
	} {
		if v := p.decide(bad); v == "" {
			t.Errorf("defaults allowed %v", bad)
		}
	}
	if v := p.shellAllowed(); !strings.Contains(v, "read-only") {
		t.Errorf("shell on read-only: %s", v)
	}
	if v := p.decide([]string{"rm", "x"}); !strings.Contains(v, "built-in read-only list") {
		t.Errorf("refusal should name the built-in list: %s", v)
	}

	// Own allow list replaces the defaults; own deny wins over allow.
	p, _ = newCommandPolicy(config.Connection{ReadOnly: true, AllowCommands: []string{"uptime", "systemctl status"}, DenyCommands: []string{"systemctl status ssh*"}})
	if p.usingDefaults {
		t.Error("explicit allow_commands should not use the defaults")
	}
	if v := p.decide([]string{"cat", "x"}); v == "" {
		t.Error("cat is not in the explicit list and must be refused")
	}
	if v := p.decide([]string{"systemctl", "status", "nginx"}); v != "" {
		t.Errorf("explicit allow refused: %s", v)
	}
	if v := p.decide([]string{"systemctl", "status", "sshd"}); !strings.Contains(v, "deny_commands") {
		t.Errorf("deny should win: %s", v)
	}

	// Deny without read-only still applies, and rules shell=true out.
	p, _ = newCommandPolicy(config.Connection{DenyCommands: []string{"reboot", "shutdown"}})
	if v := p.decide([]string{"reboot"}); v == "" {
		t.Error("deny_commands ignored on an open connection")
	}
	if v := p.decide([]string{"uptime"}); v != "" {
		t.Errorf("open connection with deny refused uptime: %s", v)
	}
	if v := p.shellAllowed(); !strings.Contains(v, "deny_commands") {
		t.Errorf("shell=true with a deny list must be refused: %q", v)
	}
	p, _ = newCommandPolicy(config.Connection{})
	if v := p.shellAllowed(); v != "" {
		t.Errorf("shell=true on an open connection refused: %s", v)
	}

	if _, err := newCommandPolicy(config.Connection{AllowCommands: []string{"*"}}); err == nil {
		t.Error("allow_commands * must be rejected")
	}
}

// newTestSSH wires an SSH connection type against the in-process server.
func newTestSSH(t *testing.T, conn config.Connection, opts ...sshtest.Option) (*SSH, *sshtest.Server) {
	t.Helper()
	sshAudit.reset()
	signer, keyPEM := sshtest.GenerateKey(t, "pw")
	opts = append([]sshtest.Option{sshtest.WithAuthorizedKeys(signer.PublicKey()), sshtest.WithUser("deploy")}, opts...)
	srv := sshtest.New(t, opts...)
	conn.Type = config.TypeSSH
	conn.Name = "box"
	conn.Host, conn.Port, conn.User = srv.Host, srv.Port, "deploy"
	conn.PrivateKey, conn.Passphrase = keyPEM, "pw"
	conn.HostKey = srv.AuthorizedKeyLine()
	s, err := NewSSH(conn, nil)
	if err != nil {
		t.Fatalf("NewSSH: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, srv
}

func call(t *testing.T, s *SSH, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
	var res *mcp.CallToolResult
	var err error
	switch tool {
	case "exec":
		res, err = s.handleExec(context.Background(), req)
	case "download":
		res, err = s.handleDownload(context.Background(), req)
	case "upload":
		res, err = s.handleUpload(context.Background(), req)
	default:
		t.Fatalf("unknown tool %s", tool)
	}
	if err != nil {
		t.Fatalf("%s returned a protocol error: %v", tool, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestSSH_ExecBasics(t *testing.T) {
	s, srv := newTestSSH(t, config.Connection{})

	res := call(t, s, "exec", map[string]any{"args": []any{"echo", "hello world"}})
	if res.IsError {
		t.Fatalf("exec failed: %s", text(res))
	}
	out := text(res)
	if !strings.HasPrefix(out, "exit 0 ·") || !strings.Contains(out, "--- stdout ---\nhello world") {
		t.Errorf("unexpected result:\n%s", out)
	}
	if cmds := srv.Commands(); len(cmds) != 1 || cmds[0] != "echo 'hello world'" {
		t.Errorf("server saw %q", cmds)
	}

	// Arguments are literals: no expansion, no operators.
	res = call(t, s, "exec", map[string]any{"args": []any{"echo", "$HOME", "*", "a;b"}})
	if got := text(res); !strings.Contains(got, "$HOME * a;b") {
		t.Errorf("arguments were interpreted:\n%s", got)
	}

	// Non-zero exit is a result, not a tool error; stderr is captured.
	res = call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "echo oops >&2; exit 3"}})
	if res.IsError {
		t.Fatalf("non-zero exit should be a normal result: %s", text(res))
	}
	if got := text(res); !strings.HasPrefix(got, "exit 3 ·") || !strings.Contains(got, "--- stderr ---\noops") {
		t.Errorf("exit/stderr not reported:\n%s", got)
	}

	// cwd and stdin.
	dir := t.TempDir()
	res = call(t, s, "exec", map[string]any{"args": []any{"pwd"}, "cwd": dir})
	if got := text(res); !strings.Contains(got, dir) {
		t.Errorf("cwd not applied:\n%s", got)
	}
	res = call(t, s, "exec", map[string]any{"args": []any{"cat"}, "stdin": "from stdin\n"})
	if got := text(res); !strings.Contains(got, "from stdin") {
		t.Errorf("stdin not passed:\n%s", got)
	}

	// Argument validation.
	for _, bad := range []map[string]any{
		{"args": []any{}},
		{"args": "echo"},
		{"args": []any{"echo", 1}},
		{"args": []any{""}},
		{"args": []any{"echo"}, "timeout": 99999},
		{"args": []any{"echo"}, "timeout": -1},
		{"args": []any{"echo", "x"}, "shell": true},
	} {
		if res := call(t, s, "exec", bad); !res.IsError {
			t.Errorf("args %v accepted", bad)
		}
	}
}

func TestSSH_ExecShellAndDryRun(t *testing.T) {
	s, srv := newTestSSH(t, config.Connection{})

	res := call(t, s, "exec", map[string]any{"args": []any{"echo one | tr o 0"}, "shell": true})
	if res.IsError || !strings.Contains(text(res), "0ne") {
		t.Fatalf("shell=true: %s", text(res))
	}
	if cmds := srv.Commands(); cmds[len(cmds)-1] != "echo one | tr o 0" {
		t.Errorf("shell command was altered: %q", cmds[len(cmds)-1])
	}

	res = call(t, s, "exec", map[string]any{"args": []any{"rm", "-rf", "/tmp/x y"}, "cwd": "/var", "dry_run": true})
	if res.IsError {
		t.Fatal(text(res))
	}
	got := text(res)
	if !strings.Contains(got, "Command line: cd -- /var && rm -rf '/tmp/x y'") || !strings.Contains(got, "Verdict: allowed") {
		t.Errorf("dry run output:\n%s", got)
	}
	if len(srv.Commands()) != 1 {
		t.Error("dry run executed something")
	}
}

func TestSSH_ReadOnly(t *testing.T) {
	s, srv := newTestSSH(t, config.Connection{ReadOnly: true})

	names := map[string]bool{}
	for _, td := range s.Tools() {
		names[td.Tool.Name] = true
	}
	if names["upload"] {
		t.Error("read-only connection exposes upload")
	}
	if !names["exec"] || !names["download"] {
		t.Error("read-only connection lacks exec or download")
	}

	res := call(t, s, "exec", map[string]any{"args": []any{"echo", "x"}})
	if !res.IsError || !strings.Contains(text(res), "not in the built-in read-only list") {
		t.Errorf("echo should be refused on read-only defaults: %s", text(res))
	}
	res = call(t, s, "exec", map[string]any{"args": []any{"echo x"}, "shell": true})
	if !res.IsError || !strings.Contains(text(res), "read-only") {
		t.Errorf("shell=true should be refused on read-only: %s", text(res))
	}
	if len(srv.Commands()) != 0 {
		t.Errorf("refused commands reached the server: %v", srv.Commands())
	}
	// A listed reader runs.
	res = call(t, s, "exec", map[string]any{"args": []any{"uname"}})
	if res.IsError {
		t.Errorf("uname refused on read-only: %s", text(res))
	}

	// Refusals are in the audit trail.
	entries := sshAudit.recent("box", 10)
	var refused int
	for _, e := range entries {
		if e.Refused != "" {
			refused++
		}
	}
	if refused != 2 {
		t.Errorf("audit has %d refusals, want 2", refused)
	}
}

func TestSSH_ExecTimeoutAndTruncation(t *testing.T) {
	s, _ := newTestSSH(t, config.Connection{})

	start := time.Now()
	res := call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "echo partial; sleep 30"}, "timeout": 1})
	if !res.IsError {
		t.Fatalf("timed-out command returned success: %s", text(res))
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("timeout took %s", time.Since(start))
	}
	got := text(res)
	if !strings.Contains(got, "timed out after 1s") || !strings.Contains(got, "--- stdout ---\npartial") {
		t.Errorf("timeout result should name the timeout and carry partial output:\n%s", got)
	}

	// 200 KB of output: inline is capped and says so; output_file keeps it all.
	res = call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "head -c 204800 /dev/zero | tr '\\0' x"}})
	got = text(res)
	if !strings.Contains(got, "stdout 204800 B") || !strings.Contains(got, "[stdout truncated: showing the first 65536 of 204800 bytes") {
		t.Errorf("truncation not announced:\n%s", got[:min(len(got), 300)])
	}
	out := filepath.Join(t.TempDir(), "out.txt")
	res = call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "head -c 204800 /dev/zero | tr '\\0' x"}, "output_file": out})
	if res.IsError {
		t.Fatal(text(res))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() != 204800 {
		t.Errorf("output_file size = %v, %v", fi, err)
	}
	if !strings.Contains(text(res), "stdout saved to: "+out) {
		t.Errorf("result does not name the file:\n%s", text(res))
	}
	// Existing output_file is refused before anything runs.
	res = call(t, s, "exec", map[string]any{"args": []any{"echo", "x"}, "output_file": out})
	if !res.IsError || !strings.Contains(text(res), "already exists") {
		t.Errorf("existing output_file accepted: %s", text(res))
	}
}

func TestSSH_UploadDownload(t *testing.T) {
	s, _ := newTestSSH(t, config.Connection{})
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	payload := strings.Repeat("0123456789abcdef", 8192) // 128 KB
	if err := os.WriteFile(src, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(dir, "remote", "copy.bin")

	// Parent missing → clear error.
	res := call(t, s, "upload", map[string]any{"file_path": src, "remote_path": remote})
	if !res.IsError || !strings.Contains(text(res), "does not exist") {
		t.Fatalf("upload into missing dir: %s", text(res))
	}
	if err := os.Mkdir(filepath.Dir(remote), 0o755); err != nil {
		t.Fatal(err)
	}

	res = call(t, s, "upload", map[string]any{"file_path": src, "remote_path": remote, "mode": "0640"})
	if res.IsError {
		t.Fatalf("upload: %s", text(res))
	}
	got, err := os.ReadFile(remote)
	if err != nil || string(got) != payload {
		t.Fatalf("uploaded content differs (%v)", err)
	}
	if fi, _ := os.Stat(remote); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 0640", fi.Mode().Perm())
	}
	if !strings.Contains(text(res), "131072 bytes") {
		t.Errorf("result: %s", text(res))
	}

	// Existing remote file: refused without overwrite, replaced with it.
	res = call(t, s, "upload", map[string]any{"file_path": src, "remote_path": remote})
	if !res.IsError || !strings.Contains(text(res), "already exists") {
		t.Errorf("overwrite without flag: %s", text(res))
	}
	res = call(t, s, "upload", map[string]any{"file_path": src, "remote_path": remote, "overwrite": true})
	if res.IsError {
		t.Errorf("overwrite=true: %s", text(res))
	}

	// Download round trip; local O_EXCL.
	dst := filepath.Join(dir, "down.bin")
	res = call(t, s, "download", map[string]any{"remote_path": remote, "output_file": dst})
	if res.IsError {
		t.Fatalf("download: %s", text(res))
	}
	got, _ = os.ReadFile(dst)
	if string(got) != payload {
		t.Error("downloaded content differs")
	}
	res = call(t, s, "download", map[string]any{"remote_path": remote, "output_file": dst})
	if !res.IsError || !strings.Contains(text(res), "already exists") {
		t.Errorf("download onto existing file: %s", text(res))
	}
	res = call(t, s, "download", map[string]any{"remote_path": filepath.Join(dir, "nope"), "output_file": filepath.Join(dir, "x")})
	if !res.IsError {
		t.Error("download of a missing remote file succeeded")
	}
	res = call(t, s, "download", map[string]any{"remote_path": dir, "output_file": filepath.Join(dir, "y")})
	if !res.IsError || !strings.Contains(text(res), "directory") {
		t.Errorf("download of a directory: %s", text(res))
	}

	// Validation.
	for _, bad := range []map[string]any{
		{"file_path": "relative.txt", "remote_path": remote},
		{"file_path": src, "remote_path": remote, "mode": "rwx"},
		{"file_path": dir, "remote_path": remote},
	} {
		if res := call(t, s, "upload", bad); !res.IsError {
			t.Errorf("upload args %v accepted", bad)
		}
	}
}

func TestSSH_AuditEndpoint(t *testing.T) {
	s, _ := newTestSSH(t, config.Connection{})
	call(t, s, "exec", map[string]any{"args": []any{"echo", "audited"}})

	rec := httptest.NewRecorder()
	SSHAuditHandler()(rec, httptest.NewRequest("GET", "/audit/ssh?connection=box&limit=5", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Count   int             `json:"count"`
		Entries []SSHAuditEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Count == 0 || body.Entries[0].Command != "echo audited" || body.Entries[0].ExitCode == nil || *body.Entries[0].ExitCode != 0 {
		t.Errorf("audit entry: %+v", body.Entries)
	}
	if body.Entries[0].Tool != "exec" || body.Entries[0].Bytes != 8 {
		t.Errorf("audit entry: %+v", body.Entries[0])
	}
}

func TestSSH_RegistrationErrors(t *testing.T) {
	_, keyPEM := sshtest.GenerateKey(t, "pw")
	base := config.Connection{Name: "box", Type: config.TypeSSH, Host: "h", User: "u", PrivateKey: keyPEM}

	if _, err := NewSSH(base, nil); err == nil || !strings.Contains(err.Error(), "box-private-key-passphrase") {
		t.Errorf("encrypted key without passphrase: %v", err)
	}
	c := base
	c.Passphrase = "pw"
	c.Auth = "password"
	if _, err := NewSSH(c, nil); err == nil || !strings.Contains(err.Error(), "auth must be") {
		t.Errorf("bad auth: %v", err)
	}
	c = base
	c.Passphrase = "pw"
	c.HostKey = "garbage"
	if _, err := NewSSH(c, nil); err == nil || !strings.Contains(err.Error(), "host_key") {
		t.Errorf("bad host_key: %v", err)
	}
	c = base
	c.Passphrase = "pw"
	c.AllowCommands = []string{"*"}
	if _, err := NewSSH(c, nil); err == nil || !strings.Contains(err.Error(), "allow_commands") {
		t.Errorf("bad allow_commands: %v", err)
	}
	c = base
	c.Passphrase = "pw"
	s, err := NewSSH(c, nil)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	s.Close()
}

// TestSSH_ExecTimeoutWithoutSignalSupport: a server that ignores the signal
// request (sshd before 7.9, forced-command sessions) must still let the tool
// call return in bounded time — through the session close, step two of abort.
func TestSSH_ExecTimeoutWithoutSignalSupport(t *testing.T) {
	s, srv := newTestSSH(t, config.Connection{}, sshtest.WithoutSignalSupport())
	start := time.Now()
	res := call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "echo started; sleep 30"}, "timeout": 1})
	if !res.IsError {
		t.Fatalf("expected a timeout error: %s", text(res))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("call took %s; abort did not bound it", d)
	}
	got := text(res)
	if !strings.Contains(got, "timed out after 1s") || !strings.Contains(got, "others may still run it") || !strings.Contains(got, "started") {
		t.Errorf("result:\n%s", got)
	}
	// The connection survives the abort; the next command runs on it.
	res = call(t, s, "exec", map[string]any{"args": []any{"echo", "alive"}})
	if res.IsError || !strings.Contains(text(res), "alive") {
		t.Errorf("connection unusable after abort: %s", text(res))
	}
	_ = srv
}

// TestSSH_TransferDeadline: a transfer that does not finish within the budget
// is aborted, the local partial file removed, and the remote partial file
// removed through a fresh session.
func TestSSH_TransferDeadline(t *testing.T) {
	old := uploadTimeout
	uploadTimeout = 400 * time.Millisecond
	t.Cleanup(func() { uploadTimeout = old })

	s, _ := newTestSSH(t, config.Connection{}, sshtest.WithSlowSFTP(60*time.Millisecond))
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 1<<20)), 0o600); err != nil { // 1 MB = 32 SFTP packets
		t.Fatal(err)
	}
	remote := filepath.Join(dir, "remote.bin")
	res := call(t, s, "upload", map[string]any{"file_path": src, "remote_path": remote})
	if !res.IsError || !strings.Contains(text(res), "transfer aborted after 400ms") {
		t.Fatalf("upload under deadline: %s", text(res))
	}
	if _, err := os.Stat(remote); err == nil {
		t.Errorf("partial remote file was left behind: %s", text(res))
	}

	// Download the other way round: the local partial file must be gone.
	if err := os.WriteFile(remote, []byte(strings.Repeat("y", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "down.bin")
	res = call(t, s, "download", map[string]any{"remote_path": remote, "output_file": dst})
	if !res.IsError || !strings.Contains(text(res), "transfer aborted") {
		t.Fatalf("download under deadline: %s", text(res))
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("partial local file was left behind")
	}
	// The connection is usable afterwards (the deadline path reconnects when needed).
	res = call(t, s, "exec", map[string]any{"args": []any{"echo", "ok"}})
	if res.IsError {
		t.Errorf("exec after aborted transfers: %s", text(res))
	}
}

// TestSSH_ExecOutputFileAndErrors covers the output_file corner cases: the
// parent directory is created, a non-zero exit keeps the file, a command that
// never produces output leaves no empty file behind on failure, and stderr is
// truncated with a note.
func TestSSH_ExecOutputFileAndErrors(t *testing.T) {
	s, _ := newTestSSH(t, config.Connection{})
	dir := t.TempDir()

	out := filepath.Join(dir, "sub", "dir", "out.txt")
	res := call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "echo data; exit 2"}, "output_file": out})
	if res.IsError {
		t.Fatalf("non-zero exit with output_file: %s", text(res))
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != "data\n" {
		t.Errorf("output_file content = %q, %v", got, err)
	}
	if !strings.HasPrefix(text(res), "exit 2 ·") || !strings.Contains(text(res), "stdout saved to: "+out) {
		t.Errorf("result:\n%s", text(res))
	}

	// Timeout before any output: no empty file is left to block the retry.
	out2 := filepath.Join(dir, "empty.txt")
	res = call(t, s, "exec", map[string]any{"args": []any{"sleep", "30"}, "timeout": 1, "output_file": out2})
	if !res.IsError {
		t.Fatal("expected timeout")
	}
	if _, err := os.Stat(out2); err == nil {
		t.Error("empty output_file left behind after a timeout without output")
	}
	if strings.Contains(text(res), "saved to") {
		t.Errorf("result advertises a file that does not exist:\n%s", text(res))
	}

	// stderr beyond 16 KB is announced as truncated.
	res = call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "head -c 40000 /dev/zero | tr '\\0' e >&2"}})
	if !strings.Contains(text(res), "stderr 40000 B") || !strings.Contains(text(res), "[stderr truncated: showing the first 16384 of 40000 bytes]") {
		t.Errorf("stderr truncation not announced:\n%s", text(res)[:min(len(text(res)), 300)])
	}

	// Killed by a signal from the host side is reported as such. KILL, not
	// TERM: dash (Linux /bin/sh) handles TERM itself and exits 143, bash on
	// macOS dies by the signal — KILL ends the shell the same way everywhere.
	res = call(t, s, "exec", map[string]any{"args": []any{"sh", "-c", "kill -KILL $$"}})
	if !res.IsError || !strings.Contains(text(res), "killed by signal") {
		t.Errorf("signal death: %s", text(res))
	}
}

// TestSSH_RefusedSessionKeepsConnection: a server that refuses a session
// (MaxSessions) is a live server; the connection must not be torn down.
func TestSSH_SubsystemRefusedKeepsConnection(t *testing.T) {
	s, srv := newTestSSH(t, config.Connection{})
	// Run a long command on the connection, then make an SFTP request fail
	// at the subsystem level by pointing download at a server without SFTP:
	// the test server always has SFTP, so exercise the classifier directly.
	if sshclient.IsTransportError(errors.New("ssh: subsystem request failed")) {
		t.Fatal("a refused subsystem is not a transport error")
	}
	_ = srv
	res := call(t, s, "exec", map[string]any{"args": []any{"echo", "x"}})
	if res.IsError {
		t.Fatal(text(res))
	}
}

// TestCapturedStream_WriteContract: the writer keeps only limit bytes but
// must report every byte as written — io.Copy treats anything less as a
// short write and aborts the copy (the first CI run on Linux, whose chunks
// are 32 KB, hit exactly that).
func TestCapturedStream_WriteContract(t *testing.T) {
	c := &capturedStream{limit: 10}
	for _, chunk := range [][]byte{[]byte("0123456789abcdef"), []byte("more"), {}} {
		n, err := c.Write(chunk)
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%d bytes) = %d, %v", len(chunk), n, err)
		}
	}
	if c.total != 20 || c.buf.String() != "0123456789" || !c.truncated() {
		t.Errorf("total=%d buf=%q truncated=%v", c.total, c.buf.String(), c.truncated())
	}
}
