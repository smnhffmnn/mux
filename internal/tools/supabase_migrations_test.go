package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

func writeMigrations(t *testing.T, root string, files map[string]string) {
	t.Helper()
	m := map[string]string{}
	for name, sql := range files {
		m["supabase/migrations/"+name] = sql
	}
	writeTree(t, root, m)
}

func TestSupabase_MigrationMatching(t *testing.T) {
	history := []sbRemoteMigration{
		{Version: "20260710085001", Name: "20260710085001_929350e6-7b35-4f7e-818f-91f6a6c404aa"},
		{Version: "20260805175905", Name: "8447fd06-6bca-464e-88de-4bad72f7834c"},
		{Version: "20260901120000", Name: "remote_schema"},
	}
	mk := func(file string) sbLocalMigration {
		m := sbMigrationFile.FindStringSubmatch(file)
		return sbLocalMigration{Version: m[1], Name: m[2], Stem: strings.TrimSuffix(file, ".sql")}
	}
	local := []sbLocalMigration{
		mk("20260710085001_929350e6-7b35-4f7e-818f-91f6a6c404aa.sql"), // version and stem
		mk("20260805175902_8447fd06-6bca-464e-88de-4bad72f7834c.sql"), // name part; version shifted by the server
		mk("20260901120000_remote_schema.sql"),                        // CLI record
		mk("20261001000000_remote_schema.sql"),                        // same name as a CLI record: NOT applied (one-to-one)
		mk("20261002000000_new_thing.sql"),
		mk("20260805175902_other.sql"), // same timestamp as a local file, other name
	}
	applied, pending, matches := sbMatchMigrations(local, history)
	stems := func(l []sbLocalMigration) []string {
		var out []string
		for _, x := range l {
			out = append(out, x.Stem)
		}
		return out
	}
	wantApplied := []string{"20260710085001_929350e6-7b35-4f7e-818f-91f6a6c404aa", "20260805175902_8447fd06-6bca-464e-88de-4bad72f7834c", "20260901120000_remote_schema"}
	wantPending := []string{"20261001000000_remote_schema", "20261002000000_new_thing", "20260805175902_other"}
	if !reflect.DeepEqual(stems(applied), wantApplied) || !reflect.DeepEqual(stems(pending), wantPending) {
		t.Errorf("applied = %v\npending = %v", stems(applied), stems(pending))
	}
	if matches["20260805175902_8447fd06-6bca-464e-88de-4bad72f7834c"].Version != "20260805175905" {
		t.Errorf("matches = %v", matches)
	}
	// Two local files on one free name-part entry: the first takes it.
	twice := []sbLocalMigration{mk("20261101000000_fix_rls.sql"), mk("20261102000000_fix_rls.sql")}
	a, p, _ := sbMatchMigrations(twice, []sbRemoteMigration{{Version: "20261101000007", Name: "fix_rls"}})
	if len(a) != 1 || len(p) != 1 || p[0].Stem != "20261102000000_fix_rls" {
		t.Errorf("one entry, two files: applied %v pending %v", stems(a), stems(p))
	}
	for name, ok := range map[string]bool{"20260101000000_x.sql": true, "20260101000000.sql": false, "readme.md": false, "x_20260101.sql": false, "20260101000000_x.sql.bak": false} {
		if (sbMigrationFile.MatchString(name)) != ok {
			t.Errorf("file pattern %q = %v", name, !ok)
		}
	}
	if got := sbOlderThanApplied([]sbLocalMigration{mk("20261001000000_a.sql")}, []sbLocalMigration{mk("20260901000000_old.sql"), mk("20261101000000_new.sql")}); !reflect.DeepEqual(got, []string{"20260901000000_old"}) {
		t.Errorf("older = %v", got)
	}
	if strings.TrimSpace(sbStripSQLComments("-- only\n/* a\nb */  \n")) != "" || strings.TrimSpace(sbStripSQLComments("select 1; -- x")) != "select 1;" {
		t.Errorf("strip = %q / %q", sbStripSQLComments("-- only\n/* a\nb */  \n"), sbStripSQLComments("select 1; -- x"))
	}
}

