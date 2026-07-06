package wireguard

import (
	"net/netip"
	"testing"
)

// DNS values often arrive verbatim from wg-quick .conf files, where the DNS
// field mixes resolver IPs with search domains ("~example.com" or bare
// domain names). Those must not fail tunnel creation — only IPs count.
func TestParseDNSToleratesSearchDomains(t *testing.T) {
	cases := []struct {
		name string
		dns  string
		want []string
	}{
		{"empty", "", nil},
		{"single v4", "10.0.0.1", []string{"10.0.0.1"}},
		{"single v6", "fd00:1701:d::1", []string{"fd00:1701:d::1"}},
		{
			"v6 with routing domains",
			"fd00:1701:d::1, ~naskorsports.com, ~stores.myfit24.de",
			[]string{"fd00:1701:d::1"},
		},
		{"bare domain", "resolver.example.com", nil},
		{"mixed order", "~corp.example.com, 10.0.0.1, fd00::1", []string{"10.0.0.1", "fd00::1"}},
		{"whitespace and empties", " , fd00::1 , ", []string{"fd00::1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDNS("test", tc.dns)
			if len(got) != len(tc.want) {
				t.Fatalf("parseDNS(%q) = %v, want %v", tc.dns, got, tc.want)
			}
			for i, w := range tc.want {
				if got[i] != netip.MustParseAddr(w) {
					t.Errorf("parseDNS(%q)[%d] = %v, want %v", tc.dns, i, got[i], w)
				}
			}
		})
	}
}
