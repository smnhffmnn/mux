package tools

import (
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

// A function like addgai's webhook: relative imports into _shared (several
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
	plan, err := sbPlanDeploy(root, "hook", nil, nil)
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
	plan, err := sbPlanDeploy(root, "fn", nil, nil)
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
	plan, err := sbPlanDeploy(root, "api", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"supabase/functions/api/VERSION",
		"supabase/functions/api/assets/a.html",
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
	plan, _ = sbPlanDeploy(root, "api", &yes, nil)
	if v, _ := plan.Metadata["verify_jwt"].(bool); !v {
		t.Error("verify_jwt override not applied")
	}
	no := false
	writeTree(t, root, map[string]string{"supabase/functions/plain/index.ts": "export default 1;"})
	plan, err = sbPlanDeploy(root, "plain", nil, &no)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := plan.Metadata["verify_jwt"].(bool); !ok || v {
		t.Errorf("verify_jwt = %v, want the remote false", plan.Metadata["verify_jwt"])
	}

	if _, err := sbPlanDeploy(root, "off", nil, nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled function: err = %v", err)
	}
	if _, err := sbPlanDeploy(root, "legacy", nil, nil); err == nil || !strings.Contains(err.Error(), "import_map.json") {
		t.Errorf("import_map.json next to the entrypoint: err = %v", err)
	}
	if _, err := sbPlanDeploy(root, "missing", nil, nil); err == nil || !strings.Contains(err.Error(), "entrypoint not found") {
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
	_, err := sbPlanDeploy(root, "fn", nil, nil)
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
	plan, err := sbPlanDeploy(root, "fn", nil, nil)
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
		"a/**/*.html": {"a/x.html": true, "a/b/c/x.html": true, "a/x.txt": false, "b/x.html": false},
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
	if got := sbStripJSONComments(`{"a": "x // not a comment", /* c */ "b": [1, 2,], } // tail`); got != `{"a": "x // not a comment",  "b": [1, 2], } ` && !strings.Contains(got, `"b": [1, 2]`) {
		t.Errorf("strip = %q", got)
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
EMPTY=

`), 0o600)
	got, err := sbReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OPENAI_API_KEY": "sk-plain",
		"QUOTED":         "with space \"q\" and \n newline",
		"SINGLE":         "raw $value # kept",
		"TRAIL":          "value",
		"EMPTY":          "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %#v\nwant %#v", got, want)
	}
	if _, err := sbReadEnvFile("relative.env"); err == nil {
		t.Error("relative path accepted")
	}
	if _, err := sbReadEnvFile(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing file: %v", err)
	}
	os.WriteFile(p, []byte("not a pair\n"), 0o600)
	if _, err := sbReadEnvFile(p); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("bad line: %v", err)
	}
}

func TestSupabase_SecretNamesAndSlugs(t *testing.T) {
	for name, ok := range map[string]bool{"OPENAI_API_KEY": true, "lower_ok": true, "_X1": true, "SUPABASE_URL": false, "1ABC": false, "WITH-DASH": false, "": false} {
		if err := sbValidSecretName(name); (err == nil) != ok {
			t.Errorf("sbValidSecretName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
	for slug, ok := range map[string]bool{"whatsapp-webhook": true, "a1_b": true, "1abc": false, "-x": false, "a/b": false, "": false, "ä": false} {
		if sbValidSlug(slug) != ok {
			t.Errorf("sbValidSlug(%q) = %v", slug, !ok)
		}
	}
}
