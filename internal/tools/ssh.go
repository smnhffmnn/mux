package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/sshclient"
)

// SSH connection type: commands and file copies on a remote host.
//
// The point of the type is that the agent never holds the credential. mux
// keeps the key (or asks the user's ssh-agent), opens the connection, runs
// what it is asked, and writes every operation to the audit trail. The agent
// sees exit codes and output, the same way it sees query results from a
// database connection without knowing the password.
//
// Three tools: exec runs one program with arguments; download and upload move
// a file over SFTP, streamed between the two disks — the bytes never pass
// through the conversation (the counterpart of output_file and file_path on
// the http type). A read-only connection has no upload, and exec is limited
// to an allow list of reading commands.

const (
	sshExecDefaultTimeout = 60 * time.Second
	sshStdoutInlineMax    = 64 * 1024
	sshStderrInlineMax    = 16 * 1024
	// sshKillGrace is how long a timed-out command gets after SIGKILL before
	// the session is torn down regardless.
	sshKillGrace = 2 * time.Second
)

// sshExecMaxTimeout is the cap a caller can raise timeout to. It is the same
// budget uploads get: one bound for "an operation that takes long". Read at
// call time, so a test that shortens uploadTimeout shortens this too.
func sshExecMaxTimeout() time.Duration { return uploadTimeout }

// DefaultSSHInstructions are shown to AI clients when a connection has no
// instructions of its own.
const DefaultSSHInstructions = `Remote host over SSH. exec runs ONE program with its arguments as a list — no shell, no pipes or globs unless shell=true is allowed on this connection; pass cwd and stdin as parameters. Output is limited inline; pass output_file to keep large stdout. Use dry_run to see the exact command line and whether the connection's policy allows it. download and upload copy files between this machine and the host by path. Every operation is logged on the mux side.`

// SSH wraps one remote host as MCP tools.
type SSH struct {
	name         string
	client       *sshclient.Client
	policy       *commandPolicy
	readOnly     bool
	instructions string
}

// NewSSH creates an SSH connection from config. The key is parsed and the
// host key policy validated here, so a misconfigured connection registers no
// tools and shows up in the health report instead of failing at the first
// command.
func NewSSH(conn config.Connection, dialer Dialer) (*SSH, error) {
	if conn.Host == "" || conn.User == "" {
		return nil, fmt.Errorf("ssh: host and user are required")
	}
	if !sshclient.ValidAuth(conn.Auth) {
		return nil, fmt.Errorf("ssh: auth must be %q or %q, got %q", sshclient.AuthKey, sshclient.AuthAgent, conn.Auth)
	}
	policy, err := newCommandPolicy(conn)
	if err != nil {
		return nil, fmt.Errorf("ssh: %w", err)
	}

	var sshDialer sshclient.Dialer
	if dialer != nil {
		sshDialer = dialer
	}
	client, err := sshclient.New(sshclient.Config{
		Name: conn.Name,
		Host: conn.Host,
		Port: conn.Port,
		User: conn.User,
		Key: sshclient.KeySource{
			Auth:       conn.Auth,
			PrivateKey: conn.PrivateKey,
			Passphrase: conn.Passphrase,
			KeyFile:    config.ExpandHome(conn.KeyFile),
			SecretName: conn.Name + "-private-key",
		},
		HostKey: sshclient.HostKeyPolicy{
			Pinned:     conn.HostKey,
			KnownHosts: sshclient.DefaultKnownHostsFiles(config.Dir()),
		},
		Dialer: sshDialer,
	})
	if err != nil {
		return nil, err
	}

	return &SSH{
		name:         conn.Name,
		client:       client,
		policy:       policy,
		readOnly:     conn.ReadOnly,
		instructions: conn.Instructions,
	}, nil
}

// Close shuts the SSH connection down (io.Closer for the registry).
func (s *SSH) Close() error { return s.client.Close() }

