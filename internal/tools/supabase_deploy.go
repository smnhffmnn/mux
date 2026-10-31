package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// Deploying an Edge Function through the Management API: the request is
// multipart/form-data with a metadata part and one file part per source
// file; the server builds the bundle and resolves npm:, jsr: and https:
// imports itself. Which local files make up a function is the supabase CLI's
// decision (apps/cli/src/shared/functions/deploy.ts, collectSourceDeployFiles,
// read 31.10.2026), reproduced here so both tools upload the same set:
//
//   - the import map, when the function has one: config.toml's import_map,
//     else deno.json / deno.jsonc next to the entrypoint, else
//     supabase/functions/import_map.json (an import_map.json next to the
//     entrypoint is an error, as in the CLI);
//   - the files matching config.toml's static_files patterns;
//   - the import graph from the entrypoint — import/export … from "…" and
//     import("…") with a relative or absolute specifier whose last segment
//     has a dot, resolved through the import map first, files outside the
//     source root skipped with a warning;
//   - the local targets of the import map (files, or directories in full).
//
// Names are anchored at the project root (the directory holding supabase/),
// with forward slashes: supabase/functions/<slug>/index.ts. The source root
// that bounds the walk is the nearest git root above the project root, or
// the project root itself; a file from above the project root would need a
// ".." in its name and is refused, as the CLI does.

const (
	sbFunctionsDir = "supabase/functions"
	sbConfigFile   = "supabase/config.toml"
)

// sbImportPattern is the CLI's importPathPattern. Group 1: static import or
// export specifier; groups 2 and 3: dynamic import("…") / import('…').
var sbImportPattern = regexp.MustCompile(`(?i)(?:import|export)\s+(?:type\s+)?(?:{[^{}]+}|.*?)\s*(?:from)?\s*['"](.*?)['"]|import\(\s*(?:"([^"\r\n]*)"|'([^'\r\n]*)')\s*[,)]`)

// sbFunctionConfig is the [functions.<slug>] table of supabase/config.toml,
// paths already resolved to absolute ones.
type sbFunctionConfig struct {
	Enabled     bool
	VerifyJWT   *bool // nil when config.toml does not say
	Entrypoint  string
	ImportMap   string // "" when none
	StaticFiles []string
}

// sbDeployFile is one part of the upload.
type sbDeployFile struct {
	Path string `json:"-"`    // absolute, cleaned
	Name string `json:"path"` // relative to the project root, forward slashes
	Size int64  `json:"size"`
}

// sbDeployPlan is everything the deploy sends, computed before the first
// byte goes out — and what dry_run shows.
type sbDeployPlan struct {
	Slug        string
	ProjectRoot string
	SourceRoot  string
	Config      sbFunctionConfig
	Files       []sbDeployFile
	Metadata    map[string]any
	Warnings    []string
	TotalBytes  int64
}

// sbImportMap is a Deno import map: imports and scopes with their targets
// resolved against the file that declared them.
type sbImportMap struct {
	Imports   map[string]string
	Scopes    map[string]map[string]string
	Reference string // deno.json "importMap": a path to the real map
}

func (m *sbImportMap) isReference() bool {
	return m != nil && len(m.Imports) == 0 && len(m.Scopes) == 0 && m.Reference != ""
}

// --- Resolving the function ---

// sbProjectRoot checks the path argument: absolute, a directory, with a
// supabase/functions directory under it.
func sbProjectRoot(raw string) (string, error) {
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
	fdir := filepath.Join(clean, filepath.FromSlash(sbFunctionsDir))
	if fi, err := os.Stat(fdir); err != nil || !fi.IsDir() {
		hint := ""
		if filepath.Base(clean) == "supabase" || filepath.Base(clean) == "functions" {
			hint = " — pass the project root, not the supabase/ or functions/ directory"
		}
		return "", fmt.Errorf("path has no %s directory: %s%s", sbFunctionsDir, clean, hint)
	}
	root, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("path is not usable: %w", err)
	}
	return root, nil
}

