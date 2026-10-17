package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/sshclient"
)

// File copies for the ssh connection type, over the SFTP subsystem.
//
// Both directions stream between the two disks: mux opens the local file and
// the remote file and copies, so the bytes never enter the tool result or the
// model's context. The local side refuses to overwrite (O_EXCL, like
// output_file everywhere else in mux); the remote side refuses too unless
// overwrite=true. A copy that fails halfway removes what it created, so a
// retry does not hit its own debris.

// sftpConn is an SFTP client plus the SSH connection it runs on, so a stalled
// transfer can be torn down at the transport when the subsystem does not
// answer anymore.
type sftpConn struct {
	*sftp.Client
	ssh *ssh.Client
}

// sftpSession opens an SFTP client on the connection, reconnecting once when
// the SSH connection has died.
func (s *SSH) sftpSession(ctx context.Context) (*sftpConn, error) {
	client, err := s.client.Get(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := newSFTP(ctx, client)
	if err == nil {
		return &sftpConn{Client: sc, ssh: client}, nil
	}
	if ctx.Err() != nil {
		s.client.Invalidate(client)
		return nil, fmt.Errorf("open sftp subsystem: %w", err)
	}
	if !sshclient.IsTransportError(err) {
		// The server answered and said no: no SFTP subsystem, or no session
		// slot. The connection is fine; reconnecting would only cut the
		// commands running on it.
		return nil, fmt.Errorf("open sftp subsystem: %w (does the host run an SFTP server, and does it allow another session?)", err)
	}
	client, err = s.client.Reconnect(ctx, client)
	if err != nil {
		return nil, err
	}
	sc, err = newSFTP(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("open sftp subsystem: %w", err)
	}
	return &sftpConn{Client: sc, ssh: client}, nil
}

// newSFTP bounds sftp.NewClient, which has no context, by ctx.
func newSFTP(ctx context.Context, client *ssh.Client) (*sftp.Client, error) {
	type result struct {
		sc  *sftp.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		sc, err := sftp.NewClient(client)
		ch <- result{sc, err}
	}()
	select {
	case r := <-ch:
		return r.sc, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.sc != nil {
				r.sc.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// copyWithDeadline runs copy and aborts it when the deadline passes. pkg/sftp
// has no context support; closing the SFTP client aborts an in-flight
// transfer when the peer still answers, and closing the SSH connection does
// when it does not (the same escalation exec uses). A stalled copy must end
// in bounded time, or the tool call never returns.
func (s *SSH) copyWithDeadline(ctx context.Context, sc *sftpConn, copy func() (int64, error)) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := copy()
		done <- result{n, err}
	}()
	var r result
	select {
	case r = <-done:
		return r.n, r.err
	case <-ctx.Done():
	}
	go sc.Close()
	select {
	case r = <-done:
	case <-time.After(sshKillGrace):
		log.Printf("[ssh] %s: sftp did not stop on close, dropping the connection", s.name)
		s.client.Invalidate(sc.ssh)
		r = <-done
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return r.n, fmt.Errorf("transfer aborted after %s: the host did not finish taking or giving the file", uploadTimeout)
	}
	return r.n, ctx.Err()
}

func (s *SSH) handleDownload(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	remotePath, err := stringArg(req, "remote_path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(remotePath) == "" {
		return mcp.NewToolResultError("remote_path is required"), nil
	}
	outputFile, err := stringArg(req, "output_file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if outputFile == "" {
		return mcp.NewToolResultError("output_file is required — the file is written to disk, not returned inline (use exec with cat for small text files)"), nil
	}
	outPath, err := validateOutputFile(outputFile)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	start := time.Now()
	n, copyErr := s.download(ctx, remotePath, outPath)
	entry := SSHAuditEntry{
		Connection: s.name, Target: s.client.Target(), Tool: "download",
		Command:    fmt.Sprintf("%s → %s", remotePath, outPath),
		DurationMS: time.Since(start).Milliseconds(), Bytes: n,
	}
	if copyErr != nil {
		entry.Error = copyErr.Error()
	}
	sshAudit.record(entry)

	if copyErr != nil {
		return mcp.NewToolResultError(copyErr.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Downloaded %s:%s → %s (%d bytes, %s)",
		s.client.Target(), remotePath, outPath, n, time.Since(start).Round(time.Millisecond))), nil
}

func (s *SSH) download(ctx context.Context, remotePath, outPath string) (int64, error) {
	sc, err := s.sftpSession(ctx)
	if err != nil {
		return 0, err
	}
	defer sc.Close()

	info, err := sc.Stat(remotePath)
	if err != nil {
		return 0, fmt.Errorf("remote_path %s: %w", remotePath, err)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("remote_path %s is a directory — download copies one file", remotePath)
	}
	src, err := sc.Open(remotePath)
	if err != nil {
		return 0, fmt.Errorf("open remote %s: %w", remotePath, err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 0, fmt.Errorf("create directory: %w", err)
	}
	dst, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return 0, fmt.Errorf("output_file already exists (refusing to overwrite): %s", outPath)
		}
		return 0, fmt.Errorf("create output_file: %w", err)
	}

	n, err := s.copyWithDeadline(ctx, sc, func() (int64, error) {
		return io.Copy(dst, &touchReader{r: src, touch: s.client.Touch})
	})
	if cerr := dst.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(outPath)
		return n, fmt.Errorf("download %s: %w", remotePath, err)
	}
	if info.Size() > 0 && n != info.Size() {
		// Size changed underneath us or the server lied; say so rather than
		// hand over a file the caller believes complete. A reported size of
		// 0 is not a claim — /proc and /sys files report 0 and have content.
		os.Remove(outPath)
		return n, fmt.Errorf("download %s: got %d bytes, the host reported %d", remotePath, n, info.Size())
	}
	return n, nil
}