// Tools returns the MCP tools for this connection.
func (s *SSH) Tools() []ToolDef {
	target := s.client.Target()
	execDesc := fmt.Sprintf("Run one program on %s over SSH and return exit code, stdout and stderr. "+
		"args is the argument vector — the program first, then its arguments — each sent as a literal (quoted for the remote shell), so no globbing, pipes or redirects happen unless shell=true. "+
		"Policy on this connection: %s.", target, s.policy.describe())
	if s.readOnly {
		execDesc += " The connection is read-only: shell=true is refused and only the listed reading commands run."
	}
	if s.instructions != "" {
		execDesc += "\n\n" + s.instructions
	}

	tools := []ToolDef{
		{
			Tool: mcp.NewTool("exec",
				mcp.WithDescription(execDesc),
				mcp.WithArray("args", mcp.Required(),
					mcp.Description(`Program and arguments as a list, e.g. ["systemctl", "status", "nginx"]. With shell=true a single element is the command line for the remote shell.`),
					mcp.WithStringItems(),
				),
				mcp.WithString("cwd", mcp.Description("Directory to run in (absolute path on the host). Default: the login directory.")),
				mcp.WithString("stdin", mcp.Description("Text written to the program's standard input, then EOF.")),
				mcp.WithNumber("timeout", mcp.Description(fmt.Sprintf("Seconds until mux stops waiting and sends SIGKILL. Default %d, at most %d (%s).",
					int(sshExecDefaultTimeout.Seconds()), int(sshExecMaxTimeout().Seconds()), sshExecMaxTimeout()))),
				mcp.WithString("output_file", mcp.Description("Save stdout to this absolute local path instead of returning it inline (supports ~). Unlimited size; an existing file is never overwritten. stderr and the exit code are still returned.")),
				mcp.WithBoolean("shell", mcp.Description("Pass args[0] verbatim to the remote login shell (pipes, globs, redirects). Only on a connection without allow_commands and deny_commands that is not read-only; args must have exactly one element.")),
				mcp.WithBoolean("dry_run", mcp.Description("Do not run: return the exact command line that would be sent and whether the connection's policy allows it.")),
			),
			Handler: s.handleExec,
		},
		{
			Tool: mcp.NewTool("download",
				mcp.WithDescription(fmt.Sprintf("Copy a file from %s to this machine over SFTP, streamed to disk. Returns path and size; the content never passes through the conversation.", target)),
				mcp.WithString("remote_path", mcp.Required(), mcp.Description("File on the host (absolute, or relative to the login directory).")),
				mcp.WithString("output_file", mcp.Required(), mcp.Description("Absolute local destination path (supports ~). An existing file is never overwritten.")),
			),
			Handler: s.handleDownload,
		},
	}

	if !s.readOnly {
		tools = append(tools, ToolDef{
			Tool: mcp.NewTool("upload",
				mcp.WithDescription(fmt.Sprintf("Copy a local file to %s over SFTP, streamed from disk. The path is read by the mux process, so it must be visible to it (in a container: mounted).", target)),
				mcp.WithString("file_path", mcp.Required(), mcp.Description("Absolute local source path (supports ~), a regular file.")),
				mcp.WithString("remote_path", mcp.Required(), mcp.Description("Destination path on the host (absolute, or relative to the login directory). The parent directory must exist.")),
				mcp.WithString("mode", mcp.Description("Permission bits for the new file as octal text, e.g. 0644 (default) or 0755.")),
				mcp.WithBoolean("overwrite", mcp.Description("Replace an existing remote file. Default false: an existing file is an error.")),
			),
			Handler: s.handleUpload,
		})
	}

	return tools
}

// --- exec ---

type execRequest struct {
	argv       []string
	cwd        string
	stdin      string
	timeout    time.Duration
	outputFile string
	shell      bool
	dryRun     bool
}

