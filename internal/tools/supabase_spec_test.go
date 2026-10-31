package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The spec tests hold the supabase type against the Management API's own
// OpenAPI document: every registered endpoint must exist with its method and
// query parameters, every field the type reads or writes must be a property
// of the component schema listed in sbSchemaFields, and the deploy metadata
// must match the nested schema of FunctionDeployBody. The document is the
// published https://api.supabase.com/api/v1-json, vendored (prose and
// examples stripped) under testdata by scripts/supabase-openapi-snapshot.sh.
// The drift workflow runs the same tests against the live document
// (SUPABASE_OPENAPI=/path), so an upstream rename fails a test before it
// fails a user.

const sbSpecSnapshot = "testdata/supabase-openapi.json"

type sbSpec struct {
	Info struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]*sbSpecSchema `json:"schemas"`
	} `json:"components"`
}

type sbSpecSchema struct {
	Ref        string                   `json:"$ref"`
	Type       any                      `json:"type"`
	AllOf      []*sbSpecSchema          `json:"allOf"`
	OneOf      []*sbSpecSchema          `json:"oneOf"`
	AnyOf      []*sbSpecSchema          `json:"anyOf"`
	Items      *sbSpecSchema            `json:"items"`
	Properties map[string]*sbSpecSchema `json:"properties"`
	Deprecated bool                     `json:"deprecated"`
}

type sbSpecParam struct {
	Name string `json:"name"`
	In   string `json:"in"`
}

func loadSupabaseSpec(t *testing.T) *sbSpec {
	t.Helper()
	path := os.Getenv("SUPABASE_OPENAPI")
	if path == "" {
		path = sbSpecSnapshot
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OpenAPI document %s: %v", path, err)
	}
	var spec sbSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse OpenAPI document %s: %v", path, err)
	}
	if len(spec.Paths) == 0 || len(spec.Components.Schemas) == 0 {
		t.Fatalf("OpenAPI document %s carries no paths or schemas", path)
	}
	t.Logf("%s %s from %s: %d paths, %d schemas", spec.Info.Title, spec.Info.Version, filepath.Base(path), len(spec.Paths), len(spec.Components.Schemas))
	return &spec
}

func (s *sbSpec) resolve(sc *sbSpecSchema) *sbSpecSchema {
	for depth := 0; sc != nil && sc.Ref != "" && depth < 10; depth++ {
		const prefix = "#/components/schemas/"
		if !strings.HasPrefix(sc.Ref, prefix) {
			return nil
		}
		sc = s.Components.Schemas[strings.TrimPrefix(sc.Ref, prefix)]
	}
	return sc
}

// properties collects the properties a schema declares, through $ref and
// allOf/oneOf/anyOf.
func (s *sbSpec) properties(sc *sbSpecSchema) map[string]*sbSpecSchema {
	out := map[string]*sbSpecSchema{}
	var walk func(*sbSpecSchema, int)
	walk = func(sc *sbSpecSchema, depth int) {
		sc = s.resolve(sc)
		if sc == nil || depth > 10 {
			return
		}
		for k, v := range sc.Properties {
			out[k] = v
		}
		for _, list := range [][]*sbSpecSchema{sc.AllOf, sc.OneOf, sc.AnyOf} {
			for _, sub := range list {
				walk(sub, depth+1)
			}
		}
	}
	walk(sc, 0)
	return out
}

func (s *sbSpec) params(t *testing.T, where, in string, pathItem map[string]json.RawMessage, rawOp json.RawMessage) map[string]bool {
	params := map[string]bool{}
	collect := func(raw json.RawMessage) {
		var list []sbSpecParam
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Errorf("%s: cannot parse parameters: %v", where, err)
			return
		}
		for _, p := range list {
			if p.In == in {
				params[p.Name] = true
			}
		}
	}
	if raw, ok := pathItem["parameters"]; ok {
		collect(raw)
	}
	var op struct {
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(rawOp, &op); err != nil {
		t.Errorf("%s: cannot parse operation: %v", where, err)
		return params
	}
	if len(op.Parameters) > 0 {
		collect(op.Parameters)
	}
	return params
}

