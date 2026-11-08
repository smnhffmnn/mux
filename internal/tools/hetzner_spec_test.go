package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

// The spec tests hold the hetzner-cloud type against Hetzner's own OpenAPI
// document, in both directions. Forward: every endpoint the registry builds
// must exist with its method and must not be deprecated, every query
// parameter must be documented, every body field the type documents must be
// a property of the operation's request body, every field a compact listing
// shows must resolve in the item's schema, and every filter value and record
// type the type offers must be in the document's enums. Backward: every
// operation of the document that is not deprecated must be reachable through
// the registry — that is the "complete client" claim, and it fails against
// the snapshot when it stops being true.
//
// The document is https://docs.hetzner.cloud/cloud.spec.json (OpenAPI 3.1),
// vendored (prose, examples and code samples stripped) under testdata by
// scripts/hetzner-openapi-snapshot.sh. Schemas are inline — the document has
// eight component schemas — so the checks walk the operations, not
// components. The drift workflow runs the same tests against the live
// document (HETZNER_OPENAPI=/path); there the coverage test only logs what
// is missing unless HETZNER_COVERAGE=strict.

const hcSpecSnapshot = "testdata/hetzner-openapi.json"

type hcSpec struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]*hcSpecSchema `json:"schemas"`
	} `json:"components"`
}

type hcSpecSchema struct {
	Ref        string                   `json:"$ref"`
	Type       any                      `json:"type"`
	AllOf      []*hcSpecSchema          `json:"allOf"`
	OneOf      []*hcSpecSchema          `json:"oneOf"`
	AnyOf      []*hcSpecSchema          `json:"anyOf"`
	Items      *hcSpecSchema            `json:"items"`
	Properties map[string]*hcSpecSchema `json:"properties"`
	Enum       []any                    `json:"enum"`
	Deprecated bool                     `json:"deprecated"`
}

type hcSpecParam struct {
	Name     string        `json:"name"`
	In       string        `json:"in"`
	Required bool          `json:"required"`
	Schema   *hcSpecSchema `json:"schema"`
}