func TestSupabase_MigrationsPendingAndApply(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	root := t.TempDir()
	writeMigrations(t, root, map[string]string{
		"20260710085001_929350e6-7b35-4f7e-818f-91f6a6c404aa.sql": "select 1;",
		"20260805175902_8447fd06-6bca-464e-88de-4bad72f7834c.sql": "select 2;",
		"20260901120000_add_widgets.sql":                          "create table w(id int);",
		"20261001000000_first_new.sql":                            "alter table w add c int; alter table w add d int;",
		"20261002000000_second_new.sql":                           "create index on w(c);",
		"notes.md":                                                "not a migration",
		"20261003000000.sql":                                      "select 1; -- bare version: skipped like the CLI does",
	})

	out, errText := sbCall(t, s, s.handleMigrationsList, nil)
	if errText != "" {
		t.Fatal(errText)
	}
	if out["count"] != float64(3) {
		t.Errorf("list = %v", out)
	}

	out, errText = sbCall(t, s, s.handleMigrationsPending, map[string]any{"path": root})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["localCount"] != float64(5) || out["historyCount"] != float64(3) || out["pendingCount"] != float64(2) {
		t.Fatalf("pending = %v", out)
	}
	pend := out["pending"].([]any)
	if pend[0].(map[string]any)["file"] != "20261001000000_first_new" || pend[1].(map[string]any)["file"] != "20261002000000_second_new" {
		t.Errorf("pending order = %v", pend)
	}
	if ap := out["applied"].([]any); len(ap) != 3 || ap[1].(map[string]any)["historyVersion"] != "20260805175905" {
		t.Errorf("applied = %v", ap)
	}
	if out["historyOnly"] != nil {
		t.Errorf("historyOnly = %v", out["historyOnly"])
	}
	if fmt.Sprint(out["skipped"]) != "[20261003000000.sql notes.md]" {
		t.Errorf("skipped = %v", out["skipped"])
	}
	if _, ok := out["olderThanLatestApplied"]; ok {
		t.Errorf("nothing out of order here: %v", out["olderThanLatestApplied"])
	}

	// Dry run: plan, no POST.
	out, errText = sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root, "dry_run": true})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["dryRun"] != true || out["plannedCount"] != float64(2) || f.count("POST", "/database/migrations") != 0 {
		t.Errorf("dry run = %v", out)
	}
	planned := out["planned"].([]any)
	if planned[0].(map[string]any)["statementsApprox"] != float64(2) || planned[0].(map[string]any)["sha256"] == "" {
		t.Errorf("planned = %v", planned)
	}

	// Apply all: in order, name = stem, idempotency key, versions read back.
	out, errText = sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["appliedCount"] != float64(2) {
		t.Fatalf("apply = %v", out)
	}
	applied := out["applied"].([]any)
	if applied[0].(map[string]any)["file"] != "20261001000000_first_new" || applied[0].(map[string]any)["historyVersion"] != "20261101000003" {
		t.Errorf("applied = %v", applied)
	}
	req, _ := f.last("POST", "/database/migrations")
	var body map[string]string
	json.Unmarshal(req.Body, &body)
	if body["name"] != "20261002000000_second_new" || body["query"] != "create index on w(c);" {
		t.Errorf("body = %v", body)
	}
	if k := req.Header.Get("Idempotency-Key"); !strings.HasPrefix(k, "20261002000000_second_new-") {
		t.Errorf("idempotency key = %q", k)
	}
	if f.count("POST", "/database/migrations") != 2 {
		t.Errorf("posts = %d", f.count("POST", "/database/migrations"))
	}

	// Nothing pending afterwards; the history recognises the stems.
	out, _ = sbCall(t, s, s.handleMigrationsPending, map[string]any{"path": root})
	if out["pendingCount"] != float64(0) {
		t.Errorf("after apply pending = %v", out)
	}
	out, _ = sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root})
	if out["plannedCount"] != float64(0) || !strings.Contains(fmt.Sprint(out["note"]), "nothing pending") {
		t.Errorf("second apply = %v", out)
	}
}