// sbGitRoot walks up from dir to the nearest directory with a .git entry.
func sbGitRoot(dir string) (string, bool) {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

// sbReadFunctionConfig reads supabase/config.toml for one function. A missing
// file or table yields the defaults. Relative paths are relative to the
// supabase/ directory, as the CLI resolves them.
func sbReadFunctionConfig(projectRoot, slug string) (sbFunctionConfig, error) {
	supabaseDir := filepath.Join(projectRoot, "supabase")
	cfg := sbFunctionConfig{
		Enabled:    true,
		Entrypoint: filepath.Join(projectRoot, filepath.FromSlash(sbFunctionsDir), slug, "index.ts"),
	}
	data, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(sbConfigFile)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read %s: %w", sbConfigFile, err)
	}
	var doc map[string]any
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", sbConfigFile, err)
	}
	functions, _ := doc["functions"].(map[string]any)
	table, _ := functions[slug].(map[string]any)
	if table == nil {
		return cfg, nil
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Clean(filepath.Join(supabaseDir, filepath.FromSlash(p)))
	}
	if v, ok := table["enabled"]; ok {
		b, ok := v.(bool)
		if !ok {
			return cfg, fmt.Errorf("%s: functions.%s.enabled must be a boolean", sbConfigFile, slug)
		}
		cfg.Enabled = b
	}
	if v, ok := table["verify_jwt"]; ok {
		b, ok := v.(bool)
		if !ok {
			return cfg, fmt.Errorf("%s: functions.%s.verify_jwt must be a boolean", sbConfigFile, slug)
		}
		cfg.VerifyJWT = &b
	}
	if v, ok := table["entrypoint"]; ok {
		s, ok := v.(string)
		if !ok {
			return cfg, fmt.Errorf("%s: functions.%s.entrypoint must be a string", sbConfigFile, slug)
		}
		if s != "" {
			cfg.Entrypoint = resolve(s)
		}
	}
	if v, ok := table["import_map"]; ok {
		s, ok := v.(string)
		if !ok {
			return cfg, fmt.Errorf("%s: functions.%s.import_map must be a string", sbConfigFile, slug)
		}
		if s != "" {
			cfg.ImportMap = resolve(s)
		}
	}
	if v, ok := table["static_files"]; ok {
		list, ok := v.([]any)
		if !ok {
			return cfg, fmt.Errorf("%s: functions.%s.static_files must be an array of strings", sbConfigFile, slug)
		}
		for _, e := range list {
			s, ok := e.(string)
			if !ok {
				return cfg, fmt.Errorf("%s: functions.%s.static_files must be an array of strings", sbConfigFile, slug)
			}
			cfg.StaticFiles = append(cfg.StaticFiles, resolve(s))
		}
	}
	return cfg, nil
}

// sbResolveImportMap applies the CLI's search order when config.toml names
// no import map: deno.json, deno.jsonc next to the entrypoint; an
// import_map.json there is refused; then supabase/functions/import_map.json.
func sbResolveImportMap(projectRoot string, cfg *sbFunctionConfig) error {
	if cfg.ImportMap != "" {
		if !sbIsFile(cfg.ImportMap) {
			return fmt.Errorf("import_map from %s not found: %s", sbConfigFile, cfg.ImportMap)
		}
		return nil
	}
	funcDir := filepath.Dir(cfg.Entrypoint)
	for _, name := range []string{"deno.json", "deno.jsonc"} {
		if p := filepath.Join(funcDir, name); sbIsFile(p) {
			cfg.ImportMap = p
			return nil
		}
	}
	fallback := filepath.Join(projectRoot, filepath.FromSlash(sbFunctionsDir), "import_map.json")
	if local := filepath.Join(funcDir, "import_map.json"); local != fallback && sbIsFile(local) {
		return fmt.Errorf("%s next to the entrypoint is not supported — rename it to deno.json or set import_map in %s (the supabase CLI refuses it too)", local, sbConfigFile)
	}
	if sbIsFile(fallback) {
		cfg.ImportMap = fallback
	}
	return nil
}

func sbIsFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// --- Import maps ---

// sbStripJSONComments removes // and /* */ comments and trailing commas,
// enough for deno.jsonc. Strings are left alone.
func sbStripJSONComments(s string) string {
	var out strings.Builder
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr:
			out.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				out.WriteByte(s[i])
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				out.WriteByte('\n')
			}
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
		default:
			out.WriteByte(c)
		}
	}
	// Trailing commas before } or ].
	re := regexp.MustCompile(`,(\s*[}\]])`)
	return re.ReplaceAllString(out.String(), "$1")
}

