# Contributing to mux

Thank you for your interest in contributing! This document provides guidelines for contributing to mux.

## Development Setup

### Prerequisites

- Go 1.24+ (check `go.mod` for the exact version)
- Node.js 22+ (for the desktop frontend)
- Make

### Building

```bash
# Headless binary (servers, CI)
CGO_ENABLED=0 go build -tags notray -ldflags "-s -w" -o mux .

# Desktop app (macOS, Windows — requires CGO + Wails v3)
make build

# Run tests
go test ./internal/...
```

### Project Structure

```
internal/
├── config/     # Configuration loading, secret management, keychain
├── provisioning/ # Remote provisioning client
├── proxy/      # MCP proxy mounts (OAuth, bearer token)
├── tools/      # MCP tool implementations (DB, API, config management)
├── tunnel/     # SSH tunnel transport
├── vault/      # Encrypted secret store, WebAuthn, approval flow
└── wireguard/  # WireGuard tunnel manager

frontend/       # Svelte desktop UI (Wails v3)
main.go         # Entry point, transport detection, server startup
register.go     # Connection registration and hot-reload
```

## Adding a New Connection Type

1. Add a `Type<Name>` constant in `internal/config/identifiers.go` (the wire-format string is the constant's value)
2. Add the type to `AllTypes` in `internal/config/types.go` using the new constant
3. Create a tool file in `internal/tools/` (see existing ones as examples)
4. Add a case in `tools.RegisterConnection()` in `internal/tools/tools.go`
5. Add a TOML example in `config.example.toml`
6. Document in `docs/connections.md`

## Pull Requests

- Target the `main` branch
- Keep PRs focused — one feature or fix per PR
- Include a clear description of what changed and why
- Ensure `go test ./internal/...` passes
- Follow existing code style (run `gofmt`)

## Code Style

- Standard Go formatting (`gofmt`)
- Error wrapping with `%w` for context
- Structured logging via `log.Printf("[component] message")`
- Mutex discipline: document what each mutex protects
- No `panic` in library code — return errors

## Reporting Issues

- Use GitHub Issues for bug reports and feature requests
- For security vulnerabilities, see [SECURITY.md](SECURITY.md)
