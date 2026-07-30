package wireguard

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/smnhffmnn/mux/internal/config"
)

// WGTunnel represents a userspace WireGuard tunnel backed by gVisor netstack.
// It implements the tools.Dialer interface (DialContext).
type WGTunnel struct {
	name string
	dev  *device.Device
	tnet *netstack.Net

	// Endpoint failover state (see watchdog): the resolved endpoint
	// candidates, the peer key needed to re-point the device, and the
	// original hostname endpoint for re-resolving after a wrap-around.
	// Owned by the watchdog goroutine after New returns — no locking;
	// nothing else may touch these fields.
	peerPubHex string
	endpoint   string
	candidates []string
	current    int

	stopOnce sync.Once
	stop     chan struct{}
}

// New creates and starts a WireGuard tunnel from the given config.
// Returns an error if the config is incomplete or the tunnel fails to start.
func New(cfg config.TunnelConfig) (*WGTunnel, error) {
	if cfg.PrivateKey == "" || cfg.PeerPublicKey == "" || cfg.PeerEndpoint == "" {
		return nil, fmt.Errorf("incomplete tunnel config (need private key, peer public key, endpoint)")
	}

	// Parse local tunnel address (strip CIDR suffix if present)
	addrStr := cfg.TunnelAddress
	if idx := strings.IndexByte(addrStr, '/'); idx > 0 {
		addrStr = addrStr[:idx]
	}
	localAddr, err := netip.ParseAddr(addrStr)
	if err != nil {
		return nil, fmt.Errorf("parse tunnel address %q: %w", cfg.TunnelAddress, err)
	}

	dnsAddrs := parseDNS(cfg.Name, cfg.DNS)

	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1420
	}

	candidates, err := resolveEndpointCandidates(cfg.PeerEndpoint)
	if err != nil {
		return nil, fmt.Errorf("resolve endpoint %q: %w", cfg.PeerEndpoint, err)
	}
	log.Printf("[wg] Tunnel %q: endpoint %s resolved to %s", cfg.Name, cfg.PeerEndpoint, strings.Join(candidates, ", "))

	// Create virtual TUN + userspace TCP/IP stack
	tunDev, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{localAddr},
		dnsAddrs,
		mtu,
	)
	if err != nil {
		return nil, fmt.Errorf("create netstack tun: %w", err)
	}

	// Create WireGuard device
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "wg"))

	pubHex, err := keyToHex(cfg.PeerPublicKey)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("peer public key: %w", err)
	}

	// Build IPC configuration
	ipcConf, err := buildIPC(cfg, candidates[0], pubHex)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("build IPC config: %w", err)
	}

	if err := dev.IpcSet(ipcConf); err != nil {
		dev.Close()
		return nil, fmt.Errorf("ipc set: %w", err)
	}

	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("device up: %w", err)
	}

	if ka := time.Duration(cfg.KeepAlive) * time.Second; ka > 0 && ka <= failStreakLimit*watchdogInterval {
		log.Printf("[wg] Tunnel %q: keepalive %ds is within the failover window (%s) — a healthy idle tunnel may be rotated spuriously, use a larger interval", cfg.Name, cfg.KeepAlive, failStreakLimit*watchdogInterval)
	}

	t := &WGTunnel{
		name:       cfg.Name,
		dev:        dev,
		tnet:       tnet,
		peerPubHex: pubHex,
		endpoint:   cfg.PeerEndpoint,
		candidates: candidates,
		stop:       make(chan struct{}),
	}
	go t.watchdog()
	return t, nil
}

// Name returns the tunnel name.
func (t *WGTunnel) Name() string { return t.name }

// DialContext dials a TCP connection through the WireGuard tunnel.
func (t *WGTunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.tnet.DialContext(ctx, network, address)
}

// LookupHost resolves a hostname through the tunnel's DNS.
func (t *WGTunnel) LookupHost(ctx context.Context, host string) ([]string, error) {
	return t.tnet.LookupContextHost(ctx, host)
}