// sbParseImportMap reads an import map (or deno.json) and resolves its
// targets against the file's directory.
func sbParseImportMap(file string, data []byte) (*sbImportMap, error) {
	var raw struct {
		ImportMap any `json:"importMap"`
		Imports   any `json:"imports"`
		Scopes    any `json:"scopes"`
	}
	if err := json.Unmarshal([]byte(sbStripJSONComments(string(data))), &raw); err != nil {
		return nil, fmt.Errorf("parse import map %s: %w", file, err)
	}
	m := &sbImportMap{Imports: map[string]string{}, Scopes: map[string]map[string]string{}}
	if s, ok := raw.ImportMap.(string); ok {
		m.Reference = s
	}
	readMap := func(v any, what string) (map[string]string, error) {
		if v == nil {
			return map[string]string{}, nil
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("parse import map %s: %s must be an object", file, what)
		}
		out := make(map[string]string, len(obj))
		for k, val := range obj {
			s, ok := val.(string)
			if !ok {
				return nil, fmt.Errorf("parse import map %s: %s.%s must be a string", file, what, k)
			}
			out[k] = s
		}
		return out, nil
	}
	imports, err := readMap(raw.Imports, "imports")
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(file)
	for k, v := range imports {
		m.Imports[k] = sbResolveImportTarget(dir, v)
	}
	if raw.Scopes != nil {
		scopes, ok := raw.Scopes.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("parse import map %s: scopes must be an object", file)
		}
		for scope, val := range scopes {
			entries, err := readMap(val, "scopes."+scope)
			if err != nil {
				return nil, err
			}
			resolved := make(map[string]string, len(entries))
			for k, v := range entries {
				resolved[k] = sbResolveImportTarget(dir, v)
			}
			m.Scopes[sbResolveImportTarget(dir, scope)] = resolved
		}
	}
	return m, nil
}

// sbIsRemote reports whether a specifier or target has a URL scheme
// (https:, npm:, jsr:, node:, …).
func sbIsRemote(target string) bool {
	if strings.HasPrefix(target, "/") {
		return false
	}
	u, err := url.Parse(target)
	return err == nil && u.Scheme != ""
}

// sbResolveImportTarget makes a relative import-map target absolute against
// the map's directory; remote and absolute targets stay as they are. A
// trailing slash (a prefix mapping) survives.
func sbResolveImportTarget(dir, target string) string {
	if strings.HasPrefix(target, "/") || sbIsRemote(target) {
		return target
	}
	resolved := filepath.ToSlash(filepath.Join(dir, filepath.FromSlash(target)))
	if strings.HasSuffix(target, "/") && !strings.HasSuffix(resolved, "/") {
		resolved += "/"
	}
	return resolved
}

// sbLoadImportMap reads the map file and follows a deno.json that only
// references another map. onRead sees every file read, so both are uploaded.
func sbLoadImportMap(file string, onRead func(string) error) (*sbImportMap, error) {
	seen := map[string]bool{}
	current := file
	for {
		abs, _ := filepath.Abs(current)
		if seen[abs] {
			return nil, fmt.Errorf("cyclic import map reference: %s", current)
		}
		seen[abs] = true
		data, err := os.ReadFile(current)
		if err != nil {
			return nil, fmt.Errorf("read import map: %w", err)
		}
		if onRead != nil {
			if err := onRead(current); err != nil {
				return nil, err
			}
		}
		m, err := sbParseImportMap(current, data)
		if err != nil {
			return nil, err
		}
		base := strings.ToLower(filepath.Base(current))
		if !((base == "deno.json" || base == "deno.jsonc") && m.isReference()) {
			return m, nil
		}
		current = filepath.Join(filepath.Dir(current), filepath.FromSlash(m.Reference))
	}
}

// sbSubstitute applies import-map matching: a key matches exactly, or as a
// prefix when it ends with "/" (and its value does too); the longest key wins.
func sbSubstitute(mappings map[string]string, specifier string) (string, bool) {
	best := ""
	bestVal := ""
	for prefix, value := range mappings {
		if prefix == "" {
			continue
		}
		if strings.HasSuffix(prefix, "/") {
			if !strings.HasSuffix(value, "/") || !strings.HasPrefix(specifier, prefix) {
				continue
			}
		} else if specifier != prefix {
			continue
		}
		if len(prefix) > len(best) {
			best, bestVal = prefix, value
		}
	}
	if best == "" {
		return "", false
	}
	return bestVal + specifier[len(best):], true
}

// sbResolveSpecifier resolves an import specifier of the file at current
// through the map's scopes, then its imports.
func (m *sbImportMap) resolveSpecifier(current, specifier string) (string, bool) {
	if m == nil {
		return specifier, false
	}
	var scoped map[string]string
	bestLen := -1
	for scope, mappings := range m.Scopes {
		matches := scope == current || (strings.HasSuffix(scope, "/") && strings.HasPrefix(current, scope))
		if matches && len(scope) > bestLen {
			scoped, bestLen = mappings, len(scope)
		}
	}
	if scoped != nil {
		if r, ok := sbSubstitute(scoped, specifier); ok {
			return r, true
		}
	}
	if r, ok := sbSubstitute(m.Imports, specifier); ok {
		return r, true
	}
	return specifier, false
}

// --- Collecting files ---

type sbCollector struct {
	plan         *sbDeployPlan
	workdir      string   // names are relative to this
	sourceRoot   string   // import walk boundary (real path)
	allowedRoots []string // import map and its targets may also live here
	seen         map[string]bool
}

