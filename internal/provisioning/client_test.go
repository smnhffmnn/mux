package provisioning

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smnhffmnn/mux/internal/config"
)

// The provisioning endpoint is the one inbound path allowed to carry a tunnel
// private key (TunnelConfig tags it `json:"-"`, so a plain unmarshal would
// silently drop it and the tunnel would never become Enabled). Guard the
// wireTunnel carry-over.
func TestFetchCarriesTunnelPrivateKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"tunnels": [{
				"name": "orbit",
				"type": "wireguard",
				"peerPublicKey": "pubkey",
				"peerEndpoint": "vpn.example.com:51820",
				"allowedIPs": "fd00::/48",
				"tunnelAddress": "fd00::2/128",
				"privateKey": "sekrit"
			}],
			"connections": [{"name": "db", "type": "mariadb"}]
		}`))
	}))
	defer srv.Close()

	resp, err := Fetch(context.Background(), srv.URL, "token")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(resp.Tunnels) != 1 {
		t.Fatalf("expected 1 tunnel, got %d", len(resp.Tunnels))
	}

	tun := resp.Tunnels[0]
	if tun.PrivateKey != "sekrit" {
		t.Errorf("PrivateKey = %q, want %q", tun.PrivateKey, "sekrit")
	}
	if tun.Source != config.SourceProvisioning {
		t.Errorf("tunnel Source = %q, want %q", tun.Source, config.SourceProvisioning)
	}
	if !tun.Enabled() {
		t.Error("tunnel should be Enabled once the private key survives parsing")
	}
	if len(resp.Connections) != 1 || resp.Connections[0].Source != config.SourceProvisioning {
		t.Errorf("connections not marked as provisioning-sourced: %+v", resp.Connections)
	}
}