func TestSupabase_MigrationsApplyStopsAndSingle(t *testing.T) {
	f := newSBFake(t)
	s := newTestSupabase(t, f, nil)
	root := t.TempDir()
	writeMigrations(t, root, map[string]string{
		"20261001000000_a.sql": "select 1;",
		"20261002000000_b.sql": "BROKEN;",
		"20261003000000_c.sql": "select 3;",
	})
	f.mu.Lock()
	f.failApply = "20261002000000_b"
	f.mu.Unlock()
	out, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root})
	if errText != "" {
		t.Fatal(errText)
	}
	if len(out["applied"].([]any)) != 1 || out["failed"].(map[string]any)["file"] != "20261002000000_b" || fmt.Sprint(out["remaining"]) != "[20261003000000_c]" {
		t.Errorf("stop at first error = %v", out)
	}
	if failed := out["failed"].(map[string]any); !strings.Contains(failed["error"].(string), "BROKEN") || failed["state"] != "not applied" {
		t.Errorf("failed = %v", failed)
	}
	if applied := out["applied"].([]any); applied[0].(map[string]any)["historyVersion"] == "" {
		t.Errorf("applied before the failure lacks its version: %v", applied)
	}
	if f.count("POST", "/database/migrations") != 2 {
		t.Errorf("posts after failure = %d, want 2 (a ok, b failed, c not tried)", f.count("POST", "/database/migrations"))
	}

	// A single file, out of order, says what stays pending; an applied one is refused.
	f.mu.Lock()
	f.failApply = ""
	f.mu.Unlock()
	out, errText = sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root, "file": "20261003000000_c.sql"})
	if errText != "" {
		t.Fatal(errText)
	}
	if out["appliedCount"] != float64(1) || !strings.Contains(fmt.Sprint(out["note"]), "20261002000000_b") {
		t.Errorf("single = %v", out)
	}
	if _, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root, "file": "20261001000000"}); !strings.Contains(errText, "already in the history") {
		t.Errorf("applied single: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root, "file": "nope"}); !strings.Contains(errText, "not in supabase/migrations") {
		t.Errorf("unknown single: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root, "dry_run": "yes"}); !strings.Contains(errText, "boolean") {
		t.Errorf("dry_run type: %q", errText)
	}

	// A lost answer: the state is unknown, not "not applied"; the history
	// read back says it ran.
	f2 := newSBFake(t)
	s2 := newTestSupabase(t, f2, nil)
	root2 := t.TempDir()
	writeMigrations(t, root2, map[string]string{"20261001000000_slow.sql": "select pg_sleep(1);", "20261002000000_after.sql": "select 2;"})
	f2.mu.Lock()
	f2.dropApply = "20261001000000_slow"
	f2.mu.Unlock()
	out, errText = sbCall(t, s2, s2.handleMigrationsApply, map[string]any{"path": root2})
	if errText != "" {
		t.Fatal(errText)
	}
	failed := out["failed"].(map[string]any)
	if !strings.HasPrefix(fmt.Sprint(failed["state"]), "applied — the history has it") || fmt.Sprint(out["remaining"]) != "[20261002000000_after]" || !strings.Contains(fmt.Sprint(out["note"]), "unknown") {
		t.Errorf("lost answer = %v", out)
	}
	if f2.count("POST", "/database/migrations") != 1 {
		t.Errorf("the run must stop after a lost answer, posts = %d", f2.count("POST", "/database/migrations"))
	}

	// Ambiguous file argument, out-of-order warning.
	root3 := t.TempDir()
	writeMigrations(t, root3, map[string]string{"20260101000000_a.sql": "select 1;", "20260101000000_b.sql": "select 2;", "20260801000000_old.sql": "select 3;"})
	if _, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root3, "file": "20260101000000", "dry_run": true}); !strings.Contains(errText, "names 2 migrations") {
		t.Errorf("ambiguous: %q", errText)
	}
	out, _ = sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root3, "dry_run": true})
	if _, ok := out["olderThanLatestApplied"]; ok {
		t.Errorf("no applied file, no order warning: %v", out)
	}
	writeMigrations(t, root3, map[string]string{"20260901120000_add_widgets.sql": "create table w(id int);"}) // applied in the fake history
	out, _ = sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": root3, "dry_run": true})
	if fmt.Sprint(out["olderThanLatestApplied"]) != "[20260101000000_a 20260101000000_b 20260801000000_old]" || out["orderWarning"] == nil {
		t.Errorf("order warning = %v", out)
	}

	// Empty or comment-only files and a missing directory are refused before anything runs.
	empty := t.TempDir()
	writeMigrations(t, empty, map[string]string{"20261001000000_e.sql": "-- nothing here\n/* nor here */\n"})
	if _, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": empty}); !strings.Contains(errText, "holds no SQL") {
		t.Errorf("empty file: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleMigrationsPending, map[string]any{"path": t.TempDir()}); !strings.Contains(errText, "has no supabase/migrations") {
		t.Errorf("no dir: %q", errText)
	}
	if _, errText := sbCall(t, s, s.handleMigrationsPending, map[string]any{"path": "relative"}); !strings.Contains(errText, "absolute") {
		t.Errorf("relative: %q", errText)
	}

	// Read-only connections have neither apply tool.
	ro := newTestSupabase(t, f, func(c *config.Connection) { c.ReadOnly = true })
	for _, td := range ro.Tools() {
		if td.Tool.Name == "migrations_apply" {
			t.Error("migrations_apply registered on a read-only connection")
		}
	}
	names := []string{}
	for _, td := range ro.Tools() {
		names = append(names, td.Tool.Name)
	}
	if !reflect.DeepEqual(names[len(names)-2:], []string{"migrations_list", "migrations_pending"}) {
		t.Errorf("read-only tools = %v", names)
	}
}

