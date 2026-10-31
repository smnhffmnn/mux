//go:build live

// Live contract test for the supabase type. Runs only with:
//
//	SUPABASE_LIVE_TOKEN=sbp_… SUPABASE_LIVE_REF=<project ref> \
//	  go test -tags 'notray live' ./internal/tools -run TestSupabaseLive -v
//
// It reads the project, lists the functions, reads one back, lists the
// secret names, runs a harmless query and reads the last hour of logs.
// Nothing is deployed, deleted or written unless SUPABASE_LIVE_DEPLOY names
// a project root and SUPABASE_LIVE_SLUG a function — then that function is
// deployed from that checkout (the version advances). Gated behind the build
// tag and the environment because it touches a real project and counts
// against the API's rate limit. The log carries counts and names only: in CI
// it is public.

package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

func newLiveSupabase(t *testing.T) *Supabase {
	t.Helper()
	token := os.Getenv("SUPABASE_LIVE_TOKEN")
	ref := os.Getenv("SUPABASE_LIVE_REF")
	if token == "" || ref == "" {
		t.Skip("SUPABASE_LIVE_TOKEN or SUPABASE_LIVE_REF not set")
	}
	s, err := NewSupabase(config.Connection{Name: "supabase-live", Type: config.TypeSupabase, Token: token, ProjectRef: ref}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSupabaseLive_Read(t *testing.T) {
	s := newLiveSupabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	p, err := s.Verify(ctx)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	t.Logf("project %s (%s, %s, %s)", p.Name, p.Ref, p.Region, p.Status)

	res, err := s.handleFunctionsList(ctx, toolRequest(nil))
	if err != nil || res.IsError {
		t.Fatalf("functions_list: %v / %s", err, resultText(t, res))
	}
	var list struct {
		Count     int          `json:"count"`
		Functions []sbFunction `json:"functions"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &list); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d functions", list.Count)
	for _, f := range list.Functions {
		t.Logf("  %s v%d %s updated %s", f.Slug, f.Version, f.Status, f.Updated)
	}

	if len(list.Functions) > 0 {
		slug := os.Getenv("SUPABASE_LIVE_SLUG")
		if slug == "" {
			slug = list.Functions[0].Slug
		}
		res, err = s.handleFunctionGet(ctx, toolRequest(map[string]any{"slug": slug}))
		if err != nil || res.IsError {
			t.Fatalf("function_get %s: %v / %s", slug, err, resultText(t, res))
		}
		var got struct {
			Files     []sbBodyFile   `json:"files"`
			FileCount int            `json:"fileCount"`
			Bundle    map[string]any `json:"bundle"`
			Metadata  map[string]any `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(resultText(t, res)), &got); err != nil {
			t.Fatal(err)
		}
		if got.Bundle != nil {
			t.Logf("%s: pre-built bundle, %v bytes", slug, got.Bundle["size"])
		} else {
			t.Logf("%s: %d files, metadata %v", slug, got.FileCount, got.Metadata)
			for _, f := range got.Files {
				t.Logf("  %s (%d bytes)", f.Path, f.Size)
			}
		}
	}

	res, err = s.handleSecretsList(ctx, toolRequest(nil))
	if err != nil || res.IsError {
		t.Fatalf("secrets_list: %v / %s", err, resultText(t, res))
	}
	var secrets struct {
		Count int `json:"count"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &secrets)
	t.Logf("%d secrets", secrets.Count)

	res, err = s.handleSQLQuery(ctx, toolRequest(map[string]any{"query": "select current_user as u, version() as v", "read_only": true}))
	if err != nil || res.IsError {
		t.Fatalf("sql_query: %v / %s", err, resultText(t, res))
	}
	t.Logf("sql_query: %s", resultText(t, res))

	res, err = s.handleLogs(ctx, toolRequest(map[string]any{"source": "function_edge_logs", "limit": float64(5)}))
	if err != nil || res.IsError {
		t.Fatalf("logs: %v / %s", err, resultText(t, res))
	}
	var logs struct {
		RowCount int    `json:"rowCount"`
		SQL      string `json:"sql"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &logs)
	t.Logf("logs: %d rows in the last hour (%s)", logs.RowCount, logs.SQL)
}

func TestSupabaseLive_Deploy(t *testing.T) {
	root := os.Getenv("SUPABASE_LIVE_DEPLOY")
	slug := os.Getenv("SUPABASE_LIVE_SLUG")
	if root == "" || slug == "" {
		t.Skip("SUPABASE_LIVE_DEPLOY (project root) or SUPABASE_LIVE_SLUG not set — no deploy")
	}
	s := newLiveSupabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	res, err := s.handleFunctionDeploy(ctx, toolRequest(map[string]any{"path": root, "slug": slug, "dry_run": true}))
	if err != nil || res.IsError {
		t.Fatalf("dry run: %v / %s", err, resultText(t, res))
	}
	t.Logf("plan: %s", resultText(t, res))

	res, err = s.handleFunctionDeploy(ctx, toolRequest(map[string]any{"path": root, "slug": slug}))
	if err != nil || res.IsError {
		t.Fatalf("deploy: %v / %s", err, resultText(t, res))
	}
	var out struct {
		Deployed bool       `json:"deployed"`
		Before   sbFunction `json:"before"`
		Function sbFunction `json:"function"`
		Warning  string     `json:"warning"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &out)
	t.Logf("deployed %s: version %d → %d %s", slug, out.Before.Version, out.Function.Version, out.Warning)
	if !out.Deployed || out.Function.Version <= out.Before.Version {
		t.Errorf("version did not advance")
	}
}
