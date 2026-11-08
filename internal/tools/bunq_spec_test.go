package tools

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// The spec tests hold the bunq type against bunq's own OpenAPI document:
// every registered endpoint must exist with its method, and every field the
// type decodes or sends from the component schemas listed in
// bunqSchemaFields must be a property of that schema. bunq's document
// describes no query parameters, so the Params of the registry are checked
// at the call only.
// The document is https://github.com/bunq/doc (swagger.json, OpenAPI 3.0),
// vendored (prose and examples stripped) under testdata by
// scripts/bunq-openapi-snapshot.sh. The drift workflow runs the same tests
// against the live document (BUNQ_OPENAPI=/path).

const bunqSpecSnapshot = "testdata/bunq-openapi.json"

func loadBunqSpec(t *testing.T) *nylasSpec {
	t.Helper()
	path := os.Getenv("BUNQ_OPENAPI")
	if path == "" {
		path = bunqSpecSnapshot
	}
	return loadSpecFile(t, path, "bunq")
}

func TestBunq_SpecEndpoints(t *testing.T) {
	spec := loadBunqSpec(t)
	keys := make([]string, 0, len(bunqEndpoints))
	for k := range bunqEndpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ep := bunqEndpoints[key]
		ops, ok := spec.Paths[ep.Path]
		if !ok {
			t.Errorf("%s: path %s is not in the OpenAPI document", key, ep.Path)
			continue
		}
		if _, ok := ops[strings.ToLower(ep.Method)]; !ok {
			t.Errorf("%s: %s %s is not in the OpenAPI document", key, ep.Method, ep.Path)
		}
	}
}

func TestBunq_SpecFields(t *testing.T) {
	spec := loadBunqSpec(t)
	names := make([]string, 0, len(bunqSchemaFields))
	for name := range bunqSchemaFields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc, ok := spec.Components.Schemas[name]
		if !ok {
			t.Errorf("schema %s is not in the OpenAPI document", name)
			continue
		}
		props := spec.properties(sc)
		for _, field := range bunqSchemaFields[name] {
			if !props[field] {
				t.Errorf("schema %s has no property %q", name, field)
			}
		}
	}
}

// TestBunq_SpecPaymentNotRegistered pins the design: the registry has no
// endpoint that moves money without the owner's acceptance.
func TestBunq_SpecPaymentNotRegistered(t *testing.T) {
	for key, ep := range bunqEndpoints {
		if ep.Method == "GET" {
			continue
		}
		last := ep.Path[strings.LastIndex(ep.Path, "/")+1:]
		switch last {
		case "installation", "device-server", "session-server", "draft-payment", "customer-statement":
		default:
			t.Errorf("%s: %s %s writes a resource the type must not write", key, ep.Method, ep.Path)
		}
	}
	for _, p := range []string{
		"/v1/user/1/monetary-account/2/payment",
		"/v1/user/{user}/monetary-account/2/payment-batch",
		"/v1/user/1/monetary-account/2/request-inquiry",
		"/v1/user/1/monetary-account/2/schedule-payment",
		"/v1/user/1/card/3",
		"/v1/user/1/monetary-account/2/payment/5/note-text", // a note, but under payment
		"/v1/user/1",
		"/v1/user/1/monetary-account-bank/2", // the daily limit lives here; raising it is the app's job
		"/v1/user/1/monetary-account/2",
	} {
		if r, ok := bunqWriteAllowed(p); ok {
			t.Errorf("bunqWriteAllowed(%s) = %q, true; want refused", p, r)
		}
	}
	for _, p := range []string{
		"/v1/user/1/monetary-account/2/draft-payment",
		"/v1/user/{user}/monetary-account/2/draft-payment/7",
		"/v1/user/1/monetary-account/2/draft-payment/7/note-text",
		"/v1/user/1/monetary-account/2/customer-statement/9",
		"/v1/user/1/export-annual-overview",
	} {
		if _, ok := bunqWriteAllowed(p); !ok {
			t.Errorf("bunqWriteAllowed(%s) refused; want allowed", p)
		}
	}
}
