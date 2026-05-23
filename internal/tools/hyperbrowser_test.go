package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFilterAgentStatusResponse_StripsScreenshots verifies that
// per-step base64 screenshots are removed both via the bytes-level
// pre-strip (the regex on raw input) AND the post-Unmarshal allowlist
// (state.screenshot would not be re-emitted even if pre-strip missed it).
func TestFilterAgentStatusResponse_StripsScreenshots(t *testing.T) {
	bigBase64 := strings.Repeat("A", 100_000) // ~100 KB stand-in for a screenshot

	in := map[string]any{
		"jobId":   "abc-123",
		"status":  "completed",
		"liveUrl": "https://app.hyperbrowser.ai/live/xyz",
		"metadata": map[string]any{
			"inputTokens":           100,
			"outputTokens":          50,
			"numTaskStepsCompleted": 2,
			"hiddenCost":            "should-be-stripped",
		},
		"data": map[string]any{
			"finalResult": "done",
			"steps": []any{
				map[string]any{
					"model_output": map[string]any{
						"current_state": map[string]any{
							"evaluation_previous_goal": "ok",
							"next_goal":                "click button",
							"memory":                   "should-be-stripped",
						},
						"action": []any{map[string]any{"click": "btn"}},
					},
					"state": map[string]any{
						"url":        "https://example.com",
						"title":      "Example",
						"screenshot": bigBase64,
					},
				},
			},
		},
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	out, err := filterAgentStatusResponse(raw)
	if err != nil {
		t.Fatalf("filter failed: %v", err)
	}

	// Output must not contain the big base64 payload.
	if strings.Contains(string(out), bigBase64) {
		t.Fatalf("output still contains the screenshot bytes")
	}

	// Output must be < 5 KB — proves the strip worked end-to-end.
	if len(out) > 5_000 {
		t.Fatalf("output too large: %d bytes (expected < 5000)", len(out))
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// Top-level fields kept.
	for _, k := range []string{"jobId", "status", "liveUrl"} {
		if _, ok := parsed[k]; !ok {
			t.Errorf("top-level field %q dropped", k)
		}
	}

	// metadata allowlist enforced.
	md, ok := parsed["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing or wrong type")
	}
	if _, ok := md["hiddenCost"]; ok {
		t.Error("metadata.hiddenCost should have been dropped")
	}
	for _, k := range []string{"inputTokens", "outputTokens", "numTaskStepsCompleted"} {
		if _, ok := md[k]; !ok {
			t.Errorf("metadata.%s missing", k)
		}
	}

	// data.steps allowlist enforced.
	data, ok := parsed["data"].(map[string]any)
	if !ok {
		t.Fatalf("data missing")
	}
	if data["finalResult"] != "done" {
		t.Errorf("finalResult wrong: %v", data["finalResult"])
	}
	steps, ok := data["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("steps missing or wrong length: %v", data["steps"])
	}
	step := steps[0].(map[string]any)
	state, _ := step["state"].(map[string]any)
	if _, ok := state["screenshot"]; ok {
		t.Error("state.screenshot should have been dropped from output")
	}
	mo, _ := step["model_output"].(map[string]any)
	cs, _ := mo["current_state"].(map[string]any)
	if _, ok := cs["memory"]; ok {
		t.Error("current_state.memory should have been dropped")
	}
	if cs["next_goal"] != "click button" {
		t.Errorf("current_state.next_goal wrong: %v", cs["next_goal"])
	}
}

// TestScreenshotFieldRegex_RemovesBytesBeforeUnmarshal proves the
// allocator-optimisation side of the v0.30.1 fix: the regex strips
// the screenshot bytes BEFORE json.Unmarshal sees them.
func TestScreenshotFieldRegex_RemovesBytesBeforeUnmarshal(t *testing.T) {
	bigBase64 := strings.Repeat("A", 200_000) // 200 KB
	body := []byte(`{"data":{"steps":[{"state":{"screenshot":"` + bigBase64 + `","url":"https://x"}}]}}`)

	stripped := screenshotField.ReplaceAll(body, []byte(`"screenshot":""`))

	if strings.Contains(string(stripped), bigBase64) {
		t.Fatal("screenshot bytes still present after regex strip")
	}
	if len(stripped) > 200 {
		t.Fatalf("stripped body too large: %d bytes", len(stripped))
	}
	// Surrounding structure preserved so Unmarshal can proceed.
	if !strings.Contains(string(stripped), `"url":"https://x"`) {
		t.Error("non-screenshot fields lost by regex")
	}
}

// TestScreenshotFieldRegex_DoesNotMatchSimilarKeys guards against false
// positives — only exact "screenshot" keys should match.
func TestScreenshotFieldRegex_DoesNotMatchSimilarKeys(t *testing.T) {
	cases := []string{
		`{"page_screenshot":"keep"}`,
		`{"screenshot_url":"keep"}`,
		`{"description":"this is a screenshot of x"}`,
	}
	for _, c := range cases {
		got := screenshotField.ReplaceAllString(c, `"screenshot":""`)
		if got != c {
			t.Errorf("regex should not have matched %q, got %q", c, got)
		}
	}
}