type hcSpecOp struct {
	OperationID string        `json:"operationId"`
	Deprecated  bool          `json:"deprecated"`
	Parameters  []hcSpecParam `json:"parameters"`
	RequestBody struct {
		Content map[string]struct {
			Schema *hcSpecSchema `json:"schema"`
		} `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]struct {
			Schema *hcSpecSchema `json:"schema"`
		} `json:"content"`
	} `json:"responses"`
}

var hcSpecMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}

func loadHetznerSpec(t *testing.T) *hcSpec {
	t.Helper()
	path := os.Getenv("HETZNER_OPENAPI")
	if path == "" {
		path = hcSpecSnapshot
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read OpenAPI document %s: %v", path, err)
	}
	var spec hcSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse OpenAPI document %s: %v", path, err)
	}
	if len(spec.Paths) == 0 {
		t.Fatalf("OpenAPI document %s carries no paths", path)
	}
	ops := 0
	for _, item := range spec.Paths {
		for m := range item {
			if hcSpecMethods[m] {
				ops++
			}
		}
	}
	t.Logf("%s %s (OpenAPI %s) from %s: %d paths, %d operations", spec.Info.Title, spec.Info.Version, spec.OpenAPI, filepath.Base(path), len(spec.Paths), ops)
	return &spec
}

func hcSpecLive() bool { return os.Getenv("HETZNER_OPENAPI") != "" }

// op decodes the operation of a method on a path, with the path-level
// parameters merged in.
func (s *hcSpec) op(t *testing.T, method, path string) (*hcSpecOp, bool) {
	t.Helper()
	item, ok := s.Paths[path]
	if !ok {
		return nil, false
	}
	raw, ok := item[strings.ToLower(method)]
	if !ok {
		return nil, false
	}
	var op hcSpecOp
	if err := json.Unmarshal(raw, &op); err != nil {
		t.Fatalf("%s %s: cannot parse operation: %v", method, path, err)
	}
	if raw, ok := item["parameters"]; ok {
		var list []hcSpecParam
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("%s: cannot parse path parameters: %v", path, err)
		}
		op.Parameters = append(op.Parameters, list...)
	}
	return &op, true
}

func (op *hcSpecOp) queryParams() map[string]hcSpecParam {
	out := map[string]hcSpecParam{}
	for _, p := range op.Parameters {
		if p.In == "query" {
			out[p.Name] = p
		}
	}
	return out
}

func (op *hcSpecOp) bodySchema() *hcSpecSchema {
	if c, ok := op.RequestBody.Content["application/json"]; ok {
		return c.Schema
	}
	return nil
}

// responseSchema returns the JSON schema of the first 2xx response.
func (op *hcSpecOp) responseSchema() *hcSpecSchema {
	for _, code := range []string{"200", "201"} {
		if r, ok := op.Responses[code]; ok {
			if c, ok := r.Content["application/json"]; ok {
				return c.Schema
			}
		}
	}
	return nil
}

func (s *hcSpec) resolve(sc *hcSpecSchema) *hcSpecSchema {
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
func (s *hcSpec) properties(sc *hcSpecSchema) map[string]*hcSpecSchema {
	out := map[string]*hcSpecSchema{}
	var walk func(*hcSpecSchema, int)
	walk = func(sc *hcSpecSchema, depth int) {
		sc = s.resolve(sc)
		if sc == nil || depth > 10 {
			return
		}
		for k, v := range sc.Properties {
			if _, seen := out[k]; !seen {
				out[k] = v
			}
		}
		for _, list := range [][]*hcSpecSchema{sc.AllOf, sc.OneOf, sc.AnyOf} {
			for _, sub := range list {
				walk(sub, depth+1)
			}
		}
	}
	walk(sc, 0)
	return out
}

// items follows an array schema (possibly behind allOf/oneOf) to its items.
func (s *hcSpec) items(sc *hcSpecSchema) *hcSpecSchema {
	sc = s.resolve(sc)
	if sc == nil {
		return nil
	}
	if sc.Items != nil {
		return sc.Items
	}
	for _, list := range [][]*hcSpecSchema{sc.AllOf, sc.OneOf, sc.AnyOf} {
		for _, sub := range list {
			if it := s.items(sub); it != nil {
				return it
			}
		}
	}
	return nil
}

// enumValues collects the enum of a schema or of its items.
func (s *hcSpec) enumValues(sc *hcSpecSchema) map[string]bool {
	out := map[string]bool{}
	var walk func(*hcSpecSchema, int)
	walk = func(sc *hcSpecSchema, depth int) {
		sc = s.resolve(sc)
		if sc == nil || depth > 6 {
			return
		}
		for _, v := range sc.Enum {
			out[fmt.Sprint(v)] = true
		}
		if sc.Items != nil {
			walk(sc.Items, depth+1)
		}
		for _, list := range [][]*hcSpecSchema{sc.AllOf, sc.OneOf, sc.AnyOf} {
			for _, sub := range list {
				walk(sub, depth+1)
			}
		}
	}
	walk(sc, 0)
	return out
}

// resolvePath follows a dotted summary path into a schema: properties by
// name, arrays through their items.
func (s *hcSpec) resolvePath(sc *hcSpecSchema, path string) bool {
	cur := sc
	for _, seg := range strings.Split(path, ".") {
		props := s.properties(cur)
		next, ok := props[seg]
		if !ok {
			return false
		}
		cur = next
		if it := s.items(cur); it != nil {
			cur = it
		}
	}
	return true
}

func hcSpecKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestHetznerCloud_SpecEndpoints(t *testing.T) {
	spec := loadHetznerSpec(t)
	eps := hcEndpoints()
	for _, key := range hcSpecKeys(eps) {
		ep := eps[key]
		op, ok := spec.op(t, ep.Method, ep.Path)
		if !ok {
			t.Errorf("%s: %s %s is not in the OpenAPI document", key, ep.Method, ep.Path)
			continue
		}
		if op.Deprecated {
			t.Errorf("%s: %s %s is deprecated in the OpenAPI document — move the registry to its successor", key, ep.Method, ep.Path)
		}
		documented := op.queryParams()
		for _, p := range ep.Params {
			if _, ok := documented[p]; !ok {
				t.Errorf("%s: query parameter %q is not documented for %s %s (documented: %s)", key, p, ep.Method, ep.Path, strings.Join(hcSpecKeys(documented), ", "))
			}
		}
		// Lists page: the registry sends page and per_page on every list call
		// but the project-wide GET /actions, which takes ids only.
		if key != "actions.list" && (strings.HasSuffix(key, ".list") || strings.HasSuffix(key, ".actions") || strings.HasSuffix(key, ".actions.of")) {
			for _, p := range []string{"page", "per_page"} {
				if _, ok := documented[p]; !ok {
					t.Errorf("%s: %s %s does not document %s", key, ep.Method, ep.Path, p)
				}
			}
		}
	}
}

func TestHetznerCloud_SpecBodyFields(t *testing.T) {
	spec := loadHetznerSpec(t)
	eps := hcEndpoints()
	for _, key := range hcSpecKeys(eps) {
		ep := eps[key]
		if len(ep.Body) == 0 {
			continue
		}
		op, ok := spec.op(t, ep.Method, ep.Path)
		if !ok {
			continue // reported by TestHetznerCloud_SpecEndpoints
		}
		body := op.bodySchema()
		if body == nil {
			t.Errorf("%s: %s %s documents no JSON request body, the registry names %d fields", key, ep.Method, ep.Path, len(ep.Body))
			continue
		}
		props := spec.properties(body)
		for _, f := range ep.Body {
			if _, ok := props[f.Name]; !ok {
				t.Errorf("%s: body field %q is not in the request body of %s %s (documented: %s)", key, f.Name, ep.Method, ep.Path, strings.Join(hcSpecKeys(props), ", "))
			}
		}
	}
}

func TestHetznerCloud_SpecSummaryFields(t *testing.T) {
	spec := loadHetznerSpec(t)
	for i := range hcResources {
		r := &hcResources[i]
		op, ok := spec.op(t, http.MethodGet, "/"+r.Collection+"/{"+r.IDParam+"}")
		if !ok {
			t.Errorf("%s: GET item is not in the OpenAPI document", r.Kind)
			continue
		}
		resp := op.responseSchema()
		if resp == nil {
			t.Errorf("%s: GET item has no JSON response schema", r.Kind)
			continue
		}
		item, ok := spec.properties(resp)[r.Item]
		if !ok {
			t.Errorf("%s: the response of GET item has no %q wrapper (has: %s)", r.Kind, r.Item, strings.Join(hcSpecKeys(spec.properties(resp)), ", "))
			continue
		}
		for _, f := range r.Summary {
			_, path, found := strings.Cut(f, "=")
			if !found {
				path = f
			}
			if !spec.resolvePath(item, path) {
				t.Errorf("%s: summary field %q does not resolve in the %s schema", r.Kind, path, r.Item)
			}
		}
		// The list response wraps the items under the collection name.
		listOp, ok := spec.op(t, http.MethodGet, "/"+r.Collection)
		if !ok {
			continue
		}
		if _, ok := spec.properties(listOp.responseSchema())[r.Collection]; !ok {
			t.Errorf("%s: the list response has no %q wrapper", r.Kind, r.Collection)
		}
	}
}

func TestHetznerCloud_SpecFilterEnums(t *testing.T) {
	spec := loadHetznerSpec(t)
	for i := range hcResources {
		r := &hcResources[i]
		op, ok := spec.op(t, http.MethodGet, "/"+r.Collection)
		if !ok {
			continue
		}
		params := op.queryParams()
		for _, name := range hcSpecKeys(r.FilterEnums) {
			p, ok := params[name]
			if !ok {
				t.Errorf("%s: filter %q with documented values is not a query parameter", r.Kind, name)
				continue
			}
			allowed := spec.enumValues(p.Schema)
			if len(allowed) == 0 {
				t.Errorf("%s: filter %q has no enum in the document, the registry documents %v", r.Kind, name, r.FilterEnums[name])
				continue
			}
			for _, v := range r.FilterEnums[name] {
				if !allowed[v] {
					t.Errorf("%s: filter %s value %q is not in the document's enum (%s)", r.Kind, name, v, strings.Join(hcSpecKeys(allowed), ", "))
				}
			}
			for v := range allowed {
				found := false
				for _, have := range r.FilterEnums[name] {
					if have == v {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: filter %s has a documented value %q the registry does not mention", r.Kind, name, v)
				}
			}
		}
	}
	// Metric types and record types are enums too.
	for _, kind := range []string{"server", "load_balancer"} {
		r := hcResourceByKind(kind)
		op, ok := spec.op(t, http.MethodGet, "/"+r.Collection+"/{id}/metrics")
		if !ok {
			t.Errorf("%s: metrics endpoint missing", kind)
			continue
		}
		allowed := spec.enumValues(op.queryParams()["type"].Schema)
		for _, m := range r.Metrics {
			if !allowed[m] {
				t.Errorf("%s: metric type %q is not documented (%s)", kind, m, strings.Join(hcSpecKeys(allowed), ", "))
			}
		}
		if len(allowed) != len(r.Metrics) {
			t.Errorf("%s: the document knows %d metric types, the registry %d", kind, len(allowed), len(r.Metrics))
		}
	}
	op, ok := spec.op(t, http.MethodPost, hcZonePath+"/rrsets")
	if !ok {
		t.Fatal("POST rrsets missing")
	}
	allowed := spec.enumValues(spec.properties(op.bodySchema())["type"])
	for _, typ := range hcRRSetTypes {
		if !allowed[typ] {
			t.Errorf("record type %q is not in the document (%s)", typ, strings.Join(hcSpecKeys(allowed), ", "))
		}
	}
	if len(allowed) != len(hcRRSetTypes) {
		t.Errorf("the document knows %d record types, the type %d: %s", len(allowed), len(hcRRSetTypes), strings.Join(hcSpecKeys(allowed), ", "))
	}
}

func TestHetznerCloud_SpecResponseExtras(t *testing.T) {
	spec := loadHetznerSpec(t)
	for i := range hcResources {
		r := &hcResources[i]
		if r.Create != nil {
			op, ok := spec.op(t, http.MethodPost, "/"+r.Collection)
			if ok {
				props := spec.properties(op.responseSchema())
				for _, k := range append([]string{r.Item}, r.CreateExtra...) {
					if _, ok := props[k]; !ok {
						t.Errorf("%s: create response has no %q (has: %s)", r.Kind, k, strings.Join(hcSpecKeys(props), ", "))
					}
				}
			}
		}
		for _, a := range r.Actions {
			op, ok := spec.op(t, http.MethodPost, "/"+r.Collection+"/{"+r.IDParam+"}/actions/"+a.Name)
			if !ok {
				continue
			}
			props := spec.properties(op.responseSchema())
			for _, k := range a.Extra {
				if _, ok := props[k]; !ok {
					t.Errorf("%s %s: response has no %q (has: %s)", r.Kind, a.Name, k, strings.Join(hcSpecKeys(props), ", "))
				}
			}
			_, hasAction := props["action"]
			_, hasActions := props["actions"]
			if !hasAction && !hasActions {
				t.Errorf("%s %s: response carries neither action nor actions (has: %s)", r.Kind, a.Name, strings.Join(hcSpecKeys(props), ", "))
			}
		}
		if r.Deletable {
			op, ok := spec.op(t, http.MethodDelete, "/"+r.Collection+"/{"+r.IDParam+"}")
			if ok {
				_, hasAction := spec.properties(op.responseSchema())["action"]
				if hasAction != r.DeleteAction {
					t.Errorf("%s: DeleteAction = %v, the document says %v", r.Kind, r.DeleteAction, hasAction)
				}
			}
		}
	}
}

// TestHetznerCloud_SpecCoverage is the "complete client" claim: every
// operation of the document that is not deprecated is in the registry.
// Against the live document a gap is logged, not failed, unless
// HETZNER_COVERAGE=strict — new API surface is Hetzner's news, not mux's
// defect; the drift report lists it as not built yet.
func TestHetznerCloud_SpecCoverage(t *testing.T) {
	spec := loadHetznerSpec(t)
	have := map[string]bool{}
	for _, ep := range hcEndpoints() {
		have[ep.Method+" "+ep.Path] = true
	}
	var missing, deprecated []string
	for _, path := range hcSpecKeys(spec.Paths) {
		for method := range spec.Paths[path] {
			if !hcSpecMethods[method] {
				continue
			}
			op, _ := spec.op(t, method, path)
			key := strings.ToUpper(method) + " " + path
			if op.Deprecated {
				deprecated = append(deprecated, key)
				if have[key] {
					t.Errorf("%s is deprecated and still in the registry", key)
				}
				continue
			}
			if !have[key] {
				missing = append(missing, key)
			}
		}
	}
	sort.Strings(deprecated)
	t.Logf("%d operations deprecated in the document, none registered: %s", len(deprecated), strings.Join(deprecated, ", "))
	if len(missing) == 0 {
		t.Logf("every non-deprecated operation of the document is reachable through the registry (%d endpoints)", len(have))
		return
	}
	sort.Strings(missing)
	msg := fmt.Sprintf("%d operations of the document are not in the registry (not built yet):\n  %s", len(missing), strings.Join(missing, "\n  "))
	if hcSpecLive() && os.Getenv("HETZNER_COVERAGE") != "strict" {
		t.Log(msg)
		return
	}
	t.Error(msg)
}

// TestHetznerCloud_KindsAgree pins the kinds the config package validates
// protected_resources entries against to the registry.
func TestHetznerCloud_KindsAgree(t *testing.T) {
	kinds := hcKinds(nil)
	if strings.Join(kinds, ",") != strings.Join(config.HetznerCloudKinds, ",") {
		t.Errorf("config.HetznerCloudKinds = %v, registry = %v", config.HetznerCloudKinds, kinds)
	}
}

// TestHetznerCloud_RegistryClasses pins the guard design: every delete is a
// lifecycle operation, creating a billed resource is, and no action of a
// record set needs lifecycle (DNS records are the DNS connection's job).
func TestHetznerCloud_RegistryClasses(t *testing.T) {
	billed := map[string]bool{"server": true, "volume": true, "floating_ip": true, "primary_ip": true, "load_balancer": true}
	for i := range hcResources {
		r := &hcResources[i]
		if r.Create != nil && billed[r.Kind] != (r.CreateClass == hcLifecycle) {
			t.Errorf("%s: CreateClass = %s, billed = %v", r.Kind, r.CreateClass, billed[r.Kind])
		}
		for _, a := range r.Actions {
			switch a.Name {
			case "rebuild", "change_type", "poweroff", "reset", "create_image", "enable_backup", "disable_backup", "resize", "delete_subnet", "import_zonefile":
				if a.Class != hcLifecycle {
					t.Errorf("%s %s should be a lifecycle operation", r.Kind, a.Name)
				}
			case "reboot", "shutdown", "poweron", "change_protection", "change_dns_ptr", "attach", "detach", "set_rules", "set_records":
				if a.Class != hcWrite {
					t.Errorf("%s %s should be a plain write", r.Kind, a.Name)
				}
			}
		}
	}
	for _, a := range hcRRSetActions {
		if a.Class != hcWrite {
			t.Errorf("rrset %s should be a plain write", a.Name)
		}
	}
	eps := hcEndpoints()
	for key, ep := range eps {
		if ep.Method == http.MethodDelete && !strings.HasPrefix(key, "rrset.") && ep.Class != hcLifecycle {
			t.Errorf("%s: a delete is a lifecycle operation", key)
		}
		if ep.Method == http.MethodGet && ep.Class != hcRead {
			t.Errorf("%s: a GET is a read", key)
		}
	}
}
