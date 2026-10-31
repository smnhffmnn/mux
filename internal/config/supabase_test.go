package config

import (
	"strings"
	"testing"
)

func TestSupabase_Validate(t *testing.T) {
	const ref = "abcdefghijklmnopqrst"
	cases := []struct {
		name string
		conn Connection
		want string // substring of the error, "" for ok
	}{
		{"other type ignores the fields", Connection{Type: TypeIMAP, ProjectRef: "x"}, ""},
		{"ref alone", Connection{Type: TypeSupabase, ProjectRef: ref}, ""},
		{"ref with default url", Connection{Type: TypeSupabase, ProjectRef: ref, URL: SupabaseDefaultURL}, ""},
		{"custom host", Connection{Type: TypeSupabase, ProjectRef: ref, URL: "http://127.0.0.1:8080"}, ""},
		{"missing ref", Connection{Type: TypeSupabase}, "project_ref"},
		{"short ref", Connection{Type: TypeSupabase, ProjectRef: "abc"}, "not a project reference"},
		{"uppercase ref", Connection{Type: TypeSupabase, ProjectRef: "ABCDEFGHIJKLMNOPQRST"}, "not a project reference"},
		{"url with path", Connection{Type: TypeSupabase, ProjectRef: ref, URL: "https://api.supabase.com/v1"}, "bare API host"},
		{"url without scheme", Connection{Type: TypeSupabase, ProjectRef: ref, URL: "api.supabase.com"}, "http(s)://host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSupabase(tc.conn)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSupabase_EnabledAndDefaults(t *testing.T) {
	if !ValidType(TypeSupabase) {
		t.Error("supabase should be a valid type")
	}
	c := Connection{Type: TypeSupabase, Token: "sbp_x", ProjectRef: "abcdefghijklmnopqrst"}
	if !c.Enabled() {
		t.Error("token + project_ref must enable the connection")
	}
	c = Connection{Type: TypeSupabase, Token: "sbp_x"}
	if c.Enabled() {
		t.Error("without project_ref the connection must not be enabled")
	}
	c = Connection{Type: TypeSupabase, ProjectRef: "abcdefghijklmnopqrst"}
	if c.Enabled() {
		t.Error("without token the connection must not be enabled")
	}

	c = Connection{Type: TypeSupabase}
	ApplyConnectionDefaults(&c)
	if c.URL != SupabaseDefaultURL {
		t.Errorf("default url = %q", c.URL)
	}
	c = Connection{Type: TypeSupabase, URL: "http://localhost:1"}
	ApplyConnectionDefaults(&c)
	if c.URL != "http://localhost:1" {
		t.Errorf("explicit url overwritten: %q", c.URL)
	}

	for raw, want := range map[string]string{
		"":                            SupabaseDefaultURL,
		" https://api.supabase.com/ ": SupabaseDefaultURL,
		"http://127.0.0.1:9000":       "http://127.0.0.1:9000",
	} {
		got, err := SupabaseBaseURL(raw)
		if err != nil || got != want {
			t.Errorf("SupabaseBaseURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}
