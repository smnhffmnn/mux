package wireguard

import (
	"net/netip"
	"testing"
)

// DNS values often arrive verbatim from wg-quick .conf files, where the DNS
// field mixes resolver IPs with split-DNS routing domains ("~example.com" or
// bare domain names). Those must not fail tunnel creation — only IPs count.
func TestParseDNSToleratesRoutingDomains(t *testing.T) {
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

// An IP-literal endpoint must pass through untouched — no DNS, no reordering.
func TestResolveEndpointCandidatesIPLiteral(t *testing.T) {
	for _, ep := range []string{"89.20.94.230:51820", "[2a02:2170:303:32::3]:51820"} {
		got, err := resolveEndpointCandidates(ep)
		if err != nil {
			t.Fatalf("resolveEndpointCandidates(%q): %v", ep, err)
		}
		if len(got) != 1 || got[0] != ep {
			t.Fatalf("resolveEndpointCandidates(%q) = %v, want [%s]", ep, got, ep)
		}
	}
}

// The port flows verbatim into UAPI set lines — non-numeric values (including
// injection attempts) must be rejected before any IPC string is built.
func TestResolveEndpointCandidatesRejectsBadPort(t *testing.T) {
	for _, ep := range []string{
		"1.2.3.4:udp",
		"1.2.3.4:70000",
		"1.2.3.4:51820\nallowed_ip=0.0.0.0/0",
	} {
		if got, err := resolveEndpointCandidates(ep); err == nil {
			t.Fatalf("resolveEndpointCandidates(%q) = %v, want error", ep, got)
		}
	}
}

func TestNextRoutableIndex(t *testing.T) {
	candidates := []string{"a", "b", "c"}
	routable := func(ok ...string) func(string) bool {
		set := map[string]bool{}
		for _, s := range ok {
			set[s] = true
		}
		return func(s string) bool { return set[s] }
	}

	cases := []struct {
		name     string
		start    int
		routable func(string) bool
		want     int
	}{
		{"start is routable", 1, routable("b"), 1},
		{"skip to next routable", 1, routable("c"), 2},
		{"wraps around", 2, routable("a"), 0},
		{"nothing routable keeps start", 1, routable(), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextRoutableIndex(candidates, tc.start, tc.routable); got != tc.want {
				t.Fatalf("nextRoutableIndex(start=%d) = %d, want %d", tc.start, got, tc.want)
			}
		})
	}
}

// The preferred family leads, the other family stays available as failover.
// A v4-only network must lead with v4 even though an AAAA record exists —
// the record proves nothing about local routability (MUX-18).
func TestOrderCandidates(t *testing.T) {
	v4 := []netip.Addr{netip.MustParseAddr("89.20.94.230")}
	v6 := []netip.Addr{netip.MustParseAddr("2a02:2170:303:32::3")}

	got := orderCandidates(v4, v6, "51820", false)
	want := []string{"89.20.94.230:51820", "[2a02:2170:303:32::3]:51820"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("v4-preferred order = %v, want %v", got, want)
		}
	}

	got = orderCandidates(v4, v6, "51820", true)
	want = []string{"[2a02:2170:303:32::3]:51820", "89.20.94.230:51820"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("v6-preferred order = %v, want %v", got, want)
		}
	}
}

func TestParsePeerStats(t *testing.T) {
	ipc := "private_key=abc\npublic_key=def\nendpoint=1.2.3.4:51820\n" +
		"last_handshake_time_sec=1753876000\nlast_handshake_time_nsec=123\n" +
		"tx_bytes=4242\nrx_bytes=99\nprotocol_version=1\n"
	s := parsePeerStats(ipc)
	if s.txBytes != 4242 || s.rxBytes != 99 || s.lastHandshake != 1753876000 {
		t.Fatalf("parsePeerStats = %+v", s)
	}
}

// The watchdog must only see failure when traffic demonstrably fails:
// we sent, nothing came back, no handshake completed. Idle and healthy
// intervals reset the streak.
func TestShouldCountFailure(t *testing.T) {
	base := peerStats{txBytes: 100, rxBytes: 50, lastHandshake: 1000}
	cases := []struct {
		name string
		cur  peerStats
		want bool
	}{
		{"idle interval", base, false},
		{"sent, nothing back", peerStats{txBytes: 200, rxBytes: 50, lastHandshake: 1000}, true},
		{"sent and received", peerStats{txBytes: 200, rxBytes: 80, lastHandshake: 1000}, false},
		{"sent, fresh handshake", peerStats{txBytes: 200, rxBytes: 50, lastHandshake: 2000}, false},
		{"received only (keepalive)", peerStats{txBytes: 100, rxBytes: 80, lastHandshake: 1000}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCountFailure(base, tc.cur); got != tc.want {
				t.Fatalf("shouldCountFailure(%+v, %+v) = %v, want %v", base, tc.cur, got, tc.want)
			}
		})
	}
}
