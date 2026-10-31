package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// writeTree creates files under root from a map of slash paths to contents.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func planNames(plan *sbDeployPlan) []string {
	names := make([]string, 0, len(plan.Files))
	for _, f := range plan.Files {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	return names
}

// A function with relative imports into _shared (several
// levels), remote and bare specifiers ignored, no import map.
func TestSupabase_PlanWalksRelativeImports(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/HEAD":            "ref: refs/heads/main\n",
		"supabase/config.toml": "project_id = \"abcdefghijklmnopqrst\"\n",
		"supabase/functions/hook/index.ts": `import { createClient } from "https://esm.sh/@supabase/supabase-js@2";
import { a } from "../_shared/a.ts";
export { b } from "../_shared/b.ts";
import type { T } from "../_shared/types.ts";
import npmThing from "npm:zod@3";
import bare from "bare-module";
const lazy = await import("./lazy.ts");
// import { commented } from "../_shared/commented.ts" — still a match, as in the CLI
import "../_shared/side-effect.ts";
`,
		"supabase/functions/hook/lazy.ts":           "export const x = 1; import '../_shared/deep/d.ts'",
		"supabase/functions/_shared/a.ts":           "import { d } from './deep/d.ts'; export const a = d;",
		"supabase/functions/_shared/b.ts":           "export const b = 2;",
		"supabase/functions/_shared/types.ts":       "export type T = string;",
		"supabase/functions/_shared/deep/d.ts":      "export const d = 3; import '../a.ts';", // cycle
		"supabase/functions/_shared/side-effect.ts": "console.log(1)",
		"supabase/functions/_shared/unused.ts":      "export const u = 0;",
	})
	plan, err := sbPlanDeploy(root, "hook", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"supabase/functions/_shared/a.ts",
		"supabase/functions/_shared/b.ts",
		"supabase/functions/_shared/deep/d.ts",
		"supabase/functions/_shared/side-effect.ts",
		"supabase/functions/_shared/types.ts",
		"supabase/functions/hook/index.ts",
		"supabase/functions/hook/lazy.ts",
	}
	if got := planNames(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v\nwant %v", got, want)
	}
	// The commented-out import names a file that does not exist: a warning,
	// not an error — the CLI behaves the same.
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "commented.ts") {
		t.Errorf("warnings = %v, want one about commented.ts", plan.Warnings)
	}
	if plan.Metadata["entrypoint_path"] != "supabase/functions/hook/index.ts" {
		t.Errorf("entrypoint_path = %v", plan.Metadata["entrypoint_path"])
	}
	if plan.Metadata["import_map_path"] != "" {
		t.Errorf("import_map_path = %v, want empty (no map)", plan.Metadata["import_map_path"])
	}
	if _, ok := plan.Metadata["verify_jwt"]; ok {
		t.Errorf("verify_jwt sent although neither config nor remote know it: %v", plan.Metadata["verify_jwt"])
	}
	if plan.SourceRoot != root {
		t.Errorf("source root = %s, want the git root %s", plan.SourceRoot, root)
	}
	if plan.TotalBytes <= 0 || plan.Files[0].Size <= 0 {
		t.Errorf("sizes not filled: %+v", plan.Files)
	}
}