func (s *SSH) parseExec(req mcp.CallToolRequest) (*execRequest, error) {
	args := req.GetArguments()
	raw, ok := args["args"]
	if !ok || raw == nil {
		return nil, errors.New("args is required")
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("args must be an array of strings, got %T", raw)
	}
	if len(list) == 0 {
		return nil, errors.New("args must name a program")
	}
	argv := make([]string, len(list))
	for i, v := range list {
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("args[%d] must be a string, got %T", i, v)
		}
		argv[i] = str
	}
	if strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("args[0] (the program) is empty")
	}

	e := &execRequest{argv: argv, timeout: sshExecDefaultTimeout}
	var err error
	if e.cwd, err = stringArg(req, "cwd"); err != nil {
		return nil, err
	}
	if e.stdin, err = stringArg(req, "stdin"); err != nil {
		return nil, err
	}
	if e.outputFile, err = stringArg(req, "output_file"); err != nil {
		return nil, err
	}
	if v, ok := args["timeout"]; ok && v != nil {
		secs, ok := numberArg(v)
		if !ok {
			return nil, fmt.Errorf("timeout must be a number of seconds, got %T", v)
		}
		if secs <= 0 {
			return nil, errors.New("timeout must be positive")
		}
		if secs > sshExecMaxTimeout().Seconds() {
			return nil, fmt.Errorf("timeout %gs exceeds the cap of %s", secs, sshExecMaxTimeout())
		}
		e.timeout = time.Duration(secs * float64(time.Second))
	}
	e.shell = req.GetBool("shell", false)
	e.dryRun = req.GetBool("dry_run", false)

	if e.shell && len(argv) != 1 {
		return nil, errors.New("shell=true takes exactly one element in args: the command line for the remote shell")
	}
	if e.cwd != "" && strings.ContainsAny(e.cwd, "\x00") {
		return nil, errors.New("cwd contains a NUL byte")
	}
	for i, a := range argv {
		if strings.ContainsRune(a, 0) {
			return nil, fmt.Errorf("args[%d] contains a NUL byte", i)
		}
	}
	return e, nil
}

// commandLine renders what is sent in the SSH exec request.
func (e *execRequest) commandLine() string {
	var cmd string
	if e.shell {
		cmd = e.argv[0]
	} else {
		cmd = shellQuote(e.argv)
	}
	if e.cwd != "" {
		cmd = "cd -- " + quoteArg(e.cwd) + " && " + cmd
	}
	return cmd
}

// policyVerdict applies the connection's policy to the request. shell=true
// cannot be checked against a list and is only allowed where no list applies.
func (s *SSH) policyVerdict(e *execRequest) string {
	if e.shell {
		return s.policy.shellAllowed()
	}
	return s.policy.decide(e.argv)
}