// Close shuts down the WireGuard device and releases all resources.
func (t *WGTunnel) Close() error {
	t.stopOnce.Do(func() { close(t.stop) })
	t.dev.Close()
	return nil
}

// Watchdog timing. WireGuard retries handshakes roughly every 5 seconds, so
// a dead endpoint keeps tx growing every interval. Three intervals (15s) of
// failing traffic are required before rotating: a healthy peer's only reply
// to one-sided traffic is the passive keepalive after 10s — a shorter window
// would race it and rotate working tunnels.
const (
	watchdogInterval = 5 * time.Second
	failStreakLimit  = 3
)

// watchdog rotates the peer endpoint to the next resolved address when
// traffic demonstrably fails: we keep sending (tx grows) but nothing comes
// back (rx flat) and no handshake completes. This is deliberately passive —
// WireGuard handshakes lazily, so an idle tunnel produces no signal and must
// not be rotated.
//
// Failed sends are the blind spot of the traffic signal: wireguard-go only
// counts tx for packets the OS accepted, so an endpoint without a local
// route (roaming into a v4-only network while the peer sits on IPv6)
// produces no tx growth at all. The idle branch covers that with a route
// check and switches to a routable candidate.
func (t *WGTunnel) watchdog() {
	ticker := time.NewTicker(watchdogInterval)
	defer ticker.Stop()

	var prev peerStats
	failStreak := 0
	noAltLogged := false

	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
		}

		ipc, err := t.dev.IpcGet()
		if err != nil {
			// Transient IPC failure. A closed device yields an empty dump
			// (no peer section) instead of an error — its zero stats never
			// count as failure, and the stop channel ends the loop.
			continue
		}
		cur := parsePeerStats(ipc)

		progress := cur.rxBytes > prev.rxBytes || cur.lastHandshake > prev.lastHandshake
		switch {
		case shouldCountFailure(prev, cur):
			failStreak++
		case progress:
			failStreak = 0
			noAltLogged = false
		default:
			// Idle — the one state a routeless endpoint produces (see the
			// function comment). Rescue via route check.
			failStreak = 0
			t.checkRoute()
		}
		prev = cur

		if failStreak >= failStreakLimit {
			failStreak = 0
			t.rotateEndpoint(&noAltLogged)
		}
	}
}

// rotateEndpoint re-points the peer at the next candidate address after
// failing traffic. After the last candidate it re-resolves the hostname —
// DNS may have changed, and a network switch can invalidate every previously
// resolved address. Candidates without a local route are skipped: switching
// onto one would silence the traffic signal entirely (failed sends count no
// tx) and leave only the slower route check to recover.
func (t *WGTunnel) rotateEndpoint(noAltLogged *bool) {
	from := t.candidates[t.current]

	next := t.current + 1
	if next >= len(t.candidates) {
		next = 0
		fresh, err := resolveEndpointCandidates(t.endpoint)
		switch {
		case err != nil:
			log.Printf("[wg] Tunnel %q: re-resolve of %s failed, keeping cached candidates: %v", t.name, t.endpoint, err)
		case len(fresh) > 0:
			t.candidates = fresh
		}
	}
	next = nextRoutableIndex(t.candidates, next, hasRouteTo)

	to := t.candidates[next]
	if to == from {
		// Nothing better to switch to (single candidate, or the failing
		// address is the only routable one). Say so once per failure episode
		// — a silent watchdog would hide exactly the diagnosis it exists for.
		if !*noAltLogged {
			log.Printf("[wg] Tunnel %q: traffic failing via %s — no alternative endpoint to switch to", t.name, from)
			*noAltLogged = true
		}
		return
	}

	t.current = next
	log.Printf("[wg] Tunnel %q: traffic failing via %s — switching endpoint to %s", t.name, from, to)
	t.setPeerEndpoint(to)
}

