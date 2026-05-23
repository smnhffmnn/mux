package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPickPageTarget_FirstPageWinsWithoutPattern(t *testing.T) {
	targets := []cdpTargetInfo{
		{TargetID: "iframe-1", Type: "iframe", URL: "https://ads.example.com"},
		{TargetID: "page-a", Type: "page", URL: "https://search.example.com/results"},
		{TargetID: "page-b", Type: "page", URL: "https://www.example.com/home"},
	}
	got, _ := pickPageTarget(targets, "")
	if got == nil {
		t.Fatal("expected a page target")
	}
	if got.TargetID != "page-a" {
		t.Errorf("expected first page target page-a, got %s", got.TargetID)
	}
}

func TestPickPageTarget_PatternFiltersByURLSubstring(t *testing.T) {
	targets := []cdpTargetInfo{
		{TargetID: "page-a", Type: "page", URL: "https://www.bing.com"},
		{TargetID: "page-b", Type: "page", URL: "https://www.example.com/home"},
	}
	got, _ := pickPageTarget(targets, "example.com")
	if got == nil {
		t.Fatal("expected a page target")
	}
	if got.TargetID != "page-b" {
		t.Errorf("pattern should have skipped bing, got %s", got.TargetID)
	}
}

func TestPickPageTarget_NilWhenNoMatch(t *testing.T) {
	targets := []cdpTargetInfo{
		{TargetID: "page-a", Type: "page", URL: "https://www.bing.com"},
	}
	got, pages := pickPageTarget(targets, "example.com")
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
	if len(pages) != 1 || pages[0] != "https://www.bing.com" {
		t.Errorf("expected available-pages slice to surface candidate URLs, got %v", pages)
	}
}

func TestPickPageTarget_IgnoresNonPageTypes(t *testing.T) {
	targets := []cdpTargetInfo{
		{TargetID: "sw-1", Type: "service_worker", URL: "https://example.com/sw.js"},
		{TargetID: "iframe-1", Type: "iframe", URL: "https://example.com/embed"},
	}
	got, pages := pickPageTarget(targets, "example.com")
	if got != nil {
		t.Error("expected nil — only non-page targets available")
	}
	if len(pages) != 0 {
		t.Errorf("expected no page candidates, got %v", pages)
	}
}

func TestDetectResultType_SubtypeWins(t *testing.T) {
	cases := []struct {
		value, cdpType, cdpSubtype, want string
	}{
		{`[1,2,3]`, "object", "array", "array"},
		{`null`, "object", "null", "null"},
	}
	for _, c := range cases {
		got := detectResultType([]byte(c.value), c.cdpType, c.cdpSubtype)
		if got != c.want {
			t.Errorf("value=%s type=%s subtype=%s → %s, want %s", c.value, c.cdpType, c.cdpSubtype, got, c.want)
		}
	}
}

func TestDetectResultType_ValueByteFallback(t *testing.T) {
	// Hyperbrowser/Chrome with returnByValue=true sometimes returns
	// {type: "object", subtype: ""} even for arrays. The value-byte
	// fallback must catch these.
	cases := []struct {
		value, cdpType, cdpSubtype, want string
	}{
		{`[1,2,3]`, "object", "", "array"},
		{`[]`, "object", "", "array"},
		{`{"a":1}`, "object", "", "object"},
		{`"hi"`, "object", "", "string"},
		{`42`, "object", "", "number"},
		{`-3.14`, "object", "", "number"},
		{`true`, "object", "", "boolean"},
		{`false`, "object", "", "boolean"},
		{`null`, "object", "", "null"},
	}
	for _, c := range cases {
		got := detectResultType([]byte(c.value), c.cdpType, c.cdpSubtype)
		if got != c.want {
			t.Errorf("value=%s → %s, want %s", c.value, got, c.want)
		}
	}
}

