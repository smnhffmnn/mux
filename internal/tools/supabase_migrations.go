package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Database migrations: the files under supabase/migrations/ of a checkout,
// applied through the Management API (POST …/database/migrations), which runs
// the SQL and records the migration in supabase_migrations.schema_migrations
// — the same table the supabase CLI's `db push` writes.
//
// The two writers differ in one point that decides the matching here: the
// CLI records the file's own version (the timestamp prefix of its name), the
// API assigns the version itself and keeps only the name it was given. A
// project whose migrations were applied through the API (Lovable does that)
// therefore has a history whose versions do not match the file names; the
// names do — as the full file stem in older entries, as the part after the
// timestamp in newer ones. A local file counts as applied when the history
// has its version, its stem, or its name part. migrations_apply sends the
// file's stem as the name, so a later run recognises it both ways.

const (
	sbMigrationsDir = "supabase/migrations"
	sbMigrationMax  = 10 << 20 // one migration file
)

// sbMigrationFile is the CLI's naming: <version>_<name>.sql, where version
// is a run of digits (a timestamp) and the name may be empty.
var sbMigrationFile = regexp.MustCompile(`^(\d+)(?:_(.+))?\.sql$`)

// sbLocalMigration is one file of supabase/migrations/.
type sbLocalMigration struct {
	Version string `json:"version"`
	Name    string `json:"name,omitempty"` // the part after the timestamp
	Stem    string `json:"file"`           // file name without .sql
	Path    string `json:"-"`
	Size    int64  `json:"size"`
}

// sbRemoteMigration is one entry of the project's history.
type sbRemoteMigration struct {
	Version string `json:"version"`
	Name    string `json:"name"`
}

func (s *Supabase) listMigrations(ctx context.Context) ([]sbRemoteMigration, error) {
	var raw []sbRemoteMigration
	if _, err := s.callJSON(ctx, "migrations", nil, nil, nil, &raw); err != nil {
		return nil, err
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Version < raw[j].Version })
	return raw, nil
}

func (s *Supabase) handleMigrationsList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	list, err := s.listMigrations(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return sbResultJSON(map[string]any{"project": s.projectRef, "count": len(list), "migrations": list})
}

// sbLocalMigrations reads the migration files of a project root, in name
// order (the order the CLI applies them).
func sbLocalMigrations(projectRoot string) ([]sbLocalMigration, error) {
	dir := filepath.Join(projectRoot, filepath.FromSlash(sbMigrationsDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("path has no %s directory: %s", sbMigrationsDir, projectRoot)
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []sbLocalMigration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := sbMigrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue // the CLI ignores files that do not fit <version>_<name>.sql as well
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, sbLocalMigration{
			Version: m[1],
			Name:    m[2],
			Stem:    strings.TrimSuffix(e.Name(), ".sql"),
			Path:    filepath.Join(dir, e.Name()),
			Size:    fi.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stem < out[j].Stem })
	return out, nil
}

// sbMigrationApplied reports whether the history holds the local file: by
// version (the CLI's record), by the file stem or by the name part (the
// API's record, both forms seen). Returns the matching entry.
func sbMigrationApplied(local sbLocalMigration, history []sbRemoteMigration) (sbRemoteMigration, bool) {
	for _, r := range history {
		if r.Version == local.Version {
			return r, true
		}
		if r.Name != "" && (r.Name == local.Stem || (local.Name != "" && r.Name == local.Name)) {
			return r, true
		}
	}
	return sbRemoteMigration{}, false
}

// sbPendingMigrations splits the local files into applied and pending.
func sbPendingMigrations(local []sbLocalMigration, history []sbRemoteMigration) (applied, pending []sbLocalMigration, matches map[string]sbRemoteMigration) {
	matches = map[string]sbRemoteMigration{}
	for _, l := range local {
		if r, ok := sbMigrationApplied(l, history); ok {
			applied = append(applied, l)
			matches[l.Stem] = r
		} else {
			pending = append(pending, l)
		}
	}
	return applied, pending, matches
}

// sbMigrationsRoot checks the path argument: absolute, a directory, with
// supabase/migrations under it.
func sbMigrationsRoot(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path is required: the project root, the directory that contains supabase/")
	}
	clean := filepath.Clean(config.ExpandHome(raw))
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("path must be an absolute path: %s", raw)
	}
	fi, err := os.Stat(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("path not found: %s (resolved on the machine running mux)", clean)
		}
		return "", fmt.Errorf("path is not usable: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", clean)
	}
	if fi, err := os.Stat(filepath.Join(clean, filepath.FromSlash(sbMigrationsDir))); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("path has no %s directory: %s", sbMigrationsDir, clean)
	}
	return clean, nil
}