func TestSupabase_SpecEndpoints(t *testing.T) {
	spec := loadSupabaseSpec(t)

	keys := make([]string, 0, len(sbEndpoints))
	for k := range sbEndpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ep := sbEndpoints[key]
		where := fmt.Sprintf("%s (%s %s)", key, ep.Method, ep.Path)
		pathItem, exists := spec.Paths[ep.Path]
		if !exists {
			t.Errorf("%s: path is not in the OpenAPI document", where)
			continue
		}
		rawOp, ok := pathItem[strings.ToLower(ep.Method)]
		if !ok {
			t.Errorf("%s: method not offered on this path (has %s)", where, pathMethods(pathItem))
			continue
		}
		var op struct {
			Deprecated bool `json:"deprecated"`
		}
		if json.Unmarshal(rawOp, &op) == nil && op.Deprecated {
			t.Errorf("%s: the operation is deprecated", where)
		}
		params := spec.params(t, where, "query", pathItem, rawOp)
		for _, p := range ep.Params {
			if !params[p] {
				t.Errorf("%s: query parameter %q is not documented for this operation", where, p)
			}
		}
		// Every {placeholder} of the path must be a documented path parameter.
		pathParams := spec.params(t, where, "path", pathItem, rawOp)
		for _, seg := range strings.Split(ep.Path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				if name := seg[1 : len(seg)-1]; !pathParams[name] {
					t.Errorf("%s: path parameter %q is not documented", where, name)
				}
			}
		}
	}
}

func TestSupabase_SpecFields(t *testing.T) {
	spec := loadSupabaseSpec(t)

	names := make([]string, 0, len(sbSchemaFields))
	for k := range sbSchemaFields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		sc, ok := spec.Components.Schemas[name]
		if !ok {
			t.Errorf("schema %q is not in the OpenAPI document", name)
			continue
		}
		props := spec.properties(sc)
		for _, field := range sbSchemaFields[name] {
			p, ok := props[field]
			if !ok {
				t.Errorf("schema %s: the type uses %q, which the document does not declare", name, field)
				continue
			}
			if p != nil && p.Deprecated {
				t.Errorf("schema %s: %q is deprecated", name, field)
			}
		}
	}

	// The deploy metadata is an inline object inside FunctionDeployBody.
	body := spec.properties(spec.Components.Schemas["FunctionDeployBody"])
	meta := spec.properties(body["metadata"])
	if len(meta) == 0 {
		t.Fatal("FunctionDeployBody.metadata declares no properties")
	}
	for _, field := range sbDeployMetadataFields {
		if _, ok := meta[field]; !ok {
			t.Errorf("FunctionDeployBody.metadata: the deploy sends %q, which the document does not declare", field)
		}
	}
	if file := spec.resolve(body["file"]); file == nil || file.Type != "array" {
		t.Errorf("FunctionDeployBody.file: expected an array of files, got %+v", file)
	}
}

// TestSupabase_SpecServer pins the host the url field defaults to.
func TestSupabase_SpecServer(t *testing.T) {
	spec := loadSupabaseSpec(t)
	for _, s := range spec.Servers {
		if strings.TrimSuffix(s.URL, "/") == "https://api.supabase.com" {
			return
		}
	}
	t.Errorf("https://api.supabase.com is not among the document's servers: %v", spec.Servers)
}

// TestSupabase_SpecRemovedLogsEndpoint makes sure the type never returns to
// logs.all, which the API answers with 410 since September 2026.
func TestSupabase_SpecRemovedLogsEndpoint(t *testing.T) {
	for key, ep := range sbEndpoints {
		if strings.HasSuffix(ep.Path, "logs.all") {
			t.Errorf("%s uses the removed logs.all endpoint", key)
		}
	}
}

// TestSupabase_SpecCoverage reports, without failing, which documented
// project paths the type does not use — the "not built yet" list the drift
// workflow prints.
func TestSupabase_SpecCoverage(t *testing.T) {
	spec := loadSupabaseSpec(t)
	used := map[string]bool{}
	for _, ep := range sbEndpoints {
		used[ep.Path] = true
	}
	var unused, other []string
	for p := range spec.Paths {
		if used[p] {
			continue
		}
		if strings.HasPrefix(p, "/v1/projects/{ref}/") && (strings.Contains(p, "/functions") || strings.Contains(p, "/secrets") || strings.Contains(p, "/database") || strings.Contains(p, "/analytics")) {
			unused = append(unused, p)
			continue
		}
		other = append(other, p)
	}
	sort.Strings(unused)
	t.Logf("used %d of %d documented paths; %d function/secret/database/analytics paths not covered:\n  %s\n(%d paths outside that area: organizations, auth, storage, branches, network, billing, OAuth)",
		len(used), len(spec.Paths), len(unused), strings.Join(unused, "\n  "), len(other))
}