// deno.json next to the entrypoint is the import map: uploaded, its local
// targets uploaded in full, its prefix mappings used to resolve imports,
// remote mappings ignored.
func TestSupabase_PlanImportMap(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"supabase/functions/fn/index.ts": `import { x } from "@lib/x.ts";
import { y } from "shared/y.ts";
import z from "zod";
`,
		"supabase/functions/fn/deno.jsonc": `{
  // comments and trailing commas are fine
  "imports": {
    "@lib/": "./lib/",
    "shared/": "../_shared/",
    "zod": "npm:zod@3",
  },
  "compilerOptions": { "jsx": "react-jsx", },
}`,
		"supabase/functions/fn/lib/x.ts":      "export const x = 1;",
		"supabase/functions/fn/lib/other.ts":  "export const o = 1;", // a directory target: uploaded in full
		"supabase/functions/_shared/y.ts":     "export const y = 2; import './w.ts';",
		"supabase/functions/_shared/w.ts":     "export const w = 2;",
		"supabase/functions/_shared/noise.ts": "export const n = 0;", // also under the prefix directory
	})
	plan, err := sbPlanDeploy(root, "fn", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"supabase/functions/_shared/noise.ts",
		"supabase/functions/_shared/w.ts",
		"supabase/functions/_shared/y.ts",
		"supabase/functions/fn/deno.jsonc",
		"supabase/functions/fn/index.ts",
		"supabase/functions/fn/lib/other.ts",
		"supabase/functions/fn/lib/x.ts",
	}
	if got := planNames(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v\nwant %v", got, want)
	}
	if plan.Metadata["import_map_path"] != "supabase/functions/fn/deno.jsonc" {
		t.Errorf("import_map_path = %v", plan.Metadata["import_map_path"])
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", plan.Warnings)
	}
}

// config.toml decides: entrypoint, import map, static files, verify_jwt,
// enabled — paths relative to supabase/.
func TestSupabase_PlanConfigToml(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"supabase/config.toml": `project_id = "abcdefghijklmnopqrst"

[functions.api]
verify_jwt = false
entrypoint = "./functions/api/main.ts"
import_map = "./functions/import_map.json"
static_files = ["./functions/api/assets/**/*.html", "./functions/api/VERSION"]

[functions.off]
enabled = false
`,
		"supabase/functions/api/main.ts":            "export default 1;",
		"supabase/functions/api/index.ts":           "// not the entrypoint",
		"supabase/functions/api/VERSION":            "7",
		"supabase/functions/api/assets/a.html":      "<a>",
		"supabase/functions/api/assets/sub/b.html":  "<b>",
		"supabase/functions/api/assets/c.txt":       "no",
		"supabase/functions/import_map.json":        `{"imports": {"std/": "https://deno.land/std/"}}`,
		"supabase/functions/off/index.ts":           "export default 0;",
		"supabase/functions/legacy/index.ts":        "export default 0;",
		"supabase/functions/legacy/import_map.json": `{"imports": {}}`,
	})
	plan, err := sbPlanDeploy(root, "api", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// assets/**/*.html needs a directory between assets and the file, as in
	// the CLI's glob: a.html directly under assets is not matched.
	want := []string{
		"supabase/functions/api/VERSION",
		"supabase/functions/api/assets/sub/b.html",
		"supabase/functions/api/main.ts",
		"supabase/functions/import_map.json",
	}
	if got := planNames(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v\nwant %v", got, want)
	}
	if v, ok := plan.Metadata["verify_jwt"].(bool); !ok || v {
		t.Errorf("verify_jwt = %v, want false from config.toml", plan.Metadata["verify_jwt"])
	}
	if got := plan.Metadata["static_patterns"]; !reflect.DeepEqual(got, []string{"supabase/functions/api/assets/**/*.html", "supabase/functions/api/VERSION"}) {
		t.Errorf("static_patterns = %v", got)
	}
	if plan.Metadata["entrypoint_path"] != "supabase/functions/api/main.ts" || plan.Metadata["import_map_path"] != "supabase/functions/import_map.json" {
		t.Errorf("metadata = %v", plan.Metadata)
	}

	// The call overrides config.toml; the remote value only fills a gap.
	yes := true
	plan, _ = sbPlanDeploy(root, "api", "", &yes, nil)
	if v, _ := plan.Metadata["verify_jwt"].(bool); !v {
		t.Error("verify_jwt override not applied")
	}
	no := false
	writeTree(t, root, map[string]string{"supabase/functions/plain/index.ts": "export default 1;"})
	plan, err = sbPlanDeploy(root, "plain", "", nil, &no)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := plan.Metadata["verify_jwt"].(bool); !ok || v {
		t.Errorf("verify_jwt = %v, want the remote false", plan.Metadata["verify_jwt"])
	}

	if _, err := sbPlanDeploy(root, "off", "", nil, nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled function: err = %v", err)
	}
	if _, err := sbPlanDeploy(root, "legacy", "", nil, nil); err == nil || !strings.Contains(err.Error(), "import_map.json") {
		t.Errorf("import_map.json next to the entrypoint: err = %v", err)
	}
	if _, err := sbPlanDeploy(root, "missing", "", nil, nil); err == nil || !strings.Contains(err.Error(), "entrypoint not found") {
		t.Errorf("missing function: err = %v", err)
	}
}

