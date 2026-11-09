//go:build live

// Live contract test for the hetzner-cloud type. Runs only with:
//
//	HETZNER_LIVE_TOKEN=… go test -tags 'notray live' ./internal/tools -run TestHetznerCloudLive -v
//
// It reads: one page of servers, the server types with their prices, the
// zones and the record sets of the first zone, a filtered price list and the
// last successful actions. Nothing is created, changed or deleted — a Read
// token is enough and recommended. Gated behind the build tag and the
// environment because it touches a real project and counts against the
// project's request budget. The log carries counts only; names appear with
// HETZNER_LIVE_VERBOSE=1 (a local run).

package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

func hcLiveLogf(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("HETZNER_LIVE_VERBOSE") == "1" {
		t.Logf(format, args...)
	}
}

func newLiveHetznerCloud(t *testing.T) *HetznerCloud {
	t.Helper()
	token := os.Getenv("HETZNER_LIVE_TOKEN")
	if token == "" {
		t.Skip("HETZNER_LIVE_TOKEN not set")
	}
	h, err := NewHetznerCloud(config.Connection{Name: "hetzner-live", Type: config.TypeHetznerCloud, Token: token, ReadOnly: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHetznerCloudLive_Read(t *testing.T) {
	h := newLiveHetznerCloud(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := h.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	t.Logf("project: %d servers, %d zones, %d of %d requests left", info.Servers, info.Zones, info.RateRemaining, info.RateLimit)

	res, err := h.handleList(ctx, toolRequest(map[string]any{"resource": "server", "per_page": float64(5)}))
	if err != nil || res.IsError {
		t.Fatalf("list server: %v / %s", err, resultText(t, res))
	}
	var servers struct {
		Count   int              `json:"count"`
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &servers); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d servers on the first page", servers.Count)
	for _, s := range servers.Servers {
		hcLiveLogf(t, "  %v %v %v %v %v", s["id"], s["name"], s["status"], s["server_type"], s["location"])
		if s["id"] == nil || s["name"] == nil || s["status"] == nil || s["server_type"] == nil {
			t.Errorf("server row lacks a summary field: %v", s)
		}
	}
	if len(servers.Servers) > 0 {
		id := hcScalar(servers.Servers[0]["id"])
		res, err = h.handleGet(ctx, toolRequest(map[string]any{"resource": "server", "id": id}))
		if err != nil || res.IsError {
			t.Fatalf("get server %s: %v / %s", id, err, resultText(t, res))
		}
		var one map[string]map[string]any
		json.Unmarshal([]byte(resultText(t, res)), &one)
		if one["server"] == nil || one["server"]["server_type"] == nil {
			t.Errorf("get server %s: no server_type in %s", id, resultText(t, res)[:200])
		}
		t.Logf("one server read back by id")
		name := hcScalar(servers.Servers[0]["name"])
		res, err = h.handleGet(ctx, toolRequest(map[string]any{"resource": "server", "id": name}))
		if err != nil || res.IsError {
			t.Fatalf("get server by name: %v / %s", err, resultText(t, res))
		}
		t.Logf("one server read back by name")
	}

	res, err = h.handleList(ctx, toolRequest(map[string]any{"resource": "server_type", "all": true}))
	if err != nil || res.IsError {
		t.Fatalf("list server_type: %v / %s", err, resultText(t, res))
	}
	var types struct {
		Count       int              `json:"count"`
		ServerTypes []map[string]any `json:"server_types"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &types)
	t.Logf("%d server types", types.Count)
	for _, st := range types.ServerTypes {
		if st["prices"] == nil || st["locations"] == nil {
			t.Errorf("server type %v lacks prices or locations", st["name"])
			break
		}
	}

	res, err = h.handleList(ctx, toolRequest(map[string]any{"resource": "zone", "all": true}))
	if err != nil || res.IsError {
		t.Fatalf("list zone: %v / %s", err, resultText(t, res))
	}
	var zones struct {
		Count int              `json:"count"`
		Zones []map[string]any `json:"zones"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &zones)
	t.Logf("%d zones", zones.Count)
	if len(zones.Zones) > 0 {
		zone := hcScalar(zones.Zones[0]["name"])
		hcLiveLogf(t, "zone %s", zone)
		res, err = h.handleListRRSets(ctx, toolRequest(map[string]any{"zone": zone, "all": true}))
		if err != nil || res.IsError {
			t.Fatalf("list_rrsets: %v / %s", err, resultText(t, res))
		}
		var sets struct {
			Count int `json:"count"`
		}
		json.Unmarshal([]byte(resultText(t, res)), &sets)
		t.Logf("%d record sets in the first zone", sets.Count)
		res, err = h.handleZonefile(ctx, toolRequest(map[string]any{"zone": zone}))
		if err != nil || res.IsError {
			t.Fatalf("zonefile: %v / %s", err, resultText(t, res))
		}
		t.Logf("zone file exported: %d bytes", len(resultText(t, res)))
	}

	res, err = h.handlePricing(ctx, toolRequest(map[string]any{"server_types": []any{"cx23", "cax11"}}))
	if err != nil || res.IsError {
		t.Fatalf("pricing: %v / %s", err, resultText(t, res))
	}
	var pricing struct {
		Pricing struct {
			Currency    string `json:"currency"`
			ServerTypes []any  `json:"server_types"`
		} `json:"pricing"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &pricing)
	t.Logf("pricing in %s, %d server types after the filter", pricing.Pricing.Currency, len(pricing.Pricing.ServerTypes))

	res, err = h.handleListActions(ctx, toolRequest(map[string]any{"resource": "server", "status": []any{"success"}, "per_page": float64(3), "sort": "finished:desc"}))
	if err != nil || res.IsError {
		t.Fatalf("list_actions: %v / %s", err, resultText(t, res))
	}
	var actions struct {
		Count int `json:"count"`
	}
	json.Unmarshal([]byte(resultText(t, res)), &actions)
	t.Logf("%d recent successful server actions", actions.Count)

	res, err = h.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v1/locations?per_page=2"}))
	if err != nil || res.IsError {
		t.Fatalf("api_get: %v / %s", err, resultText(t, res))
	}
	t.Logf("api_get /v1/locations: ok")
}
