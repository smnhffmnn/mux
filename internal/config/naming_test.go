package config

import (
	"regexp"
	"strings"
	"testing"
)

// clientPattern is the strictest known MCP client validation (Claude.ai chat).
var clientPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestSanitizeToolName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already clean", "youtrack-naskor_get_issue", "youtrack-naskor_get_issue"},
		{"spaces", "M365 Naskor_get_message", "M365_Naskor_get_message"},
		{"spaces and colon", "Youtrack Agile Board Naskor: Sprint_get_current_sprint", "Youtrack_Agile_Board_Naskor__Sprint_get_current_sprint"},
		{"umlaut is one rune, one underscore", "büro_tool", "b_ro_tool"},
		{"dot and slash", "api.v2/call", "api_v2_call"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeToolName(tt.in); got != tt.want {
				t.Errorf("SanitizeToolName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeToolName_Idempotent(t *testing.T) {
	inputs := []string{
		"Youtrack Agile Board Naskor: Sprint_get_current_sprint",
		"M365 Naskor_send_draft",
		strings.Repeat("x", 100),
	}
	for _, in := range inputs {
		once := SanitizeToolName(in)
		twice := SanitizeToolName(once)
		if once != twice {
			t.Errorf("not idempotent: %q → %q → %q", in, once, twice)
		}
	}
}

func TestSanitizeToolName_SatisfiesClientPattern(t *testing.T) {
	inputs := []string{
		"Youtrack Agile Board Naskor: Sprint_get_current_sprint",
		"Naskor ERP - Twinfield Browse_get",
		"Hyperbrowser Simon_browser_use_agent_start_and_some_extra_length_beyond_the_limit",
		"ümläut connection_tool",
	}
	for _, in := range inputs {
		got := SanitizeToolName(in)
		if !clientPattern.MatchString(got) {
			t.Errorf("SanitizeToolName(%q) = %q does not match %s", in, got, clientPattern)
		}
	}
}

func TestSanitizeToolName_Truncates(t *testing.T) {
	long := strings.Repeat("a", 80)
	if got := SanitizeToolName(long); len(got) != 64 {
		t.Errorf("len = %d, want 64", len(got))
	}
}