func sbContained(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func sbContainedInAny(roots []string, candidate string) bool {
	for _, r := range roots {
		if sbContained(r, candidate) {
			return true
		}
	}
	return false
}

func sbRealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func (c *sbCollector) warn(format string, args ...any) {
	c.plan.Warnings = append(c.plan.Warnings, fmt.Sprintf(format, args...))
}

// add registers one file under its workdir-relative name. A name that would
// need ".." is an error, as in the CLI: the server anchors every path at the
// project root.
func (c *sbCollector) add(p string) error {
	real := sbRealPath(p)
	if c.seen[real] {
		return nil
	}
	fi, err := os.Stat(real)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}
	if fi.IsDir() {
		return fmt.Errorf("file path is a directory: %s", p)
	}
	rel, err := filepath.Rel(c.workdir, filepath.Clean(p))
	if err != nil {
		return err
	}
	name := filepath.ToSlash(rel)
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return fmt.Errorf("%s lies outside the project root %s — the API anchors file names at the project root and cannot carry it (the supabase CLI refuses it as well)", p, c.workdir)
		}
	}
	c.seen[real] = true
	c.plan.Files = append(c.plan.Files, sbDeployFile{Path: filepath.Clean(p), Name: name, Size: fi.Size()})
	c.plan.TotalBytes += fi.Size()
	return nil
}

// walkImports follows relative imports from src, breadth first, within
// allowedRoots. Specifiers go through the import map first; what still is
// not a relative or absolute path (npm:, jsr:, https:, bare) is the
// server's business.
func (c *sbCollector) walkImports(m *sbImportMap, src string, allowedRoots []string) error {
	queue := []string{filepath.Clean(src)}
	visited := map[string]bool{}
	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if visited[current] {
			continue
		}
		visited[current] = true

		real := sbRealPath(current)
		if !sbContainedInAny(allowedRoots, real) {
			c.warn("skipping import path outside source root: %s", current)
			continue
		}
		data, err := os.ReadFile(real)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				c.warn("failed to read file: open %s: no such file or directory", c.display(current))
				continue
			}
			var pe *fs.PathError
			if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "not a directory") {
				return fmt.Errorf("failed to read file: open %s: not a directory", c.display(current))
			}
			return fmt.Errorf("failed to read file: %w", err)
		}
		if err := c.add(current); err != nil {
			return err
		}
		currentSlash := filepath.ToSlash(current)
		for _, match := range sbImportPattern.FindAllStringSubmatch(string(data), -1) {
			raw := match[1]
			if raw == "" {
				raw = match[2]
			}
			if raw == "" {
				raw = match[3]
			}
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			modulePath, substituted := m.resolveSpecifier(currentSlash, raw)
			modulePath = filepath.ToSlash(modulePath)
			// A module file needs a dot in its final segment; a trailing
			// slash or a dotless segment is a directory-shaped specifier.
			final := modulePath[strings.LastIndex(modulePath, "/")+1:]
			if !strings.Contains(final, ".") {
				continue
			}
			if !strings.HasPrefix(modulePath, "./") && !strings.HasPrefix(modulePath, "../") && !strings.HasPrefix(modulePath, "/") && !sbWindowsAbs(modulePath) {
				continue
			}
			if !substituted && (strings.HasPrefix(modulePath, "./") || strings.HasPrefix(modulePath, "../")) {
				modulePath = filepath.ToSlash(filepath.Join(filepath.Dir(current), filepath.FromSlash(modulePath)))
			}
			resolved := filepath.Clean(filepath.FromSlash(modulePath))
			if !filepath.IsAbs(resolved) {
				resolved, _ = filepath.Abs(resolved)
			}
			if !sbContainedInAny(allowedRoots, sbRealPath(resolved)) {
				c.warn("skipping import path outside source root: %s", modulePath)
				continue
			}
			queue = append(queue, resolved)
		}
	}
	return nil
}

func sbWindowsAbs(p string) bool {
	return len(p) > 2 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && p[2] == '/'
}