// checkRoute rescues the tunnel when the current endpoint address has lost
// its local route: failed sends produce no tx signal, so the traffic
// watchdog never fires — this check is the only remaining signal. It only
// acts when a routable alternative exists, so a fully offline machine stays
// quiet.
func (t *WGTunnel) checkRoute() {
	cur := t.candidates[t.current]
	if hasRouteTo(cur) {
		return
	}
	i := nextRoutableIndex(t.candidates, t.current, hasRouteTo)
	if i == t.current {
		return
	}
	log.Printf("[wg] Tunnel %q: no route to %s — switching endpoint to %s", t.name, cur, t.candidates[i])
	t.current = i
	t.setPeerEndpoint(t.candidates[i])
}

// nextRoutableIndex returns the index of the first candidate at or after
// start (wrapping around) that routable reports reachable, or start when
// none is.
func nextRoutableIndex(candidates []string, start int, routable func(string) bool) int {
	for off := 0; off < len(candidates); off++ {
		i := (start + off) % len(candidates)
		if routable(candidates[i]) {
			return i
		}
	}
	return start
}

// setPeerEndpoint re-points the existing peer at a new endpoint address.
func (t *WGTunnel) setPeerEndpoint(endpoint string) {
	conf := fmt.Sprintf("public_key=%s\nupdate_only=true\nendpoint=%s\n", t.peerPubHex, endpoint)
	if err := t.dev.IpcSet(conf); err != nil {
		log.Printf("[wg] Tunnel %q: endpoint switch failed: %v", t.name, err)
	}
}

// peerStats is the slice of IPC state the watchdog decides on.
type peerStats struct {
	txBytes       uint64
	rxBytes       uint64
	lastHandshake int64 // unix seconds, 0 = never
}

// parsePeerStats extracts the single peer's counters from a wireguard-go
// UAPI dump. mux tunnels have exactly one peer, so plain key matching is
// sufficient.
func parsePeerStats(ipc string) peerStats {
	var s peerStats
	for _, line := range strings.Split(ipc, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "tx_bytes":
			s.txBytes, _ = strconv.ParseUint(v, 10, 64)
		case "rx_bytes":
			s.rxBytes, _ = strconv.ParseUint(v, 10, 64)
		case "last_handshake_time_sec":
			s.lastHandshake, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return s
}

// shouldCountFailure reports whether the interval between two stat snapshots
// shows failing traffic: we sent something, received nothing, and no
// handshake completed in the interval. Idle intervals (nothing sent) and
// working intervals (anything received, or a fresh handshake) reset the
// verdict.
func shouldCountFailure(prev, cur peerStats) bool {
	sent := cur.txBytes > prev.txBytes
	received := cur.rxBytes > prev.rxBytes
	handshook := cur.lastHandshake > prev.lastHandshake
	return sent && !received && !handshook
}

// buildIPC constructs the IPC configuration string for wireguard-go.
// Keys must be hex-encoded (not base64); pubHex is the already-converted
// peer public key (the caller also needs it for endpoint updates). endpoint
// must be in IP:port form (a resolved candidate) — WireGuard IPC does not
// accept hostnames.
func buildIPC(cfg config.TunnelConfig, endpoint, pubHex string) (string, error) {
	privHex, err := keyToHex(cfg.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("private key: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", privHex)
	fmt.Fprintf(&b, "public_key=%s\n", pubHex)

	if cfg.PresharedKey != "" {
		pskHex, err := keyToHex(cfg.PresharedKey)
		if err != nil {
			return "", fmt.Errorf("preshared key: %w", err)
		}
		fmt.Fprintf(&b, "preshared_key=%s\n", pskHex)
	}

	fmt.Fprintf(&b, "endpoint=%s\n", endpoint)

	// allowed_ip can appear multiple times
	for _, aip := range strings.Split(cfg.AllowedIPs, ",") {
		aip = strings.TrimSpace(aip)
		if aip != "" {
			fmt.Fprintf(&b, "allowed_ip=%s\n", aip)
		}
	}

	if cfg.KeepAlive > 0 {
		fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", cfg.KeepAlive)
	}

	return b.String(), nil
}

// resolveEndpointCandidates turns an endpoint (hostname:port or IP:port) into
// an ordered list of IP:port candidates for the peer. The preferred address
// family leads, the rest follow as failover targets for the watchdog.
//
// Preference is decided by local routability, not by record type: an AAAA
// record proves nothing about this network — DNS happily serves it inside a
// v4-only home network, where blindly preferring IPv6 sends every handshake
// to an unreachable address (MUX-18). hasRouteTo asks the local stack for a
// route to the concrete address instead.
func resolveEndpointCandidates(endpoint string) ([]string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("split host:port: %w", err)
	}
	// The port flows verbatim into UAPI set lines (endpoint=...) — reject
	// anything non-numeric so no config value can smuggle extra directives
	// into the IPC stream.
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return nil, fmt.Errorf("invalid endpoint port %q", port)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return []string{endpoint}, nil // already an IP — nothing to choose
	}
	// Bounded lookup: the watchdog re-resolves after failover wrap-around,
	// and a hanging resolver (typical right after a network change) must not
	// stall it for the OS resolver timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("lookup %q: %w", host, err)
	}

	var v4, v6 []netip.Addr
	for _, ip := range ips {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		if a.Is6() {
			v6 = append(v6, a)
		} else {
			v4 = append(v4, a)
		}
	}
	if len(v4) == 0 && len(v6) == 0 {
		return nil, fmt.Errorf("no addresses for %q", host)
	}

	preferV6 := len(v6) > 0 && hasRouteTo(net.JoinHostPort(v6[0].String(), port))
	return orderCandidates(v4, v6, port, preferV6), nil
}