func (s *SSH) handleExec(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	e, err := s.parseExec(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	cmd := e.commandLine()
	verdict := s.policyVerdict(e)

	if e.dryRun {
		var b strings.Builder
		fmt.Fprintf(&b, "dry run — nothing was executed\n\nTarget: %s\nCommand line: %s\nPolicy: %s\n", s.client.Target(), cmd, s.policy.describe())
		if s.policy.usingDefaults {
			fmt.Fprintf(&b, "Built-in allow list: %s\n", strings.Join(DefaultReadOnlyCommands(), ", "))
		}
		if verdict == "" {
			b.WriteString("Verdict: allowed\n")
		} else {
			b.WriteString("Verdict: " + verdict + "\n")
		}
		fmt.Fprintf(&b, "Timeout: %s", e.timeout)
		return mcp.NewToolResultText(b.String()), nil
	}

	if verdict != "" {
		sshAudit.record(SSHAuditEntry{Connection: s.name, Target: s.client.Target(), Tool: "exec", Command: cmd, Refused: verdict})
		return mcp.NewToolResultError(verdict), nil
	}

	// Check the destination before connecting: a command that ran for nothing
	// because the output file existed is the worse order of events.
	var outPath string
	if e.outputFile != "" {
		outPath, err = validateOutputFile(e.outputFile)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	res, runErr := s.run(ctx, cmd, e, outPath)
	// The file exists only if the command was started; an error before that
	// (connect, session) must not advertise a path with nothing behind it,
	// and an empty file from a command that died before writing is removed
	// so the retry with the same output_file is not refused.
	if outPath != "" {
		if fi, err := os.Stat(outPath); err != nil {
			res.outPath = ""
		} else if runErr != nil && fi.Size() == 0 {
			os.Remove(outPath)
			res.outPath = ""
		} else {
			res.outPath = outPath
		}
	}
	entry := SSHAuditEntry{
		Connection: s.name, Target: s.client.Target(), Tool: "exec", Command: cmd,
		DurationMS: res.duration.Milliseconds(), Bytes: res.stdout.total, StderrBytes: res.stderr.total,
	}
	if res.exitKnown {
		code := res.exitCode
		entry.ExitCode = &code
	}
	if runErr != nil {
		entry.Error = runErr.Error()
	}
	sshAudit.record(entry)

	if runErr != nil {
		msg := runErr.Error()
		if res.started() {
			msg += "\n\n" + res.render(false)
		}
		return mcp.NewToolResultError(msg), nil
	}
	return mcp.NewToolResultText(res.render(true)), nil
}

// execResult is what came back from one command.
type execResult struct {
	exitCode  int
	exitKnown bool
	duration  time.Duration
	stdout    *capturedStream
	stderr    *capturedStream
	timedOut  bool
	timeout   time.Duration
	// began is set once the remote accepted the exec request.
	began bool
	// outPath is the output_file that holds stdout, "" when none exists.
	outPath string
}

// started reports whether the command reached the host, i.e. whether exit
// code, duration and output sizes mean anything.
func (r *execResult) started() bool {
	return r.began || r.exitKnown || r.stdout.total > 0 || r.stderr.total > 0
}

// capturedStream keeps the first limit bytes of a stream and counts all of
// them, so the result can say "showing 65536 of 2 MB" instead of ending
// mid-line without a word (the failure MUX-35 is about).
type capturedStream struct {
	buf   bytes.Buffer
	limit int
	total int64
	// touch reports arriving bytes to the connection's keepalive (may be nil).
	touch func()
}

func (c *capturedStream) Write(p []byte) (int, error) {
	n := len(p)
	if c.touch != nil && n > 0 {
		c.touch()
	}
	c.total += int64(n)
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		c.buf.Write(p)
	}
	// Always the full length: a writer that keeps less than it was given
	// must still report it all consumed, or io.Copy stops with ErrShortWrite
	// — which is exactly what happened on the first chunk that crossed the
	// limit.
	return n, nil
}

func (c *capturedStream) truncated() bool { return c.total > int64(c.buf.Len()) }

// run executes cmd and collects the result. stdout goes to outPath when set,
// otherwise into a bounded buffer. A timeout sends SIGKILL and closes the
// session; the result then says so and carries whatever arrived.
func (s *SSH) run(ctx context.Context, cmd string, e *execRequest, outPath string) (*execResult, error) {
	res := &execResult{
		stdout:  &capturedStream{limit: sshStdoutInlineMax, touch: s.client.Touch},
		stderr:  &capturedStream{limit: sshStderrInlineMax, touch: s.client.Touch},
		timeout: e.timeout,
	}
	start := time.Now()
	defer func() { res.duration = time.Since(start) }()

	// The whole call, connection setup included, is bounded by the timeout:
	// a session request on a connection that silently died would otherwise
	// wait for the keepalive to notice.
	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	client, sess, err := s.newSession(runCtx)
	if err != nil {
		if runCtx.Err() != nil && ctx.Err() == nil {
			res.timedOut = true
			return res, fmt.Errorf("no session within %s (cap %s; pass timeout to raise it): %w", e.timeout, sshExecMaxTimeout(), err)
		}
		return res, err
	}
	defer sess.Close()

	var stdout io.Writer = res.stdout
	var outFile *errorRecordingFile
	if outPath != "" {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return res, fmt.Errorf("create directory for output_file: %w", err)
		}
		f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return res, fmt.Errorf("create output_file: %w", err)
		}
		outFile = &errorRecordingFile{File: f}
		// Count bytes for the result; the file gets everything.
		stdout = io.MultiWriter(outFile, &countOnly{n: &res.stdout.total, touch: s.client.Touch})
	}
	if e.stdin != "" {
		sess.Stdin = strings.NewReader(e.stdin)
	}
	sess.Stdout = stdout
	sess.Stderr = res.stderr

	if err := sess.Start(cmd); err != nil {
		if outFile != nil {
			outFile.Close()
			os.Remove(outPath)
		}
		if sshclient.IsTransportError(err) {
			s.client.Invalidate(client)
		}
		return res, fmt.Errorf("start command: %w", err)
	}
	res.began = true

	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()

	var err2 error
	select {
	case err2 = <-waitErr:
	case <-runCtx.Done():
		res.timedOut = ctx.Err() == nil // a caller cancel is not a timeout
		err2 = s.abort(sess, client, waitErr)
	}

	if outFile != nil {
		cerr := outFile.Close()
		if werr := outFile.err; werr != nil {
			// Session.Wait drops a copy error when the exit status is
			// non-zero, and Close reports nothing about earlier writes — so
			// the wrapper is the one place that knows the file is short.
			return res, fmt.Errorf("write output_file %s: %w (the file is incomplete)", outPath, werr)
		}
		if cerr != nil && err2 == nil {
			err2 = fmt.Errorf("write output_file: %w", cerr)
		}
	}

	switch {
	case res.timedOut:
		return res, fmt.Errorf("command timed out after %s (cap %s; pass timeout to raise it): SIGKILL sent and session closed — hosts that honour the signal request (OpenSSH 7.9+, no forced command) have killed the program, others may still run it", e.timeout, sshExecMaxTimeout())
	case ctx.Err() != nil:
		return res, ctx.Err()
	case err2 == nil:
		res.exitCode, res.exitKnown = 0, true
	default:
		var exitErr *ssh.ExitError
		var missing *ssh.ExitMissingError
		switch {
		case errors.As(err2, &exitErr):
			res.exitCode, res.exitKnown = exitErr.ExitStatus(), true
			if sig := exitErr.Signal(); sig != "" {
				return res, fmt.Errorf("command was killed by signal %s", sig)
			}
		case errors.As(err2, &missing):
			return res, errors.New("remote closed the session without an exit status (killed, or the connection dropped)")
		default:
			// A local failure (writing output_file) or a transport error.
			// Only the latter is a reason to give up the connection.
			if sshclient.IsTransportError(err2) {
				s.client.Invalidate(client)
			}
			return res, fmt.Errorf("command failed: %w", err2)
		}
	}
	return res, nil
}

