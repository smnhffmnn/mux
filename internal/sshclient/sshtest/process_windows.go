//go:build windows

package sshtest

import "os/exec"

// Windows has no process groups in the POSIX sense and no /bin/sh; the
// server compiles there so `go test ./...` does, but tests that run commands
// are expected to be skipped on Windows.
func setProcessGroup(*exec.Cmd) {}

func signalProcessGroup(cmd *exec.Cmd, _ bool) {
	_ = cmd.Process.Kill()
}

func exitSignal(*exec.ExitError) (string, bool) { return "", false }
