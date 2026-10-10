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

// The spec tests hold the YouTrack REST type against YouTrack's own OpenAPI
// document: every registered endpoint must exist with its method and query
// parameters, and every fields= selector may only name properties the entity
// schemas know. The document is YouTrack's published /api/openapi.json,
// vendored (descriptions stripped) under testdata by scripts/youtrack-openapi-
// snapshot.sh. The drift workflow runs the same tests against the live
// document (YOUTRACK_OPENAPI=/path), so an upstream rename fails a test
// before it fails a user.

const ytSpecSnapshot = "testdata/youtrack-openapi.json"

type ytSpec struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]*ytSpecSchema `json:"schemas"`
	} `json:"components"`
}

type ytSpecSchema struct {
	Type       string                   `json:"type"`
	Ref        string                   `json:"$ref"`
	Items      *ytSpecSchema            `json:"items"`
	AllOf      []*ytSpecSchema          `json:"allOf"`
	Properties map[string]*ytSpecSchema `json:"properties"`
}

type ytSpecParameter struct {
	Name string `json:"name"`
	In   string `json:"in"`
}

type ytSpecOperation struct {
	Parameters []ytSpecParameter `json:"parameters"`
	Responses  map[string]struct {
		Content map[string]struct {
			Schema *ytSpecSchema `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

func loadYouTrackSpec(t *testing.T) *ytSpec {
	t.Helper()
	path := os.Getenv("YOUTRACK_OPENAPI")
	if path == "" {
		path = ytSpecSnapshot
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OpenAPI document %s: %v", path, err)
	}
	var spec ytSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse OpenAPI document %s: %v", path, err)
	}
	if len(spec.Paths) == 0 || len(spec.Components.Schemas) == 0 {
		t.Fatalf("OpenAPI document %s carries no paths or schemas", path)
	}
	t.Logf("YouTrack OpenAPI %s from %s: %d paths, %d schemas", spec.Info.Version, filepath.Base(path), len(spec.Paths), len(spec.Components.Schemas))
	return &spec
}

// --- schema index with subtype unions ---

type ytSpecIndex struct {
	schemas  map[string]*ytSpecSchema
	parents  map[string][]string // schema → schemas it extends (allOf $ref)
	children map[string][]string // schema → schemas extending it
}

func newYTSpecIndex(spec *ytSpec) *ytSpecIndex {
	ix := &ytSpecIndex{schemas: spec.Components.Schemas, parents: map[string][]string{}, children: map[string][]string{}}
	for name, s := range spec.Components.Schemas {
		for _, a := range s.AllOf {
			if p := refName(a.Ref); p != "" {
				ix.parents[name] = append(ix.parents[name], p)
				ix.children[p] = append(ix.children[p], name)
			}
		}
	}
	return ix
}

func refName(ref string) string {
	const prefix = "#/components/schemas/"
	if strings.HasPrefix(ref, prefix) {
		return strings.TrimPrefix(ref, prefix)
	}
	return ""
}

// family returns a schema together with all its ancestors and descendants —
// the set of properties a polymorphic value of that declared type can carry.
func (ix *ytSpecIndex) family(name string) []string {
	seen := map[string]bool{}
	var out []string
	var up, down func(string)
	up = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
		for _, p := range ix.parents[n] {
			up(p)
		}
	}
	down = func(n string) {
		for _, c := range ix.children[n] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				// A child's other ancestors do not belong to the family.
			}
			down(c)
		}
	}
	up(name)
	down(name)
	return out
}

// ownProperties returns a schema's directly declared properties, including
// those of inline allOf objects.
func ownProperties(s *ytSpecSchema) map[string]*ytSpecSchema {
	out := map[string]*ytSpecSchema{}
	if s == nil {
		return out
	}
	for k, v := range s.Properties {
		out[k] = v
	}
	for _, a := range s.AllOf {
		if a.Ref == "" {
			for k, v := range a.Properties {
				out[k] = v
			}
		}
	}
	return out
}

// propertyTypes returns the declared types (schema names) a property has
// across a family, and whether any member declares it at all.
func (ix *ytSpecIndex) propertyTypes(family []string, prop string) (refs []string, declared bool) {
	seen := map[string]bool{}
	for _, name := range family {
		p, ok := ownProperties(ix.schemas[name])[prop]
		if !ok {
			continue
		}
		declared = true
		r := refName(p.Ref)
		if r == "" && p.Items != nil {
			r = refName(p.Items.Ref)
		}
		if r != "" && !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	sort.Strings(refs)
	return refs, declared
}

// --- fields= selector grammar: a,b(c,d(e)),f ---

type ytSelNode struct {
	Name     string
	Children []ytSelNode
}

func parseFieldSelector(s string) ([]ytSelNode, error) {
	nodes, rest, err := parseSelList(s)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rest) != "" {
		return nil, fmt.Errorf("unexpected %q after selector", rest)
	}
	return nodes, nil
}

func parseSelList(s string) ([]ytSelNode, string, error) {
	var nodes []ytSelNode
	for {
		s = strings.TrimLeft(s, " ")
		i := 0
		for i < len(s) && s[i] != ',' && s[i] != '(' && s[i] != ')' {
			i++
		}
		name := strings.TrimSpace(s[:i])
		if name == "" {
			return nil, "", fmt.Errorf("empty field name in selector near %q", s)
		}
		node := ytSelNode{Name: name}
		s = s[i:]
		if strings.HasPrefix(s, "(") {
			children, rest, err := parseSelList(s[1:])
			if err != nil {
				return nil, "", err
			}
			if !strings.HasPrefix(rest, ")") {
				return nil, "", fmt.Errorf("missing ) after %s", name)
			}
			node.Children = children
			s = rest[1:]
		}
		nodes = append(nodes, node)
		if strings.HasPrefix(s, ",") {
			s = s[1:]
			continue
		}
		return nodes, s, nil
	}
}

// checkSelector validates selector nodes against the family of a declared
// schema. Children are checked against the union of every type the property
// is declared with across the family (custom field values are polymorphic);
// a property whose types are all opaque (object/array without $ref) accepts
// any sub-selection; a property nobody in the family declares is an error.
func (ix *ytSpecIndex) checkSelector(t *testing.T, where string, schema string, nodes []ytSelNode) {
	ix.checkNodes(t, where, schema, ix.family(schema), nodes)
}

func (ix *ytSpecIndex) checkNodes(t *testing.T, where, typeName string, union []string, nodes []ytSelNode) {
	for _, n := range nodes {
		refs, declared := ix.propertyTypes(union, n.Name)
		if !declared {
			t.Errorf("%s: selector names %q, which no type of %s declares", where, n.Name, typeName)
			continue
		}
		if len(n.Children) == 0 || len(refs) == 0 {
			continue // leaf, or opaque object: the API decides, nothing to check
		}
		var childUnion []string
		for _, r := range refs {
			childUnion = append(childUnion, ix.family(r)...)
		}
		ix.checkNodes(t, where+"."+n.Name, strings.Join(refs, "|"), childUnion, n.Children)
	}
}

// responseSchema finds the schema name an operation returns (array element
// for lists).
func responseSchema(op ytSpecOperation) string {
	for _, code := range []string{"200", "201"} {
		r, ok := op.Responses[code]
		if !ok {
			continue
		}
		for _, c := range r.Content {
			if c.Schema == nil {
				continue
			}
			if n := refName(c.Schema.Ref); n != "" {
				return n
			}
			if c.Schema.Items != nil {
				if n := refName(c.Schema.Items.Ref); n != "" {
					return n
				}
			}
		}
	}
	return ""
}

func TestYouTrackREST_SpecEndpoints(t *testing.T) {
	spec := loadYouTrackSpec(t)
	ix := newYTSpecIndex(spec)

	keys := make([]string, 0, len(ytEndpoints))
	for k := range ytEndpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		ep := ytEndpoints[key]
		if ep.Raw {
			continue
		}
		where := fmt.Sprintf("%s (%s %s)", key, ep.Method, ep.Path)
		pathItem, exists := spec.Paths[ep.Path]
		if ep.Undocumented {
			if exists {
				t.Errorf("%s: marked Undocumented but the OpenAPI document now lists it — drop the flag and validate its selector", where)
			}
			continue
		}
		if !exists {
			t.Errorf("%s: path is not in the OpenAPI document", where)
			continue
		}
		rawOp, ok := pathItem[strings.ToLower(ep.Method)]
		if !ok {
			t.Errorf("%s: method not offered on this path (has %s)", where, pathMethods(pathItem))
			continue
		}
		var op ytSpecOperation
		if err := json.Unmarshal(rawOp, &op); err != nil {
			t.Errorf("%s: cannot parse operation: %v", where, err)
			continue
		}
		params := map[string]bool{}
		for _, p := range op.Parameters {
			params[p.Name] = true
		}
		if rawPathParams, ok := pathItem["parameters"]; ok {
			var pp []ytSpecParameter
			if json.Unmarshal(rawPathParams, &pp) == nil {
				for _, p := range pp {
					params[p.Name] = true
				}
			}
		}
		for _, p := range ep.Params {
			if !params[p] {
				t.Errorf("%s: query parameter %q is not documented for this operation", where, p)
			}
		}
		for _, p := range ep.UndocumentedParams {
			if params[p] {
				t.Errorf("%s: query parameter %q is now documented — move it from UndocumentedParams to Params", where, p)
			}
		}
		if ep.Fields != "" {
			if !params["fields"] {
				t.Errorf("%s: sends fields= but the operation documents no fields parameter", where)
			}
			schema := responseSchema(op)
			if schema == "" {
				t.Errorf("%s: cannot determine the response schema to validate the selector against", where)
				continue
			}
			nodes, err := parseFieldSelector(ep.Fields)
			if err != nil {
				t.Errorf("%s: selector does not parse: %v", where, err)
				continue
			}
			ix.checkSelector(t, where, schema, nodes)
		}
	}
}

func pathMethods(item map[string]json.RawMessage) string {
	var ms []string
	for k := range item {
		switch k {
		case "get", "post", "put", "patch", "delete":
			ms = append(ms, strings.ToUpper(k))
		}
	}
	sort.Strings(ms)
	return strings.Join(ms, ", ")
}

// TestYouTrackREST_SpecCoverage reports, without failing, which documented
// paths the type does not use — the "not built yet" list the drift workflow
// prints. It is a log, not a judgement: the type covers what agents need,
// not the whole API.
func TestYouTrackREST_SpecCoverage(t *testing.T) {
	spec := loadYouTrackSpec(t)
	used := map[string]bool{}
	for _, ep := range ytEndpoints {
		if !ep.Raw {
			used[ep.Path] = true
		}
	}
	var unused, admin []string
	for p := range spec.Paths {
		if used[p] {
			continue
		}
		if strings.HasPrefix(p, "/admin/") {
			admin = append(admin, p)
			continue
		}
		unused = append(unused, p)
	}
	sort.Strings(unused)
	sort.Strings(admin)
	t.Logf("used %d of %d documented paths; %d admin paths reachable through admin_get/admin_request; %d other paths not covered:\n  %s",
		len(used), len(spec.Paths), len(admin), len(unused), strings.Join(unused, "\n  "))
}

func TestYouTrackREST_SelectorParser(t *testing.T) {
	nodes, err := parseFieldSelector("id,customFields(name,value(login,$type)),project(shortName)")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 || nodes[1].Name != "customFields" || len(nodes[1].Children) != 2 || nodes[1].Children[1].Children[1].Name != "$type" {
		t.Errorf("parsed %+v", nodes)
	}
	for _, bad := range []string{"id,", "a(b", "a)b", ",a"} {
		if _, err := parseFieldSelector(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}