// Files above the project root cannot be named; files outside the git root
// are skipped with a warning; a deno.json that only references another map
// uploads both.
func TestSupabase_PlanBoundaries(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo", "app")
	writeTree(t, base, map[string]string{
		"repo/.git/HEAD":      "ref\n",
		"repo/shared/util.ts": "export const u = 1;", // inside git root, above project root
		"outside.ts":          "export const o = 1;", // outside git root
		"repo/app/supabase/functions/fn/index.ts": "import '../../../../shared/util.ts';\nimport '../../../../../outside.ts';\n",
	})
	_, err := sbPlanDeploy(root, "fn", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "outside the project root") {
		t.Fatalf("a file above the project root must be refused, got %v", err)
	}

	writeTree(t, base, map[string]string{
		"repo/app/supabase/functions/fn/index.ts":       "import '../../../../../outside.ts';\nimport './ok.ts';\n",
		"repo/app/supabase/functions/fn/ok.ts":          "1",
		"repo/app/supabase/functions/fn/deno.json":      `{"importMap": "./maps/real.json"}`,
		"repo/app/supabase/functions/fn/maps/real.json": `{"imports": {"x/": "./x/"}}`,
		"repo/app/supabase/functions/fn/maps/x/one.ts":  "1",
	})
	plan, err := sbPlanDeploy(root, "fn", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"supabase/functions/fn/deno.json",
		"supabase/functions/fn/index.ts",
		"supabase/functions/fn/maps/real.json",
		"supabase/functions/fn/maps/x/one.ts",
		"supabase/functions/fn/ok.ts",
	}
	if got := planNames(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v\nwant %v", got, want)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "outside source root") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	if plan.SourceRoot != filepath.Join(base, "repo") {
		t.Errorf("source root = %s", plan.SourceRoot)
	}
}

func TestSupabase_ProjectRootArg(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"supabase/functions/fn/index.ts": "1"})
	if got, err := sbProjectRoot(root); err != nil || got == "" {
		t.Errorf("valid root: %v %v", got, err)
	}
	for raw, want := range map[string]string{
		"":                              "path is required",
		"relative/path":                 "absolute",
		filepath.Join(root, "nope"):     "not found",
		filepath.Join(root, "supabase"): "pass the project root",
		filepath.Join(root, "supabase/functions/fn/index.ts"): "not a directory",
		t.TempDir(): "has no supabase/functions",
	} {
		if _, err := sbProjectRoot(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("sbProjectRoot(%q) = %v, want %q", raw, err, want)
		}
	}
}