func TestDetectResultType_ScalarTypesTrustCDP(t *testing.T) {
	cases := []struct {
		value, cdpType, want string
	}{
		{`"hello"`, "string", "string"},
		{`42`, "number", "number"},
		{`true`, "boolean", "boolean"},
		{``, "undefined", "undefined"},
		{``, "function", "function"},
	}
	for _, c := range cases {
		got := detectResultType([]byte(c.value), c.cdpType, "")
		if got != c.want {
			t.Errorf("value=%q type=%s → %s, want %s", c.value, c.cdpType, got, c.want)
		}
	}
}

func TestMapCDPResultType(t *testing.T) {
	cases := []struct {
		t, subtype, want string
	}{
		{"string", "", "string"},
		{"number", "", "number"},
		{"boolean", "", "boolean"},
		{"undefined", "", "undefined"},
		{"object", "", "object"},
		{"object", "array", "array"},
		{"object", "null", "null"},
		{"object", "regexp", "object"}, // subtype not in allowlist → stays "object"
		{"function", "", "function"},
	}
	for _, c := range cases {
		got := mapCDPResultType(c.t, c.subtype)
		if got != c.want {
			t.Errorf("mapCDPResultType(%q,%q)=%q, want %q", c.t, c.subtype, got, c.want)
		}
	}
}

func TestSummariseExpression_TruncatesAndNormalisesWhitespace(t *testing.T) {
	long := strings.Repeat("abc ", 50) // 200 chars
	got := summariseExpression(long)
	if len(got) > 90 { // 80 + ellipsis allowance
		t.Errorf("summary not truncated: %d chars", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis marker, got %q", got)
	}

	// Whitespace collapse: multi-line expressions get folded.
	got2 := summariseExpression("document\n  .title")
	if strings.Contains(got2, "\n") {
		t.Errorf("newlines not collapsed: %q", got2)
	}
}

// TestCDPEvaluateResult_MarshalShape locks in the wire format the briefing
// asked for: {result, type, execution_time_ms} on success, {error: {...}}
// on exception. Anything else and downstream agents will break.
func TestCDPEvaluateResult_MarshalShape(t *testing.T) {
	success := cdpEvaluateResult{
		Result:          json.RawMessage(`"Example Domain"`),
		Type:            "string",
		ExecutionTimeMs: 42,
		Target:          &cdpTargetSummary{TargetID: "t-1", URL: "https://example.com", Title: "Example"},
	}
	b, err := json.Marshal(success)
	if err != nil {
		t.Fatalf("marshal success: %v", err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(b, &roundtrip); err != nil {
		t.Fatalf("unmarshal success: %v", err)
	}
	for _, k := range []string{"result", "type", "execution_time_ms", "target"} {
		if _, ok := roundtrip[k]; !ok {
			t.Errorf("success: missing key %q", k)
		}
	}
	if _, ok := roundtrip["error"]; ok {
		t.Errorf("success: unexpected error key")
	}

	failure := cdpEvaluateResult{
		ExecutionTimeMs: 5,
		Error: &cdpEvaluateError{
			Message: "nonexistent is not defined",
			Type:    "ReferenceError",
			Stack:   "    at <anonymous>:1:1",
		},
	}
	b, err = json.Marshal(failure)
	if err != nil {
		t.Fatalf("marshal failure: %v", err)
	}
	var roundtrip2 map[string]any
	if err := json.Unmarshal(b, &roundtrip2); err != nil {
		t.Fatalf("unmarshal failure: %v", err)
	}
	errObj, ok := roundtrip2["error"].(map[string]any)
	if !ok {
		t.Fatalf("failure: error key missing or wrong type")
	}
	for _, k := range []string{"message", "type", "stack"} {
		if _, ok := errObj[k]; !ok {
			t.Errorf("failure: error.%s missing", k)
		}
	}
	// result/type must be absent on the error path so consumers can switch
	// on "error" key presence without ambiguity.
	if _, ok := roundtrip2["result"]; ok {
		t.Errorf("failure: result should be absent, got %v", roundtrip2["result"])
	}
	if _, ok := roundtrip2["type"]; ok {
		t.Errorf("failure: type should be absent, got %v", roundtrip2["type"])
	}
}
