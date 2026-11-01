package tools

import (
	"encoding/json"
	"fmt"
	"os"
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
		{Version: "20260901120000", Name: "add_widgets"},
	}
	cases := []struct {
		file    string
		applied bool
	}{
		{"20260710085001_929350e6-7b35-4f7e-818f-91f6a6c404aa.sql", true}, // version and stem
		{"20260805175902_8447fd06-6bca-464e-88de-4bad72f7834c.sql", true}, // name part; version shifted by the server
		{"20260901120000_add_widgets.sql", true},                          // CLI record
		{"20260901120000.sql", true},                                      // version only
		{"20261001000000_new_thing.sql", false},
		{"20260805175902_other.sql", false}, // same timestamp, other name
	}
	for _, c := range cases {
		m := sbMigrationFile.FindStringSubmatch(c.file)
		l := sbLocalMigration{Version: m[1], Name: m[2], Stem: strings.TrimSuffix(c.file, ".sql")}
		if _, got := sbMigrationApplied(l, history); got != c.applied {
			t.Errorf("%s applied = %v, want %v", c.file, got, c.applied)
		}
	}
	for name, ok := range map[string]bool{"20260101000000_x.sql": true, "20260101000000.sql": true, "readme.md": false, "x_20260101.sql": false, "20260101000000_x.sql.bak": false} {
		if (sbMigrationFile.MatchString(name)) != ok {
			t.Errorf("file pattern %q = %v", name, !ok)
		}
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
	if !strings.Contains(out["failed"].(map[string]any)["error"].(string), "BROKEN") {
		t.Errorf("error text = %v", out["failed"])
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

	// Empty files and a missing directory are refused before anything runs.
	empty := t.TempDir()
	writeMigrations(t, empty, map[string]string{"20261001000000_e.sql": ""})
	if _, errText := sbCall(t, s, s.handleMigrationsApply, map[string]any{"path": empty}); !strings.Contains(errText, "is empty") {
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
	_ = os.Remove
	_ = filepath.Join
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
}