func TestSupabase_GlobAndImportMapHelpers(t *testing.T) {
	cases := map[string]map[string]bool{
		"a/**/*.html": {"a/x.html": false, "a/b/c/x.html": true, "a/x.txt": false, "b/x.html": false},
		"a/**.html":   {"a/x.html": true, "a/b/x.html": true},
		"a/[x.ts":     {"a/[x.ts": true},
		"a/*.ts":      {"a/x.ts": true, "a/b/x.ts": false},
		"a/?.ts":      {"a/x.ts": true, "a/xy.ts": false},
		"a/[ab].ts":   {"a/a.ts": true, "a/c.ts": false},
		"a/**":        {"a/x": true, "a/b/c": true},
	}
	for glob, checks := range cases {
		re, err := sbGlobRegexp(glob)
		if err != nil {
			t.Fatalf("%s: %v", glob, err)
		}
		for p, want := range checks {
			if re.MatchString(p) != want {
				t.Errorf("%s matches %s = %v, want %v", glob, p, !want, want)
			}
		}
	}

	m := &sbImportMap{
		Imports: map[string]string{"@lib/": "/root/lib/", "zod": "npm:zod", "bad/": "/root/bad"},
		Scopes:  map[string]map[string]string{"/root/fn/": {"@lib/": "/root/fn/lib/"}},
	}
	if r, ok := m.resolveSpecifier("/root/other.ts", "@lib/x.ts"); !ok || r != "/root/lib/x.ts" {
		t.Errorf("imports prefix: %q %v", r, ok)
	}
	if r, ok := m.resolveSpecifier("/root/fn/index.ts", "@lib/x.ts"); !ok || r != "/root/fn/lib/x.ts" {
		t.Errorf("scoped prefix: %q %v", r, ok)
	}
	if r, ok := m.resolveSpecifier("/root/fn/index.ts", "zod"); !ok || r != "npm:zod" {
		t.Errorf("exact: %q %v", r, ok)
	}
	if _, ok := m.resolveSpecifier("/root/fn/index.ts", "bad/x.ts"); ok {
		t.Error("a prefix key whose value lacks the trailing slash must not match")
	}
	if _, ok := m.resolveSpecifier("/root/fn/index.ts", "./x.ts"); ok {
		t.Error("an unmapped specifier must not match")
	}
	for in, want := range map[string]bool{"https://x/y": true, "npm:zod": true, "jsr:@std/x": true, "./x": false, "../x": false, "/abs": false, "bare": false} {
		if sbIsRemote(in) != want {
			t.Errorf("sbIsRemote(%q) = %v", in, !want)
		}
	}
	var parsed map[string]any
	stripped := sbStripJSONComments("\ufeff" + `{"a": "x // not, a comment,]", /* c */ "b": [1, 2,], "c": {"d": 1,},} // tail`)
	if err := json.Unmarshal([]byte(stripped), &parsed); err != nil {
		t.Fatalf("strip = %q: %v", stripped, err)
	}
	if parsed["a"] != "x // not, a comment,]" || len(parsed["b"].([]any)) != 2 {
		t.Errorf("strip = %q", stripped)
	}
}

func TestSupabase_EnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte(`# comment
export OPENAI_API_KEY=sk-plain
QUOTED="with space \"q\" and \n newline"
SINGLE='raw $value # kept'
TRAIL=value # a comment
AFTER_QUOTE="abc" # prod
SUPABASE_URL=https://x.supabase.co
EMPTY=

`), 0o600)
	got, skipped, err := sbReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OPENAI_API_KEY": "sk-plain",
		"QUOTED":         "with space \"q\" and \n newline",
		"SINGLE":         "raw $value # kept",
		"TRAIL":          "value",
		"AFTER_QUOTE":    "abc",
		"EMPTY":          "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %#v\nwant %#v", got, want)
	}
	if !reflect.DeepEqual(skipped, []string{"SUPABASE_URL"}) {
		t.Errorf("skipped = %v", skipped)
	}

	// What the arguments come from a model: the answer must not describe
	// the file, and only dotenv-shaped regular files are read at all.
	if _, _, err := sbReadEnvFile("relative.env"); err == nil {
		t.Error("relative path accepted")
	}
	if _, _, err := sbReadEnvFile(filepath.Join(dir, "missing.env")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing file: %v", err)
	}
	key := filepath.Join(dir, "id_rsa")
	os.WriteFile(key, []byte("yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=\n"), 0o600)
	if _, _, err := sbReadEnvFile(key); err == nil || !strings.Contains(err.Error(), "dotenv file") || strings.Contains(err.Error(), "yAnz") {
		t.Errorf("non-dotenv name: %v", err)
	}
	os.WriteFile(p, []byte("yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=\n"), 0o600)
	if _, _, err := sbReadEnvFile(p); err == nil || strings.Contains(err.Error(), "yAnz") || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("key-like line must fail without echoing it: %v", err)
	}
	os.WriteFile(p, []byte("HOME=/root\x00AWS_SECRET=abc\x00"), 0o600)
	if _, _, err := sbReadEnvFile(p); err == nil || !strings.Contains(err.Error(), "not a text file") {
		t.Errorf("NUL bytes: %v", err)
	}
	os.WriteFile(p, []byte(""), 0o600)
	if _, _, err := sbReadEnvFile(p); err == nil || !strings.Contains(err.Error(), "regular, non-empty") {
		t.Errorf("empty file: %v", err)
	}
	os.WriteFile(p, []byte("not a pair\n"), 0o600)
	if _, _, err := sbReadEnvFile(p); err == nil || !strings.Contains(err.Error(), "line 1") || strings.Contains(err.Error(), "pair") {
		t.Errorf("bad line: %v", err)
	}
	os.WriteFile(p, []byte("X=\"unterminated\n"), 0o600)
	if _, _, err := sbReadEnvFile(p); err == nil || !strings.Contains(err.Error(), "unterminated") {
		t.Errorf("unterminated: %v", err)
	}
	for name, ok := range map[string]bool{".env": true, ".env.local": true, "production.env": true, ".ENV": true, "env": false, "id_rsa": false, ".envrc": false, "environ": false} {
		if sbIsEnvFileName(name) != ok {
			t.Errorf("sbIsEnvFileName(%q) = %v", name, !ok)
		}
	}
}