func (c *sbCollector) display(p string) string {
	rel, err := filepath.Rel(c.workdir, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// collectImportMapTargets uploads the local targets of the map: a file (and
// its own imports), or a directory in full.
func (c *sbCollector) collectImportMapTargets(m *sbImportMap) error {
	var targets []string
	for _, t := range m.Imports {
		if !sbIsRemote(t) {
			targets = append(targets, t)
		}
	}
	for _, scope := range m.Scopes {
		for _, t := range scope {
			if !sbIsRemote(t) {
				targets = append(targets, t)
			}
		}
	}
	sort.Strings(targets)
	for _, t := range targets {
		p := filepath.Clean(filepath.FromSlash(t))
		fi, err := os.Stat(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				c.warn("skipping import map target that does not exist: %s", t)
				continue
			}
			var pe *fs.PathError
			if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "not a directory") {
				c.warn("skipping import map target that is not a directory: %s", t)
				continue
			}
			return err
		}
		real := sbRealPath(p)
		if !sbContainedInAny(c.allowedRoots, real) {
			c.warn("skipping import path outside source root: %s", t)
			continue
		}
		if !fi.IsDir() {
			if err := c.add(p); err != nil {
				return err
			}
			if err := c.walkImports(m, p, c.allowedRoots); err != nil {
				return err
			}
			continue
		}
		err = filepath.WalkDir(p, func(sub string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if sub != p && d.Type()&fs.ModeSymlink != 0 {
					return filepath.SkipDir
				}
				return nil
			}
			if !sbContainedInAny(c.allowedRoots, sbRealPath(sub)) {
				c.warn("skipping import path outside source root: %s", sub)
				return nil
			}
			return c.add(sub)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// sbExpandStatic expands one static_files entry: a plain path must exist; a
// pattern with * ? [ ] (and ** for any depth) is matched under its glob-free
// prefix.
func sbExpandStatic(pattern string) ([]string, error) {
	if !strings.ContainsAny(pattern, "*?[") {
		if _, err := os.Stat(pattern); err != nil {
			return nil, fmt.Errorf("no files matched pattern: %s", pattern)
		}
		return []string{pattern}, nil
	}
	slash := filepath.ToSlash(pattern)
	base := slash
	if i := strings.IndexAny(slash, "*?["); i >= 0 {
		base = slash[:i]
		if j := strings.LastIndex(base, "/"); j >= 0 {
			base = base[:j]
		} else {
			base = "."
		}
	}
	re, err := sbGlobRegexp(slash)
	if err != nil {
		return nil, err
	}
	var matches []string
	err = filepath.WalkDir(filepath.FromSlash(base), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if re.MatchString(filepath.ToSlash(p)) {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no files matched pattern: %s", pattern)
		}
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no files matched pattern: %s", pattern)
	}
	sort.Strings(matches)
	return matches, nil
}

// sbGlobRegexp converts a glob to a regular expression over slash paths:
// ** any depth, * within a segment, ? one character, [...] a class.
func sbGlobRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(glob[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("invalid static_files pattern %q: unclosed [", glob)
			}
			class := glob[i+1 : i+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// sbPlanDeploy computes the plan for one function: config, import map,
// files and metadata. remoteVerifyJWT is the deployed function's setting,
// used when config.toml and the call do not say — the CLI keeps it as well.
func sbPlanDeploy(projectRoot, slug string, verifyOverride *bool, remoteVerifyJWT *bool) (*sbDeployPlan, error) {
	cfg, err := sbReadFunctionConfig(projectRoot, slug)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("function %s is disabled in %s (enabled = false)", slug, sbConfigFile)
	}
	if !sbIsFile(cfg.Entrypoint) {
		return nil, fmt.Errorf("entrypoint not found: %s (a function lives in %s/%s/index.ts unless %s says otherwise)", cfg.Entrypoint, sbFunctionsDir, slug, sbConfigFile)
	}
	if err := sbResolveImportMap(projectRoot, &cfg); err != nil {
		return nil, err
	}
	sourceRoot := projectRoot
	if g, ok := sbGitRoot(projectRoot); ok {
		sourceRoot = g
	}
	plan := &sbDeployPlan{Slug: slug, ProjectRoot: projectRoot, SourceRoot: sourceRoot, Config: cfg}
	realSource := sbRealPath(sourceRoot)
	c := &sbCollector{plan: plan, workdir: projectRoot, sourceRoot: realSource, allowedRoots: []string{realSource}, seen: map[string]bool{}}

	// Roots the import map may reach beyond the source root: its own
	// directory, and the directory of a map it references.
	var importMap *sbImportMap
	if cfg.ImportMap != "" {
		realMap := sbRealPath(cfg.ImportMap)
		if !sbContained(realSource, realMap) {
			c.allowedRoots = append(c.allowedRoots, filepath.Dir(realMap))
		}
		if data, err := os.ReadFile(cfg.ImportMap); err == nil {
			if m, err := sbParseImportMap(cfg.ImportMap, data); err == nil && m.Reference != "" {
				ref := sbRealPath(filepath.Join(filepath.Dir(cfg.ImportMap), filepath.FromSlash(m.Reference)))
				if !sbContained(realSource, ref) {
					c.allowedRoots = append(c.allowedRoots, filepath.Dir(ref))
				}
			}
		}
		importMap, err = sbLoadImportMap(cfg.ImportMap, func(file string) error {
			if !sbContainedInAny(c.allowedRoots, sbRealPath(file)) {
				return fmt.Errorf("refusing to upload import map outside allowed roots: %s", file)
			}
			return c.add(file)
		})
		if err != nil {
			return nil, err
		}
	}

	for _, pattern := range cfg.StaticFiles {
		matches, err := sbExpandStatic(pattern)
		if err != nil {
			c.warn("%v", err)
			continue
		}
		for _, p := range matches {
			if !sbContained(realSource, sbRealPath(p)) {
				return nil, fmt.Errorf("refusing to upload asset outside source root: %s", p)
			}
			if err := c.add(p); err != nil {
				return nil, err
			}
		}
	}

	if err := c.walkImports(importMap, cfg.Entrypoint, []string{realSource}); err != nil {
		return nil, err
	}
	if importMap != nil {
		if err := c.collectImportMapTargets(importMap); err != nil {
			return nil, err
		}
	}

	rel := func(p string) string {
		r, err := filepath.Rel(projectRoot, p)
		if err != nil {
			return filepath.ToSlash(p)
		}
		return filepath.ToSlash(r)
	}
	meta := map[string]any{
		"name":            slug,
		"entrypoint_path": rel(cfg.Entrypoint),
		"import_map_path": "",
		"static_patterns": []string{},
	}
	if cfg.ImportMap != "" {
		meta["import_map_path"] = rel(cfg.ImportMap)
	}
	if len(cfg.StaticFiles) > 0 {
		patterns := make([]string, 0, len(cfg.StaticFiles))
		for _, p := range cfg.StaticFiles {
			patterns = append(patterns, rel(p))
		}
		meta["static_patterns"] = patterns
	}
	switch {
	case verifyOverride != nil:
		meta["verify_jwt"] = *verifyOverride
	case cfg.VerifyJWT != nil:
		meta["verify_jwt"] = *cfg.VerifyJWT
	case remoteVerifyJWT != nil:
		meta["verify_jwt"] = *remoteVerifyJWT
	}
	plan.Metadata = meta
	return plan, nil
}

// --- Upload ---

// sbDeployBody renders the multipart body: the metadata part first, then one
// "file" part per source file with its workdir-relative name, streamed from
// disk at send time with an exact Content-Length.
func sbDeployBody(plan *sbDeployPlan) (*sbBody, error) {
	metaJSON, err := json.Marshal(plan.Metadata)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	boundary := multipart.NewWriter(io.Discard).Boundary()
	var head bytes.Buffer
	fmt.Fprintf(&head, "--%s\r\nContent-Disposition: form-data; name=\"metadata\"\r\n\r\n%s\r\n", boundary, metaJSON)
	heads := make([][]byte, len(plan.Files))
	length := int64(head.Len())
	for i, f := range plan.Files {
		heads[i] = []byte(fmt.Sprintf("--%s\r\nContent-Disposition: %s\r\nContent-Type: application/octet-stream\r\n\r\n",
			boundary, partContentDisposition("file", f.Name)))
		length += int64(len(heads[i])) + f.Size + 2
	}
	tail := []byte("--" + boundary + "--\r\n")
	length += int64(len(tail))
	open := func() (io.ReadCloser, error) {
		readers := make([]io.Reader, 0, len(plan.Files)*3+2)
		var closers []io.Closer
		readers = append(readers, bytes.NewReader(head.Bytes()))
		for i, f := range plan.Files {
			fh, err := os.Open(f.Path)
			if err != nil {
				for _, c := range closers {
					c.Close()
				}
				return nil, fmt.Errorf("open %s: %w", f.Path, err)
			}
			closers = append(closers, fh)
			readers = append(readers, bytes.NewReader(heads[i]), io.LimitReader(fh, f.Size), strings.NewReader("\r\n"))
		}
		readers = append(readers, bytes.NewReader(tail))
		return &ytMultiCloser{Reader: io.MultiReader(readers...), closers: closers}, nil
	}
	return &sbBody{open: open, length: length, contentType: "multipart/form-data; boundary=" + boundary}, nil
}

func (s *Supabase) handleFunctionDeploy(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	slug, err := sbSlugArg(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	rawPath, err := stringArg(req, "path")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	projectRoot, err := sbProjectRoot(rawPath)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	args := req.GetArguments()
	var verifyOverride *bool
	if v, ok := args["verify_jwt"]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("verify_jwt must be a boolean, got %T", v)), nil
		}
		verifyOverride = &b
	}
	dryRun := req.GetBool("dry_run", false)

	// What is deployed now: its verify_jwt is kept when neither config.toml
	// nor the call says otherwise, and its version tells the caller what
	// changed.
	before, err := s.getFunction(ctx, slug)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read the deployed function: %v", err)), nil
	}
	var remoteVerify *bool
	if before != nil {
		remoteVerify = before.VerifyJWT
	}
	plan, err := sbPlanDeploy(projectRoot, slug, verifyOverride, remoteVerify)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	result := map[string]any{
		"project":     s.projectRef,
		"slug":        slug,
		"projectRoot": projectRoot,
		"sourceRoot":  plan.SourceRoot,
		"metadata":    plan.Metadata,
		"files":       plan.Files,
		"fileCount":   len(plan.Files),
		"totalBytes":  plan.TotalBytes,
	}
	if len(plan.Warnings) > 0 {
		result["warnings"] = plan.Warnings
	}
	if before != nil {
		result["before"] = renderSBFunction(*before)
	} else {
		result["before"] = nil
		result["note"] = "the function is not deployed yet — the deploy creates it"
	}
	if dryRun {
		result["dryRun"] = true
		result["deployed"] = false
		return sbResultJSON(result)
	}

	body, err := sbDeployBody(plan)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	client := *s.client
	client.Timeout = uploadTimeout
	q := url.Values{}
	q.Set("slug", slug)
	resp, err := s.call(ctx, "function.deploy", nil, q, body, "", &client)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("deploy failed: %v", err)), nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, sbMaxJSONBody))
	if resp.StatusCode != http.StatusCreated {
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return mcp.NewToolResultError(fmt.Sprintf("unexpected deploy status %d (the API answers 201 on success): %s", resp.StatusCode, strings.TrimSpace(string(data)))), nil
		}
		return mcp.NewToolResultError("deploy failed: " + sbErrorMessage(resp.StatusCode, data).Error()), nil
	}
	var deployed sbRawFunction
	if err := json.Unmarshal(data, &deployed); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("deployed (HTTP 201), but the answer did not decode: %v — read it back with function_get", err)), nil
	}
	result["deployed"] = true
	result["function"] = renderSBFunction(deployed)
	if before != nil && deployed.Version <= before.Version {
		result["warning"] = fmt.Sprintf("the version did not advance (%d before, %d after)", before.Version, deployed.Version)
	}
	return sbResultJSON(result)
}