// orderCandidates renders the address lists as IP:port strings, preferred
// family first. Split out from resolveEndpointCandidates for testability.
func orderCandidates(v4, v6 []netip.Addr, port string, preferV6 bool) []string {
	format := func(addrs []netip.Addr) []string {
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, net.JoinHostPort(a.String(), port))
		}
		return out
	}
	if preferV6 {
		return append(format(v6), format(v4)...)
	}
	return append(format(v4), format(v6)...)
}

// hasRouteTo reports whether the local stack has a route to the IP:port
// candidate. A UDP "connect" performs only a local route lookup — no packet
// is sent — so this is a cheap, offline check that fails exactly where it
// should: in a network whose stack cannot reach the address family at all.
func hasRouteTo(hostport string) bool {
	c, err := net.DialTimeout("udp", hostport, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// keyToHex converts a base64-encoded WireGuard key to hex.
func keyToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("key must be 32 bytes, got %d", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// parseDNS extracts resolver addresses from a wg-quick-style DNS value.
// wg-quick allows domain entries alongside resolver IPs in the DNS field
// (e.g. "fd00::1, ~corp.example.com" — systemd-style routing domains with a
// "~" prefix). On an OS-level tunnel those act as split-DNS scoping: they
// limit which hostnames are sent to the tunnel's resolver at all. Tunnel
// configs often arrive verbatim from such .conf files, so tolerate those
// entries: only IP addresses become resolvers, non-IP entries are logged and
// skipped. mux does not need the scoping — each tunnel's resolver serves
// exclusively that tunnel's connections, so no other query can reach it.
func parseDNS(tunnelName, dns string) []netip.Addr {
	var out []netip.Addr
	for _, d := range strings.Split(dns, ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		a, err := netip.ParseAddr(d)
		if err != nil {
			log.Printf("[wg] Tunnel %q: ignoring non-IP DNS entry %q (split-DNS routing domain — scoping is implicit in mux, the tunnel resolver only serves this tunnel's connections)", tunnelName, d)
			continue
		}
		out = append(out, a)
	}
	return out
}