// A symlink that points out of the checkout must never be uploaded, even
// when an import map tries to widen the boundary (deno.json "importMap"
// pointing far up — the CLI admits that directory, mux does not).
func TestSupabase_PlanSymlinkExfiltration(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret", "id_rsa")
	writeTree(t, base, map[string]string{
		"secret/id_rsa":                          "PRIVATE KEY",
		"repo/.git/HEAD":                         "ref\n",
		"repo/supabase/functions/evil/index.ts":  "export default 1;",
		"repo/supabase/functions/evil/deno.json": `{"importMap": "../../../../x.json"}`,
		"x.json":                                 `{"imports": {"k": "./repo/supabase/functions/evil/key.txt"}}`,
		"repo/supabase/functions/evil2/index.ts": "import './key.txt';",
	})
	root := filepath.Join(base, "repo")
	if err := os.Symlink(secret, filepath.Join(root, "supabase/functions/evil/key.txt")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	os.Symlink(secret, filepath.Join(root, "supabase/functions/evil2/key.txt"))

	// deno.json that only references a map outside the source root: refused
	// (the CLI would admit that map's directory as a boundary).
	if _, err := sbPlanDeploy(root, "evil", "", nil, nil); err == nil || !strings.Contains(err.Error(), "outside the source root") {
		t.Errorf("import map outside the source root must be refused, got %v", err)
	}
	// A map inside the root whose target is a symlink out of it: skipped.
	writeTree(t, base, map[string]string{"repo/supabase/functions/evil/deno.json": `{"imports": {"k": "./key.txt"}}`})
	plan, err := sbPlanDeploy(root, "evil", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Files {
		if strings.Contains(f.Name, "key.txt") || f.Path == secret {
			t.Fatalf("symlink out of the checkout was uploaded: %+v", f)
		}
	}
	if len(plan.Warnings) == 0 || !strings.Contains(plan.Warnings[0], "outside source root") {
		t.Errorf("warnings = %v", plan.Warnings)
	}
	// The same through a relative import.
	plan, err = sbPlanDeploy(root, "evil2", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := planNames(plan); !reflect.DeepEqual(got, []string{"supabase/functions/evil2/index.ts"}) {
		t.Errorf("files = %v", got)
	}
}

// Symlinked directories inside the checkout: a map target that is a symlink
// is followed, a symlinked subdirectory is not descended, a symlinked file
// counts; .env files in a directory target stay home.
func TestSupabase_PlanSymlinkDirectories(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{
		"repo/.git/HEAD":                           "ref\n",
		"repo/packages/shared/y.ts":                "export const y = 1;",
		"repo/packages/shared/.env":                "SECRET=1",
		"repo/packages/shared/nested/z.ts":         "export const z = 1;",
		"repo/packages/lib/l.ts":                   "export const l = 1;",
		"repo/app/supabase/functions/fn/index.ts":  "import { y } from 'shared/y.ts';",
		"repo/app/supabase/functions/fn/deno.json": `{"imports": {"shared/": "../_shared/"}}`,
	})
	root := filepath.Join(base, "repo", "app")
	if err := os.Symlink(filepath.Join(base, "repo/packages/shared"), filepath.Join(root, "supabase/functions/_shared")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	os.Symlink(filepath.Join(base, "repo/packages/lib"), filepath.Join(base, "repo/packages/shared/lib"))
	os.Symlink(filepath.Join(base, "repo/packages/lib/l.ts"), filepath.Join(base, "repo/packages/shared/l-link.ts"))
	plan, err := sbPlanDeploy(root, "fn", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"supabase/functions/_shared/l-link.ts",
		"supabase/functions/_shared/nested/z.ts",
		"supabase/functions/_shared/y.ts",
		"supabase/functions/fn/deno.json",
		"supabase/functions/fn/index.ts",
	}
	if got := planNames(plan); !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v\nwant %v\nwarnings %v", got, want, plan.Warnings)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], ".env") {
		t.Errorf("warnings = %v, want one about .env", plan.Warnings)
	}
	// Uploads open the resolved path.
	for _, f := range plan.Files {
		if strings.Contains(f.Path, "_shared") {
			t.Errorf("file path not resolved: %s", f.Path)
		}
	}
}

// [remotes.<name>] blocks whose project_id is the connection's project
// overlay the base function table, as the CLI's config loader does.
func TestSupabase_PlanRemotesOverlay(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"supabase/config.toml": `project_id = "local"

[functions.hook]
verify_jwt = false

[remotes.production]
project_id = "abcdefghijklmnopqrst"

[remotes.production.functions.hook]
verify_jwt = true

[remotes.staging]
project_id = "tsrqponmlkjihgfedcba"

[remotes.staging.functions.hook]
enabled = false
`,
		"supabase/functions/hook/index.ts": "export default 1;",
	})
	plan, err := sbPlanDeploy(root, "hook", "abcdefghijklmnopqrst", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := plan.Metadata["verify_jwt"].(bool); !v {
		t.Errorf("production overlay not applied: %v", plan.Metadata)
	}
	plan, err = sbPlanDeploy(root, "hook", "zzzzzzzzzzzzzzzzzzzz", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := plan.Metadata["verify_jwt"].(bool); v {
		t.Errorf("unrelated project must see the base config: %v", plan.Metadata)
	}
	if _, err := sbPlanDeploy(root, "hook", "tsrqponmlkjihgfedcba", nil, nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("staging overlay (enabled = false): %v", err)
	}
}

// Non-regular files are refused before anything is opened for upload.
func TestSupabase_PlanRefusesFifo(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"supabase/functions/fn/index.ts": "import './pipe.ts';"})
	if err := syscallMkfifo(filepath.Join(root, "supabase/functions/fn/pipe.ts")); err != nil {
		t.Skip("mkfifo not available:", err)
	}
	if _, err := sbPlanDeploy(root, "fn", "", nil, nil); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("fifo import: %v", err)
	}
}

func TestSupabase_SecretNamesAndSlugs(t *testing.T) {
	for name, ok := range map[string]bool{"OPENAI_API_KEY": true, "lower_ok": true, "_X1": true, "SUPABASE_URL": false, "1ABC": false, "WITH-DASH": false, "": false} {
		if err := sbValidSecretName(name); (err == nil) != ok {
			t.Errorf("sbValidSecretName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
	for slug, ok := range map[string]bool{"hello-world": true, "a1_b": true, "1abc": false, "-x": false, "a/b": false, "": false, "ä": false} {
		if sbValidSlug(slug) != ok {
			t.Errorf("sbValidSlug(%q) = %v", slug, !ok)
		}
	}
}
