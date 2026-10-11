package config

import "testing"

func TestParseFieldGuards(t *testing.T) {
	guards, err := ParseFieldGuards([]string{"P:Sprint priority", " Department ", "*:Tag", "", "t : State"})
	if err != nil {
		t.Fatalf("ParseFieldGuards: %v", err)
	}
	want := []FieldGuard{{"P", "Sprint priority"}, {"", "Department"}, {"", "Tag"}, {"t", "State"}}
	if len(guards) != len(want) {
		t.Fatalf("got %d guards, want %d: %+v", len(guards), len(want), guards)
	}
	for i := range want {
		if guards[i] != want[i] {
			t.Errorf("guard %d = %+v, want %+v", i, guards[i], want[i])
		}
	}
	for _, bad := range []string{"P:", ":", " : "} {
		if _, err := ParseFieldGuards([]string{bad}); err == nil {
			t.Errorf("entry %q must be refused", bad)
		}
	}
	if err := ValidateFieldGuards([]string{"P:Sprint priority"}); err != nil {
		t.Errorf("valid list refused: %v", err)
	}
}

func TestFieldGuardMatches(t *testing.T) {
	g := FieldGuard{Project: "P", Field: "Sprint priority"}
	if !g.Matches("p", "SPRINT PRIORITY") || g.Matches("T", "Sprint priority") || g.Matches("P", "Priority") {
		t.Error("project-scoped guard matching is off")
	}
	all := FieldGuard{Field: "Department"}
	if !all.MatchesProject("anything") || !all.Matches("T", "department") {
		t.Error("unscoped guard must match every project")
	}
}