// newSession opens a session on the connection, bounded by ctx. A failure
// that means the connection is gone triggers one reconnect; a refusal from a
// live server (sshd MaxSessions reached) is returned as is, so the sessions
// that are running keep running.
func (s *SSH) newSession(ctx context.Context) (*ssh.Client, *ssh.Session, error) {
	client, err := s.client.Get(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess, err := openSession(ctx, client)
	if err == nil {
		return client, sess, nil
	}
	if ctx.Err() != nil {
		s.client.Invalidate(client)
		return nil, nil, fmt.Errorf("open session: %w", err)
	}
	if !sshclient.IsTransportError(err) {
		return nil, nil, fmt.Errorf("open session: %w (the server refused another session; sshd MaxSessions?)", err)
	}
	log.Printf("[ssh] %s: new session failed (%v), reconnecting", s.name, err)
	client, err = s.client.Reconnect(ctx, client)
	if err != nil {
		return nil, nil, err
	}
	sess, err = openSession(ctx, client)
	if err != nil {
		return nil, nil, fmt.Errorf("open session: %w", err)
	}
	return client, sess, nil
}

// openSession wraps client.NewSession, which has no context: on a connection
// that stopped answering it blocks until the keepalive gives up on it.
func openSession(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	type result struct {
		sess *ssh.Session
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		sess, err := client.NewSession()
		ch <- result{sess, err}
	}()
	select {
	case r := <-ch:
		return r.sess, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.sess != nil {
				r.sess.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// errorRecordingFile remembers the first write error, which os.File forgets
// and Session.Wait may drop.
type errorRecordingFile struct {
	*os.File
	err error
}

func (f *errorRecordingFile) Write(p []byte) (int, error) {
	n, err := f.File.Write(p)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

// numberArg reads a numeric tool argument. JSON decodes numbers as float64;
// the integer cases cover clients and tests that hand the handler Go values.
func numberArg(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// abort ends a running command in three escalating steps, each given
// sshKillGrace: SIGKILL to the remote process (honoured by OpenSSH 7.9+ for
// sessions without a forced command; older servers ignore it), then closing
// the session channel, then closing the whole SSH connection. The last step exists for a
// dead or stalled connection: Session.Wait returns only when the peer
// acknowledges the channel close, and a peer that has gone away never does —
// closing the transport ends every channel locally and unblocks Wait for
// certain. The connection is lost for the next command, which then reconnects.
func (s *SSH) abort(sess *ssh.Session, client *ssh.Client, waitErr <-chan error) error {
	_ = sess.Signal(ssh.SIGKILL)
	select {
	case err := <-waitErr:
		return err
	case <-time.After(sshKillGrace):
	}
	sess.Close()
	select {
	case err := <-waitErr:
		return err
	case <-time.After(sshKillGrace):
	}
	log.Printf("[ssh] %s: peer did not acknowledge the session close, dropping the connection", s.name)
	s.client.Invalidate(client)
	return <-waitErr
}

type countOnly struct {
	n     *int64
	touch func()
}

func (c *countOnly) Write(p []byte) (int, error) {
	if c.touch != nil && len(p) > 0 {
		c.touch()
	}
	*c.n += int64(len(p))
	return len(p), nil
}

// render formats the result. Header first (exit code, duration, sizes), then
// stdout and stderr with explicit truncation notes.
func (r *execResult) render(complete bool) string {
	var b strings.Builder
	if r.exitKnown {
		fmt.Fprintf(&b, "exit %d", r.exitCode)
	} else {
		b.WriteString("no exit status")
	}
	fmt.Fprintf(&b, " · %s · stdout %d B · stderr %d B", r.duration.Round(time.Millisecond), r.stdout.total, r.stderr.total)
	if !complete {
		b.WriteString(" (partial)")
	}

	if r.outPath != "" {
		if complete {
			fmt.Fprintf(&b, "\n\nstdout saved to: %s", r.outPath)
		} else {
			fmt.Fprintf(&b, "\n\npartial stdout saved to: %s (remove it before retrying with the same output_file)", r.outPath)
		}
	} else if r.stdout.total > 0 {
		b.WriteString("\n\n--- stdout ---\n")
		b.WriteString(printable(r.stdout.buf.Bytes()))
		if r.stdout.truncated() {
			fmt.Fprintf(&b, "\n[stdout truncated: showing the first %d of %d bytes — pass output_file to keep all of it]", r.stdout.buf.Len(), r.stdout.total)
		}
	}
	if r.stderr.total > 0 {
		b.WriteString("\n\n--- stderr ---\n")
		b.WriteString(printable(r.stderr.buf.Bytes()))
		if r.stderr.truncated() {
			fmt.Fprintf(&b, "\n[stderr truncated: showing the first %d of %d bytes]", r.stderr.buf.Len(), r.stderr.total)
		}
	}
	return b.String()
}

// printable makes captured bytes safe to inline: invalid UTF-8 is replaced,
// so binary output does not corrupt the JSON-RPC frame.
func printable(b []byte) string {
	s := strings.TrimRight(string(b), "\n")
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}