func TestSupabase_ComparePlan(t *testing.T) {
	plan := &sbDeployPlan{
		Metadata: map[string]any{"entrypoint_path": "supabase/functions/hook/index.ts"},
		Files: []sbDeployFile{
			{Name: "supabase/functions/hook/index.ts", Size: 10},
			{Name: "supabase/functions/_shared/a.ts", Size: 20},
			{Name: "supabase/functions/hook/deno.json", Size: 5},
		},
	}
	deployed := []sbBodyFile{
		{Path: "functions/hook/index.ts", Size: 10},
		{Path: "functions/_shared/a.ts", Size: 21},
		{Path: "functions/_shared/old.ts", Size: 3},
	}
	out := sbComparePlan(plan, deployed, "supabase/functions/hook/index.ts")
	if out["sameFiles"] != false || fmt.Sprint(out["onlyLocal"]) != "[functions/hook/deno.json]" || fmt.Sprint(out["onlyDeployed"]) != "[functions/_shared/old.ts]" || fmt.Sprint(out["sizeDiffers"]) != "[functions/_shared/a.ts (local 20, deployed 21)]" {
		t.Errorf("compare = %v", out)
	}
	// Same anchoring on both sides, identical sets.
	same := sbComparePlan(plan, []sbBodyFile{
		{Path: "supabase/functions/hook/index.ts", Size: 10},
		{Path: "supabase/functions/_shared/a.ts", Size: 20},
		{Path: "supabase/functions/hook/deno.json", Size: 5},
	}, "supabase/functions/hook/index.ts")
	if same["sameFiles"] != true {
		t.Errorf("same = %v", same)
	}
	if sbCommonSuffix("supabase/functions/hook/index.ts", "functions/hook/index.ts") != "functions/hook/index.ts" {
		t.Error("common suffix")
	}

	// Same size, different bytes: contentDiffers. Two files on one anchored
	// name: a warning, never sameFiles.
	root := t.TempDir()
	writeTree(t, root, map[string]string{"supabase/functions/hook/index.ts": "0123456789", "a.ts": "x"})
	p2 := &sbDeployPlan{
		Metadata: map[string]any{"entrypoint_path": "supabase/functions/hook/index.ts"},
		Files: []sbDeployFile{
			{Name: "supabase/functions/hook/index.ts", Path: filepath.Join(root, "supabase/functions/hook/index.ts"), Size: 10},
		},
	}
	out = sbComparePlan(p2, []sbBodyFile{{Path: "functions/hook/index.ts", Size: 10, content: []byte("9876543210")}}, "supabase/functions/hook/index.ts")
	if out["sameFiles"] != false || fmt.Sprint(out["contentDiffers"]) != "[functions/hook/index.ts]" {
		t.Errorf("content compare = %v", out)
	}
	out = sbComparePlan(p2, []sbBodyFile{{Path: "functions/hook/index.ts", Size: 10, content: []byte("0123456789")}}, "supabase/functions/hook/index.ts")
	if out["sameFiles"] != true || out["anchoredAt"] != "functions" {
		t.Errorf("byte-equal = %v", out)
	}
	p2.Files = append(p2.Files, sbDeployFile{Name: "functions/hook/index.ts", Path: filepath.Join(root, "a.ts"), Size: 1})
	out = sbComparePlan(p2, []sbBodyFile{{Path: "functions/hook/index.ts", Size: 10, content: []byte("0123456789")}}, "supabase/functions/hook/index.ts")
	if out["sameFiles"] != false || out["warning"] == nil {
		t.Errorf("collision = %v", out)
	}
}
