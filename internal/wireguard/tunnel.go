package wireguard

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strings"

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

	// Parse DNS servers
	var dnsAddrs []netip.Addr
	if cfg.DNS != "" {
		for _, d := range strings.Split(cfg.DNS, ",") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			a, err := netip.ParseAddr(d)
			if err != nil {
				return nil, fmt.Errorf("parse DNS %q: %w", d, err)
			}
			dnsAddrs = append(dnsAddrs, a)
		}
	}

	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1420
	}

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

	// Build IPC configuration
	ipcConf, err := buildIPC(cfg)
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

	return &WGTunnel{
		name: cfg.Name,
		dev:  dev,
		tnet: tnet,
	}, nil
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
	t.dev.Close()
	return nil
}

// buildIPC constructs the IPC configuration string for wireguard-go.
// Keys must be hex-encoded (not base64).
func buildIPC(cfg config.TunnelConfig) (string, error) {
	privHex, err := keyToHex(cfg.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("private key: %w", err)
	}
	pubHex, err := keyToHex(cfg.PeerPublicKey)
	if err != nil {
		return "", fmt.Errorf("peer public key: %w", err)
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

	// WireGuard IPC requires IP:port, not hostname:port — resolve if needed.
	endpoint, err := resolveEndpoint(cfg.PeerEndpoint)
	if err != nil {
		return "", fmt.Errorf("resolve endpoint %q: %w", cfg.PeerEndpoint, err)
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

// resolveEndpoint ensures the endpoint is in IP:port form.
// If the host part is already an IP, it is returned as-is.
// Otherwise it is resolved via DNS, preferring IPv6 over IPv4.
func resolveEndpoint(endpoint string) (string, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("split host:port: %w", err)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return endpoint, nil // already an IP
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return "", fmt.Errorf("lookup %q: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no addresses for %q", host)
	}
	// Prefer IPv6 if available
	resolved := ips[0]
	for _, ip := range ips {
		if a, err := netip.ParseAddr(ip); err == nil && a.Is6() {
			resolved = ip
			break
		}
	}
	// IPv6 addresses need brackets in host:port notation
	if a, err := netip.ParseAddr(resolved); err == nil && a.Is6() {
		return "[" + resolved + "]:" + port, nil
	}
	return resolved + ":" + port, nil
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