// --- Reading a function back ---

// sbBodyFile is one file of a deployed function as the API hands it back.
type sbBodyFile struct {
	Path    string `json:"path"`
	Size    int    `json:"size"`
	content []byte
}

// sbFunctionBody reads the deployed source files. With Accept:
// multipart/form-data the API answers one part per file (the path in a
// Supabase-Path header or the Content-Disposition file name) plus a metadata
// part; a function deployed as a pre-built bundle comes back as one binary
// eszip instead.
func (s *Supabase) sbFunctionBody(ctx context.Context, slug string) (files []sbBodyFile, metadata map[string]any, eszip []byte, err error) {
	resp, err := s.call(ctx, "function.body", map[string]string{"function_slug": slug}, nil, nil, "multipart/form-data", nil)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, nil, nil, sbErrorMessage(resp.StatusCode, data)
	}
	mediaType, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	limited := io.LimitReader(resp.Body, sbMaxBundleBody+1)
	if !strings.HasPrefix(mediaType, "multipart/") || params["boundary"] == "" {
		data, err := io.ReadAll(limited)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read function body: %w", err)
		}
		if int64(len(data)) > sbMaxBundleBody {
			return nil, nil, nil, fmt.Errorf("function body larger than %d MB", sbMaxBundleBody>>20)
		}
		return nil, nil, data, nil
	}
	mr := multipart.NewReader(limited, params["boundary"])
	var total int64
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read function body: %w", err)
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read function body: %w", err)
		}
		total += int64(len(data))
		if total > sbMaxBundleBody {
			return nil, nil, nil, fmt.Errorf("function body larger than %d MB", sbMaxBundleBody>>20)
		}
		name := part.Header.Get("Supabase-Path")
		if name == "" {
			if _, p, err := mime.ParseMediaType(part.Header.Get("Content-Disposition")); err == nil {
				name = p["filename"]
			}
		}
		if name != "" {
			files = append(files, sbBodyFile{Path: name, Size: len(data), content: data})
			continue
		}
		if part.FormName() == "metadata" {
			var m map[string]any
			if json.Unmarshal(data, &m) == nil {
				metadata = m
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, metadata, nil, nil
}

func (s *Supabase) handleFunctionGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	slug, err := sbSlugArg(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	file, err := stringArg(req, "file")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	outputDir, err := stringArg(req, "output_dir")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if file != "" && outputDir != "" {
		return mcp.NewToolResultError("file and output_dir are alternatives: one file inline, or all files to disk"), nil
	}
	var outRoot string
	if outputDir != "" {
		outRoot = filepath.Clean(config.ExpandHome(outputDir))
		if !filepath.IsAbs(outRoot) {
			return mcp.NewToolResultError(fmt.Sprintf("output_dir must be an absolute path: %s", outputDir)), nil
		}
	}

	fn, err := s.getFunction(ctx, slug)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if fn == nil {
		return mcp.NewToolResultError(fmt.Sprintf("function %q is not deployed in project %s (functions_list shows what is)", slug, s.projectRef)), nil
	}
	files, metadata, eszip, err := s.sbFunctionBody(ctx, slug)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("read the function's files: %v", err)), nil
	}
	result := map[string]any{
		"project":  s.projectRef,
		"function": renderSBFunction(*fn),
	}
	if metadata != nil {
		result["metadata"] = metadata
	}
	if eszip != nil {
		result["bundle"] = map[string]any{"format": "eszip", "size": len(eszip), "note": "the function was deployed as a pre-built bundle; the API has no source files for it"}
		if outputDir != "" {
			dest := filepath.Join(outRoot, slug+".eszip")
			if err := sbWriteNew(dest, eszip); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			result["written"] = []string{dest}
		}
		return sbResultJSON(result)
	}
	list := make([]sbBodyFile, len(files))
	copy(list, files)
	result["files"] = list
	result["fileCount"] = len(files)

	if file != "" {
		want := strings.TrimPrefix(filepath.ToSlash(file), "./")
		var found *sbBodyFile
		for i := range files {
			if files[i].Path == want || strings.HasSuffix(files[i].Path, "/"+want) {
				if found != nil {
					return mcp.NewToolResultError(fmt.Sprintf("file %q matches more than one path — pass the full path from files", file)), nil
				}
				found = &files[i]
			}
		}
		if found == nil {
			return mcp.NewToolResultError(fmt.Sprintf("file %q is not among the function's files — see files in this result", file)), nil
		}
		if len(found.content) > sbInlineFileMax {
			return mcp.NewToolResultError(fmt.Sprintf("%s is %d bytes, more than the %d KB inlined — use output_dir", found.Path, len(found.content), sbInlineFileMax>>10)), nil
		}
		if bytes.IndexByte(found.content, 0) >= 0 {
			return mcp.NewToolResultError(fmt.Sprintf("%s is binary — use output_dir", found.Path)), nil
		}
		result["file"] = found.Path
		result["content"] = string(found.content)
		return sbResultJSON(result)
	}

	if outputDir != "" {
		var written []string
		for _, f := range files {
			rel := filepath.FromSlash(path.Clean("/" + f.Path))[1:]
			dest := filepath.Join(outRoot, rel)
			if !sbContained(outRoot, dest) {
				return mcp.NewToolResultError(fmt.Sprintf("refusing to write %s outside %s", f.Path, outRoot)), nil
			}
			if err := sbWriteNew(dest, f.content); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			written = append(written, dest)
		}
		result["written"] = written
		result["outputDir"] = outRoot
	}
	return sbResultJSON(result)
}

