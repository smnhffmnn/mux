//go:build windows

package vault

import "net"

// GitHost describes a git credential source derived from a connection config.
type GitHost struct {
	Host      string
	Username  string
	SecretKey string
}

// CredentialSocket is a no-op on Windows (Unix domain sockets with umask are not available).
type CredentialSocket struct {
	listener net.Listener
}

func NewCredentialSocket(_ *Vault, _ []GitHost) *CredentialSocket {
	return &CredentialSocket{}
}

func SocketPath() string { return "" }

func (cs *CredentialSocket) Listen() error { return nil }
func (cs *CredentialSocket) Close()        {}
