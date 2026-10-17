//go:build unix

package sshtest

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group, so a signal can
// reach the children sh -c spawns.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup sends TERM (or KILL) to the command's process group.
func signalProcessGroup(cmd *exec.Cmd, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	// Negative pid: the whole process group.
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// exitSignal names the signal that ended the process, in the form the SSH
// protocol expects ("KILL", "TERM"), or ok=false for a normal exit.
func exitSignal(ee *exec.ExitError) (string, bool) {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", false
	}
	switch ws.Signal() {
	case syscall.SIGKILL:
		return "KILL", true
	case syscall.SIGTERM:
		return "TERM", true
	case syscall.SIGINT:
		return "INT", true
	case syscall.SIGHUP:
		return "HUP", true
	}
	return "KILL", true
}