// sbWriteNew writes data to a file that must not exist yet, creating parent
// directories.
func sbWriteNew(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create directory: %v", err)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists (refusing to overwrite)", dest)
		}
		return fmt.Errorf("create %s: %v", dest, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(dest)
		return fmt.Errorf("write %s: %v", dest, err)
	}
	return f.Close()
}

// --- Env files ---

// sbReadEnvFile reads KEY=VALUE lines (dotenv form: comments, export
// prefix, single or double quotes) from an absolute path on this machine.
// The values never enter the tool call.
func sbReadEnvFile(raw string) (map[string]string, error) {
	clean := filepath.Clean(config.ExpandHome(raw))
	if !filepath.IsAbs(clean) {
		return nil, fmt.Errorf("env_file must be an absolute path: %s", raw)
	}
	f, err := os.Open(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("env_file not found: %s (resolved on the machine running mux)", clean)
		}
		return nil, fmt.Errorf("env_file is not readable: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		eq := strings.IndexByte(text, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("env_file line %d is not KEY=VALUE", line)
		}
		key := strings.TrimSpace(text[:eq])
		val := strings.TrimSpace(text[eq+1:])
		switch {
		case len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"':
			unq, err := sbUnquoteDouble(val)
			if err != nil {
				return nil, fmt.Errorf("env_file line %d: %v", line, err)
			}
			val = unq
		case len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'':
			val = val[1 : len(val)-1]
		default:
			if i := strings.Index(val, " #"); i >= 0 {
				val = strings.TrimSpace(val[:i])
			}
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read env_file: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("env_file %s holds no KEY=VALUE lines", clean)
	}
	return out, nil
}

// sbUnquoteDouble handles the escapes dotenv files use in double quotes.
func sbUnquoteDouble(s string) (string, error) {
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case '"', '\\', '$':
			b.WriteByte(s[i])
		default:
			return "", fmt.Errorf("unknown escape \\%c in a double-quoted value", s[i])
		}
	}
	return b.String(), nil
}

func sortStrings(s []string) { sort.Strings(s) }