func (s *Supabase) handleMigrationsPending(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := stringArg(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	root, err := sbMigrationsRoot(rawPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	local, err := sbLocalMigrations(root)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	history, err := s.listMigrations(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	applied, pending, matches := sbPendingMigrations(local, history)
	type appliedEntry struct {
		sbLocalMigration
		HistoryVersion string `json:"historyVersion"`
		HistoryName    string `json:"historyName,omitempty"`
	}
	ap := make([]appliedEntry, 0, len(applied))
	for _, l := range applied {
		r := matches[l.Stem]
		ap = append(ap, appliedEntry{l, r.Version, r.Name})
	}
	if pending == nil {
		pending = []sbLocalMigration{}
	}
	// History entries no local file accounts for: applied elsewhere, or
	// named in a way the matching does not see.
	known := map[string]bool{}
	for _, r := range matches {
		known[r.Version] = true
	}
	var unmatched []sbRemoteMigration
	for _, r := range history {
		if !known[r.Version] {
			unmatched = append(unmatched, r)
		}
	}
	return sbResultJSON(map[string]any{
		"project":       s.projectRef,
		"migrationsDir": filepath.Join(root, filepath.FromSlash(sbMigrationsDir)),
		"localCount":    len(local),
		"historyCount":  len(history),
		"pendingCount":  len(pending),
		"pending":       pending,
		"applied":       ap,
		"historyOnly":   unmatched,
		"matchingRule":  "a file counts as applied when the history has its version, its file stem or its name part (the part after the timestamp)",
	})
}

func (s *Supabase) handleMigrationsApply(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rawPath, err := stringArg(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	root, err := sbMigrationsRoot(rawPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	only, err := stringArg(req, "file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	only = strings.TrimSuffix(strings.TrimSpace(only), ".sql")
	dryRun := false
	if v, ok := req.GetArguments()["dry_run"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("dry_run must be a boolean, got %T", v)), nil
		}
		dryRun = b
	}
	local, err := sbLocalMigrations(root)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	history, err := s.listMigrations(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	_, pending, _ := sbPendingMigrations(local, history)

	var todo []sbLocalMigration
	if only != "" {
		found := false
		for _, l := range local {
			if l.Stem == only || l.Version == only {
				found = true
				if r, ok := sbMigrationApplied(l, history); ok {
					return mcp.NewToolResultError(fmt.Sprintf("%s.sql is already in the history (version %s, name %q) — nothing to apply", l.Stem, r.Version, r.Name)), nil
				}
				todo = []sbLocalMigration{l}
			}
		}
		if !found {
			return mcp.NewToolResultError(fmt.Sprintf("file %q is not in %s (name it as <version>_<name> or by its version)", only, sbMigrationsDir)), nil
		}
		// A single file out of order is the caller's decision, but it is
		// said so.
	} else {
		todo = pending
	}

	type planned struct {
		sbLocalMigration
		Statements int    `json:"statementsApprox"`
		SHA256     string `json:"sha256"`
	}
	plan := make([]planned, 0, len(todo))
	contents := make([][]byte, 0, len(todo))
	for _, l := range todo {
		if l.Size == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("%s.sql is empty — the API refuses an empty query", l.Stem)), nil
		}
		if l.Size > sbMigrationMax {
			return mcp.NewToolResultError(fmt.Sprintf("%s.sql is %d bytes, more than the %d MB a migration may have here", l.Stem, l.Size, sbMigrationMax>>20)), nil
		}
		data, err := os.ReadFile(l.Path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read %s: %v", l.Path, err)), nil
		}
		sum := sha256.Sum256(data)
		plan = append(plan, planned{l, strings.Count(string(data), ";"), hex.EncodeToString(sum[:])})
		contents = append(contents, data)
	}

	result := map[string]any{
		"project":      s.projectRef,
		"pendingCount": len(pending),
		"planned":      plan,
		"plannedCount": len(plan),
	}
	if only != "" && len(pending) > 1 {
		skipped := make([]string, 0, len(pending))
		for _, p := range pending {
			if p.Stem != only && p.Version != only {
				skipped = append(skipped, p.Stem)
			}
		}
		if len(skipped) > 0 {
			result["note"] = fmt.Sprintf("only %s is applied; still pending afterwards: %s", only, strings.Join(skipped, ", "))
		}
	}
	if dryRun {
		result["dryRun"] = true
		result["applied"] = []any{}
		return sbResultJSON(result)
	}
	if len(plan) == 0 {
		result["applied"] = []any{}
		result["note"] = "nothing pending — every local migration is in the history"
		return sbResultJSON(result)
	}

	type appliedOut struct {
		File    string `json:"file"`
		Version string `json:"historyVersion,omitempty"`
	}
	var applied []appliedOut
	for i, l := range todo {
		body := map[string]any{"query": string(contents[i]), "name": l.Stem}
		// The idempotency key ties a retry (429, a broken connection) to the
		// same migration: name and content, nothing else.
		key := plan[i].SHA256[:32]
		if _, err := s.callJSONHeaders(ctx, "migrations.apply", nil, nil, map[string]string{"Idempotency-Key": l.Stem + "-" + key}, body, nil); err != nil {
			result["applied"] = applied
			result["failed"] = map[string]any{"file": l.Stem, "error": err.Error()}
			remaining := make([]string, 0)
			for _, r := range todo[i+1:] {
				remaining = append(remaining, r.Stem)
			}
			result["remaining"] = remaining
			result["note"] = "stopped at the first error; the migrations before it are applied and recorded, this one is not — fix the file and run again"
			return sbResultJSON(result)
		}
		applied = append(applied, appliedOut{File: l.Stem})
	}
	// Read the history back for the versions the server assigned.
	if after, err := s.listMigrations(ctx); err == nil {
		for i := range applied {
			for _, r := range after {
				if r.Name == applied[i].File {
					applied[i].Version = r.Version
				}
			}
		}
	}
	result["applied"] = applied
	result["appliedCount"] = len(applied)
	return sbResultJSON(result)
}
