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

// The spec tests hold the nylas type against Nylas' own OpenAPI document:
// every registered endpoint must exist with its method and query parameters,
// and every field the type decodes must be a property of the schema it reads.
// The document is Nylas' published https://developer.nylas.com/openapi.json,
// vendored (prose and examples stripped) under testdata by
// scripts/nylas-openapi-snapshot.sh. The drift workflow runs the same tests
// against the live document (NYLAS_OPENAPI=/path), so an upstream rename
// fails a test before it fails a user.

const nylasSpecSnapshot = "testdata/nylas-openapi.json"

type nylasSpec struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas    map[string]*nylasSpecSchema `json:"schemas"`
		Parameters map[string]nylasSpecParam   `json:"parameters"`
	} `json:"components"`
}

type nylasSpecSchema struct {
	Ref        string                      `json:"$ref"`
	Type       any                         `json:"type"`
	AllOf      []*nylasSpecSchema          `json:"allOf"`
	OneOf      []*nylasSpecSchema          `json:"oneOf"`
	AnyOf      []*nylasSpecSchema          `json:"anyOf"`
	Properties map[string]*nylasSpecSchema `json:"properties"`
}

type nylasSpecParam struct {
	Ref  string `json:"$ref"`
	Name string `json:"name"`
	In   string `json:"in"`
}

func loadNylasSpec(t *testing.T) *nylasSpec {
	t.Helper()
	path := os.Getenv("NYLAS_OPENAPI")
	if path == "" {
		path = nylasSpecSnapshot
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OpenAPI document %s: %v", path, err)
	}
	var spec nylasSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse OpenAPI document %s: %v", path, err)
	}
	if len(spec.Paths) == 0 || len(spec.Components.Schemas) == 0 {
		t.Fatalf("OpenAPI document %s carries no paths or schemas", path)
	}
	t.Logf("Nylas OpenAPI %s from %s: %d paths, %d schemas", spec.Info.Version, filepath.Base(path), len(spec.Paths), len(spec.Components.Schemas))
	return &spec
}

// resolve follows $ref into components.schemas.
func (s *nylasSpec) resolve(sc *nylasSpecSchema) *nylasSpecSchema {
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
// allOf/oneOf/anyOf — the union of what an instance can carry.
func (s *nylasSpec) properties(sc *nylasSpecSchema) map[string]bool {
	out := map[string]bool{}
	var walk func(*nylasSpecSchema, int)
	walk = func(sc *nylasSpecSchema, depth int) {
		sc = s.resolve(sc)
		if sc == nil || depth > 10 {
			return
		}
		for k := range sc.Properties {
			out[k] = true
		}
		for _, list := range [][]*nylasSpecSchema{sc.AllOf, sc.OneOf, sc.AnyOf} {
			for _, sub := range list {
				walk(sub, depth+1)
			}
		}
	}
	walk(sc, 0)
	return out
}

// resolveParam follows $ref into components.parameters.
func (s *nylasSpec) resolveParam(p nylasSpecParam) nylasSpecParam {
	const prefix = "#/components/parameters/"
	if p.Ref != "" && strings.HasPrefix(p.Ref, prefix) {
		if r, ok := s.Components.Parameters[strings.TrimPrefix(p.Ref, prefix)]; ok {
			return r
		}
	}
	return p
}

// queryParams returns the documented query parameters of an operation,
// path-level parameters included.
func (s *nylasSpec) queryParams(t *testing.T, where string, pathItem map[string]json.RawMessage, rawOp json.RawMessage) map[string]bool {
	params := map[string]bool{}
	collect := func(raw json.RawMessage) {
		var list []nylasSpecParam
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Errorf("%s: cannot parse parameters: %v", where, err)
			return
		}
		for _, p := range list {
			p = s.resolveParam(p)
			if p.In == "query" {
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

func TestNylas_SpecEndpoints(t *testing.T) {
	spec := loadNylasSpec(t)

	keys := make([]string, 0, len(nylasEndpoints))
	for k := range nylasEndpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		ep := nylasEndpoints[key]
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
		params := spec.queryParams(t, where, pathItem, rawOp)
		for _, p := range ep.Params {
			if !params[p] {
				t.Errorf("%s: query parameter %q is not documented for this operation", where, p)
			}
		}
	}
}

func TestNylas_SpecFields(t *testing.T) {
	spec := loadNylasSpec(t)

	names := make([]string, 0, len(nylasSchemaFields))
	for k := range nylasSchemaFields {
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
		for _, field := range nylasSchemaFields[name] {
			if !props[field] {
				t.Errorf("schema %s: the type reads %q, which the document does not declare", name, field)
			}
		}
	}
}

// TestNylas_SpecServers pins the hosts the region field maps to.
func TestNylas_SpecServers(t *testing.T) {
	spec := loadNylasSpec(t)
	documented := map[string]bool{}
	for _, s := range spec.Servers {
		documented[strings.TrimSuffix(s.URL, "/")] = true
	}
	for _, region := range []string{"us", "eu"} {
		want := "https://api." + region + ".nylas.com"
		if !documented[want] {
			t.Errorf("server %s (region %s) is not in the OpenAPI document's servers: %v", want, region, spec.Servers)
		}
	}
}

// TestNylas_SpecCoverage reports, without failing, which documented mail
// paths the type does not use — the "not built yet" list the drift workflow
// prints. It is a log, not a judgement: the type covers what agents need,
// not the whole API.
func TestNylas_SpecCoverage(t *testing.T) {
	spec := loadNylasSpec(t)
	used := map[string]bool{}
	for _, ep := range nylasEndpoints {
		used[ep.Path] = true
	}
	var unused, other []string
	for p := range spec.Paths {
		if used[p] {
			continue
		}
		if strings.HasPrefix(p, "/v3/grants/{grant_id}/") && (strings.Contains(p, "/messages") || strings.Contains(p, "/threads") || strings.Contains(p, "/drafts") || strings.Contains(p, "/folders") || strings.Contains(p, "/attachments")) {
			unused = append(unused, p)
			continue
		}
		other = append(other, p)
	}
	sort.Strings(unused)
	t.Logf("used %d of %d documented paths; %d mail paths not covered:\n  %s\n(%d paths outside mail: calendar, contacts, scheduler, notetaker, auth, admin)",
		len(used), len(spec.Paths), len(unused), strings.Join(unused, "\n  "), len(other))
}
