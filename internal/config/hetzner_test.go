package config

import (
	"strings"
	"testing"
)

func TestHetznerCloudBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  string
	}{
		{"", HetznerCloudDefaultURL, ""},
		{"https://api.hetzner.cloud", "https://api.hetzner.cloud", ""},
		{"https://api.hetzner.cloud/", "https://api.hetzner.cloud", ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", ""},
		{"http://api.hetzner.cloud", "", "loopback"},
		{"https://api.hetzner.cloud/v1", "", "bare API host"},
		{"https://api.hetzner.cloud?x=1", "", "without query"},
		{"ftp://api.hetzner.cloud", "", "http(s)"},
	}
	for _, c := range cases {
		got, err := HetznerCloudBaseURL(c.in)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%q: err = %v, want containing %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestParseResourceGuards(t *testing.T) {
	guards, err := ParseResourceGuards([]string{" my-cell-01 ", "server:prod-*", "*:shared", "", "Zone:example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(guards) != 4 {
		t.Fatalf("got %d guards, want 4", len(guards))
	}
	if guards[0] != (ResourceGuard{Kind: "", Pattern: "my-cell-01"}) {
		t.Errorf("guard 0 = %+v", guards[0])
	}
	if guards[1] != (ResourceGuard{Kind: "server", Pattern: "prod-*"}) {
		t.Errorf("guard 1 = %+v", guards[1])
	}
	if guards[2].Kind != "" || guards[3].Kind != "zone" {
		t.Errorf("guards 2/3 = %+v %+v", guards[2], guards[3])
	}

	for _, bad := range []string{"server:", ":", "server:[", "sever:x"} {
		if _, err := ParseResourceGuards([]string{bad}); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestResourceGuardMatches(t *testing.T) {
	g := ResourceGuard{Kind: "server", Pattern: "prod-*"}
	if !g.Matches("server", "prod-db-01") || !g.Matches("Server", "PROD-web") {
		t.Error("pattern should match prod-* servers, case-insensitively")
	}
	if g.Matches("volume", "prod-db-01") || g.Matches("server", "staging-01") {
		t.Error("pattern must not match another kind or another name")
	}
	any := ResourceGuard{Pattern: "my-cell-01"}
	if !any.Matches("server", "my-cell-01") || !any.Matches("volume", "my-cell-01") || any.Matches("volume", "my-cell-01-data") {
		t.Error("a guard without kind covers every kind, exact name only")
	}
}

func TestValidateHetznerCloud(t *testing.T) {
	ok := Connection{Type: TypeHetznerCloud, ProtectedResources: []string{"server:prod-*"}}
	if err := ValidateHetznerCloud(ok); err != nil {
		t.Errorf("valid connection refused: %v", err)
	}
	if err := ValidateHetznerCloud(Connection{Type: TypeBunq, URL: "nonsense"}); err != nil {
		t.Errorf("another type must not be checked: %v", err)
	}
	if err := ValidateHetznerCloud(Connection{Type: TypeHetznerCloud, URL: "https://api.hetzner.cloud/v1"}); err == nil {
		t.Error("URL with path accepted")
	}
	if err := ValidateHetznerCloud(Connection{Type: TypeHetznerCloud, ProtectedResources: []string{"server:"}}); err == nil {
		t.Error("empty guard name accepted")
	}
	if err := ValidateHetznerCloud(Connection{Type: TypeHetznerCloud, ReadOnly: true, AllowLifecycle: true}); err == nil {
		t.Error("read_only with allow_lifecycle accepted")
	}
}