func (s *SSH) handleUpload(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	filePath, err := stringArg(req, "file_path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	remotePath, err := stringArg(req, "remote_path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if filePath == "" || strings.TrimSpace(remotePath) == "" {
		return mcp.NewToolResultError("file_path and remote_path are required"), nil
	}
	modeText, err := stringArg(req, "mode")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	mode := os.FileMode(0o644)
	if modeText != "" {
		bits, perr := strconv.ParseUint(strings.TrimPrefix(modeText, "0o"), 8, 32)
		if perr != nil || bits > 0o7777 {
			return mcp.NewToolResultError(fmt.Sprintf("mode must be octal permission bits like 0644, got %q", modeText)), nil
		}
		mode = os.FileMode(bits)
	}
	overwrite := req.GetBool("overwrite", false)

	local := filepath.Clean(config.ExpandHome(filePath))
	if !filepath.IsAbs(local) {
		return mcp.NewToolResultError(fmt.Sprintf("file_path must be an absolute path: %s", filePath)), nil
	}
	fi, err := os.Stat(local)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return mcp.NewToolResultError(fmt.Sprintf("file_path not found: %s (resolved on the machine running mux)", local)), nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("file_path is not usable: %v", err)), nil
	}
	if !fi.Mode().IsRegular() {
		return mcp.NewToolResultError(fmt.Sprintf("file_path is not a regular file: %s", local)), nil
	}

	start := time.Now()
	n, copyErr := s.upload(ctx, local, remotePath, mode, overwrite)
	entry := SSHAuditEntry{
		Connection: s.name, Target: s.client.Target(), Tool: "upload",
		Command:    fmt.Sprintf("%s → %s", local, remotePath),
		DurationMS: time.Since(start).Milliseconds(), Bytes: n,
	}
	if copyErr != nil {
		entry.Error = copyErr.Error()
	}
	sshAudit.record(entry)

	if copyErr != nil {
		return mcp.NewToolResultError(copyErr.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Uploaded %s → %s:%s (%d bytes, mode %04o, %s)",
		local, s.client.Target(), remotePath, n, mode, time.Since(start).Round(time.Millisecond))), nil
}

func (s *SSH) upload(ctx context.Context, local, remotePath string, mode os.FileMode, overwrite bool) (int64, error) {
	src, err := os.Open(local)
	if err != nil {
		return 0, fmt.Errorf("open file_path: %w", err)
	}
	defer src.Close()

	sc, err := s.sftpSession(ctx)
	if err != nil {
		return 0, err
	}
	defer sc.Close()

	if info, err := sc.Stat(remotePath); err == nil && info.IsDir() {
		return 0, fmt.Errorf("remote_path %s is a directory — give the file's full destination path", remotePath)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !overwrite {
		flags |= os.O_EXCL
	}
	dst, err := sc.OpenFile(remotePath, flags)
	if err != nil {
		if !overwrite {
			if _, serr := sc.Stat(remotePath); serr == nil {
				return 0, fmt.Errorf("remote_path %s already exists (pass overwrite=true to replace it)", remotePath)
			}
		}
		if _, serr := sc.Stat(path.Dir(remotePath)); serr != nil {
			return 0, fmt.Errorf("remote directory %s does not exist (create it with exec mkdir first)", path.Dir(remotePath))
		}
		return 0, fmt.Errorf("create remote %s: %w", remotePath, err)
	}

	// Uploads see no incoming payload; the SFTP acknowledgements the client
	// library consumes are traffic too, but invisible here. Touch per chunk
	// written instead: a write that returns means the server acknowledged
	// earlier chunks (the library bounds requests in flight).
	n, err := s.copyWithDeadline(ctx, sc, func() (int64, error) {
		return io.Copy(&touchWriter{w: dst, touch: s.client.Touch}, src)
	})
	if cerr := dst.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		if !overwrite {
			// We created it; do not leave a partial file behind. After an
			// aborted transfer this SFTP session is closed, so the removal
			// goes through a fresh one — if the connection is still there.
			if rerr := s.removeRemote(sc, remotePath); rerr != nil {
				return n, fmt.Errorf("upload to %s: %w — a partial file may remain there (%v)", remotePath, err, rerr)
			}
		}
		return n, fmt.Errorf("upload to %s: %w", remotePath, err)
	}
	if err := sc.Chmod(remotePath, mode); err != nil {
		return n, fmt.Errorf("uploaded %d bytes, but chmod %04o failed: %w", n, mode, err)
	}
	return n, nil
}

// touchReader and touchWriter report transfer progress to the connection's
// keepalive, so a slow file copy is not mistaken for a dead connection.
type touchReader struct {
	r     io.Reader
	touch func()
}

func (t *touchReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}

type touchWriter struct {
	w     io.Writer
	touch func()
}

func (t *touchWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 {
		t.touch()
	}
	return n, err
}

// removeRemote deletes a file the upload created but did not finish. It
// tries the session the upload used; when that one is gone (an aborted
// transfer closes it), it opens a fresh one on the same connection, and gives
// up when the connection itself is gone.
func (s *SSH) removeRemote(sc *sftpConn, remotePath string) error {
	if err := sc.Remove(remotePath); err == nil {
		return nil
	}
	if !s.client.IsCurrent(sc.ssh) {
		return errors.New("connection was dropped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fresh, err := newSFTP(ctx, sc.ssh)
	if err != nil {
		return err
	}
	defer fresh.Close()
	return fresh.Remove(remotePath)
}
