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

// sbMigrationFile is the CLI's naming: <version>_<name>.sql, version a run
// of digits (a timestamp), name required — the CLI skips a bare <version>.sql
// and so does this.
var sbMigrationFile = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

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
// order (the order the CLI applies them). skipped names the entries of the
// directory that are not migration files (other names, directories,
// symlinks), so nothing disappears silently.
func sbLocalMigrations(projectRoot string) (files []sbLocalMigration, skipped []string, err error) {
	dir := filepath.Join(projectRoot, filepath.FromSlash(sbMigrationsDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("path has no %s directory: %s", sbMigrationsDir, projectRoot)
		}
		return nil, nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []sbLocalMigration
	for _, e := range entries {
		m := sbMigrationFile.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			skipped = append(skipped, e.Name())
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, nil, err
		}
		if !fi.Mode().IsRegular() {
			skipped = append(skipped, e.Name())
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
	return out, skipped, nil
}

// sbMatchMigrations pairs local files with history entries one-to-one. Pass
// one matches on version — the CLI's record — and uses the entry up. Pass two
// matches the entries still free by name: the file stem, or the part after
// the timestamp, which is how the API records a migration (the server
// assigns the version; Lovable's newer entries carry only the name part).
// One-to-one matters: with the CLI's history a repeated name (every
// `supabase db pull` writes <ts>_remote_schema.sql) would otherwise mark a
// new file as applied, and it would never reach the database.
func sbMatchMigrations(local []sbLocalMigration, history []sbRemoteMigration) (applied, pending []sbLocalMigration, matches map[string]sbRemoteMigration) {
	matches = map[string]sbRemoteMigration{}
	used := make([]bool, len(history))
	matched := make([]bool, len(local))
	for i, l := range local {
		for j, r := range history {
			if !used[j] && r.Version == l.Version {
				used[j], matched[i] = true, true
				matches[l.Stem] = r
				break
			}
		}
	}
	for i, l := range local {
		if matched[i] {
			continue
		}
		for j, r := range history {
			if used[j] || r.Name == "" {
				continue
			}
			if r.Name == l.Stem || r.Name == l.Name {
				used[j], matched[i] = true, true
				matches[l.Stem] = r
				break
			}
		}
	}
	for i, l := range local {
		if matched[i] {
			applied = append(applied, l)
		} else {
			pending = append(pending, l)
		}
	}
	return applied, pending, matches
}

// sbOlderThanApplied lists the pending files whose version lies before the
// newest applied one — out of order, which the CLI refuses without
// --include-all. Reported, not refused: the caller decides.
func sbOlderThanApplied(applied, pending []sbLocalMigration) []string {
	newest := ""
	for _, a := range applied {
		if a.Version > newest {
			newest = a.Version
		}
	}
	var out []string
	for _, p := range pending {
		if newest != "" && p.Version < newest {
			out = append(out, p.Stem)
		}
	}
	return out
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
	local, skipped, err := sbLocalMigrations(root)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	history, err := s.listMigrations(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	applied, pending, matches := sbMatchMigrations(local, history)
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
	result := map[string]any{
		"project":       s.projectRef,
		"migrationsDir": filepath.Join(root, filepath.FromSlash(sbMigrationsDir)),
		"localCount":    len(local),
		"historyCount":  len(history),
		"pendingCount":  len(pending),
		"pending":       pending,
		"applied":       ap,
		"historyOnly":   unmatched,
		"matchingRule":  "a file counts as applied when one history entry has its version, else its file stem or its name part (the part after the timestamp); every entry matches at most one file",
	}
	if older := sbOlderThanApplied(applied, pending); len(older) > 0 {
		result["olderThanLatestApplied"] = older
	}
	if len(skipped) > 0 {
		result["skipped"] = skipped
	}
	return sbResultJSON(result)
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
	local, _, err := sbLocalMigrations(root)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	history, err := s.listMigrations(ctx)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	applied0, pending, matches := sbMatchMigrations(local, history)

	var todo []sbLocalMigration
	if only != "" {
		var candidates []sbLocalMigration
		for _, l := range local {
			if l.Stem == only || l.Version == only {
				candidates = append(candidates, l)
			}
		}
		switch len(candidates) {
		case 0:
			return mcp.NewToolResultError(fmt.Sprintf("file %q is not in %s (name it as <version>_<name> or by its version)", only, sbMigrationsDir)), nil
		case 1:
		default:
			names := make([]string, 0, len(candidates))
			for _, c := range candidates {
				names = append(names, c.Stem)
			}
			return mcp.NewToolResultError(fmt.Sprintf("file %q names %d migrations (%s) — pass the full file name", only, len(candidates), strings.Join(names, ", "))), nil
		}
		l := candidates[0]
		if r, ok := matches[l.Stem]; ok {
			return mcp.NewToolResultError(fmt.Sprintf("%s.sql is already in the history (version %s, name %q) — nothing to apply", l.Stem, r.Version, r.Name)), nil
		}
		todo = []sbLocalMigration{l}
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
		if l.Size > sbMigrationMax {
			return mcp.NewToolResultError(fmt.Sprintf("%s.sql is %d bytes, more than the %d MB a migration may have here", l.Stem, l.Size, sbMigrationMax>>20)), nil
		}
		data, err := os.ReadFile(l.Path)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("read %s: %v", l.Path, err)), nil
		}
		if len(strings.TrimSpace(sbStripSQLComments(string(data)))) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf("%s.sql holds no SQL (empty, or only comments) — the API refuses an empty query", l.Stem)), nil
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
	if older := sbOlderThanApplied(applied0, todo); len(older) > 0 {
		result["olderThanLatestApplied"] = older
		result["orderWarning"] = "these files are older than the newest applied migration — the CLI would refuse them without --include-all; applying them is your decision"
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
	// versionsFromHistory fills in the versions the server assigned.
	versionsFromHistory := func() {
		after, err := s.listMigrations(ctx)
		if err != nil {
			return
		}
		for i := range applied {
			for _, r := range after {
				if r.Name == applied[i].File {
					applied[i].Version = r.Version
				}
			}
		}
	}
	for i, l := range todo {
		body := map[string]any{"query": string(contents[i]), "name": l.Stem}
		// The idempotency key names this migration by file and content, so the
		// 429 retry inside the call repeats the same request. What the server
		// does with the key beyond that is not documented.
		key := l.Stem + "-" + plan[i].SHA256[:32]
		_, err := s.callJSONHeadersWith(ctx, s.longClient, "migrations.apply", nil, nil, map[string]string{"Idempotency-Key": key}, body, nil)
		if err == nil {
			applied = append(applied, appliedOut{File: l.Stem})
			continue
		}
		remaining := make([]string, 0)
		for _, r := range todo[i+1:] {
			remaining = append(remaining, r.Stem)
		}
		result["remaining"] = remaining
		failed := map[string]any{"file": l.Stem, "error": err.Error()}
		if sbAnswered(err) {
			// The server said no: the SQL did not run (the API applies a
			// migration as one transaction).
			failed["state"] = "not applied"
			result["note"] = "stopped at the first error; the migrations before it are applied and recorded, this one is not — fix the file and run again"
		} else {
			// No answer: a timeout or a dead connection after the request
			// went out. The server may still be running the SQL, or may have
			// finished it. The history decides, not a retry.
			failed["state"] = "unknown"
			if after, lerr := s.listMigrations(ctx); lerr == nil {
				for _, r := range after {
					if r.Name == l.Stem {
						failed["state"] = "applied — the history has it (version " + r.Version + ") although the request got no answer"
					}
				}
			}
			result["note"] = "the request got no answer; whether the migration ran is unknown — check migrations_list (or wait and check again if it is a long one) before running anything else on this project"
		}
		result["failed"] = failed
		versionsFromHistory()
		result["applied"] = applied
		result["appliedCount"] = len(applied)
		return sbResultJSON(result)
	}
	versionsFromHistory()
	result["applied"] = applied
	result["appliedCount"] = len(applied)
	return sbResultJSON(result)
}

// sbStripSQLComments removes -- line comments and /* */ blocks, enough to
// tell a file with no statements from one with some.
func sbStripSQLComments(sql string) string {
	var b strings.Builder
	for i := 0; i < len(sql); i++ {
		switch {
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}
