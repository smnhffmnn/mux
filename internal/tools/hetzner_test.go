package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
)

// hcFake is a Cloud API stand-in for one project: two servers (one of them
// a protected cell host), a zone with one record set, actions that run for
// one poll before they succeed, and the error envelopes the type renders.
type hcFake struct {
	srv        *httptest.Server
	mu         sync.Mutex
	requests   []hcFakeRequest
	actions    map[int]int // action id → polls until success
	nextID     int
	rrsets     map[string]map[string]any // "name/TYPE" → rrset
	conflict   int                       // answer the first N PUTs with 409 conflict
	rateLow    bool                      // send a low RateLimit-Remaining
	imagePages int                       // pages the image listing spans (per_page rows each)
	gateway502 map[string]int            // method+path → remaining 502 answers
	dropOnce   map[string]bool           // method+path → close the connection once without an answer
}

type hcFakeRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
	Auth   string
}

func newHCFake(t *testing.T) *hcFake {
	t.Helper()
	f := &hcFake{actions: map[int]int{}, nextID: 1000, imagePages: 3, gateway502: map[string]int{}, dropOnce: map[string]bool{}, rrsets: map[string]map[string]any{
		"www/A": {"id": "www/A", "name": "www", "type": "A", "ttl": 300, "records": []any{map[string]any{"value": "198.51.100.1"}}, "labels": map[string]any{}, "protection": map[string]any{"change": false}, "zone": 7},
	}}
	servers := []map[string]any{
		{"id": 1, "name": "my-cell-01", "status": "running", "created": "2026-10-18T09:00:00+00:00",
			"server_type": map[string]any{"id": 22, "name": "cpx32", "prices": []any{}}, "location": map[string]any{"id": 1, "name": "fsn1"},
			"public_net":  map[string]any{"ipv4": map[string]any{"id": 11, "ip": "203.0.113.10", "dns_ptr": "cell.example"}, "ipv6": map[string]any{"id": 12, "ip": "2001:db8::/64"}, "firewalls": []any{map[string]any{"id": 5, "status": "applied"}}, "floating_ips": []any{}},
			"private_net": []any{}, "image": map[string]any{"id": 3, "name": "debian-12"}, "volumes": []any{9}, "labels": map[string]any{"role": "cell"},
			"protection": map[string]any{"delete": true, "rebuild": true}, "locked": false, "rescue_enabled": false, "backup_window": nil},
		{"id": 2, "name": "scratch-01", "status": "off", "created": "2026-11-01T09:00:00+00:00",
			"server_type": map[string]any{"id": 1, "name": "cx23"}, "location": map[string]any{"id": 2, "name": "nbg1"},
			"public_net":  map[string]any{"ipv4": map[string]any{"id": 13, "ip": "203.0.113.20"}, "ipv6": nil, "firewalls": []any{}, "floating_ips": []any{}},
			"private_net": []any{}, "image": nil, "volumes": []any{}, "labels": map[string]any{}, "protection": map[string]any{"delete": false, "rebuild": false}},
	}
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("RateLimit-Limit", "3600")
		if f.rateLow {
			w.Header().Set("RateLimit-Remaining", "12")
			w.Header().Set("RateLimit-Reset", "1800000000")
		} else {
			w.Header().Set("RateLimit-Remaining", "3500")
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	apiErr := func(w http.ResponseWriter, status int, code, msg string, details any) {
		write(w, status, map[string]any{"error": map[string]any{"code": code, "message": msg, "details": details}})
	}
	newAction := func(command string, polls int, resources ...map[string]any) map[string]any {
		f.nextID++
		f.actions[f.nextID] = polls
		status := "running"
		if polls == 0 {
			status = "success"
		}
		return map[string]any{"id": f.nextID, "command": command, "status": status, "progress": 0, "started": "2026-11-08T17:00:00+00:00", "finished": nil, "resources": resources, "error": nil}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			json.Unmarshal(raw, &body)
		}
		f.mu.Lock()
		f.requests = append(f.requests, hcFakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body, Auth: r.Header.Get("Authorization")})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok_test" {
			apiErr(w, 401, "unauthorized", "unable to authenticate", nil)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/v1")
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + p
		if f.gateway502[key] > 0 {
			f.gateway502[key]--
			w.WriteHeader(502)
			io.WriteString(w, "<html>bad gateway</html>")
			return
		}
		if f.dropOnce[key] {
			delete(f.dropOnce, key)
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
			panic("cannot hijack")
		}
		switch {
		case p == "/images" && r.Method == http.MethodGet:
			page, perPage := 1, 25
			fmt.Sscan(q.Get("page"), &page)
			fmt.Sscan(q.Get("per_page"), &perPage)
			var out []any
			for i := 0; i < perPage; i++ {
				out = append(out, map[string]any{"id": (page-1)*perPage + i + 1, "name": fmt.Sprintf("img-%d-%d", page, i), "type": "snapshot", "status": "available", "labels": map[string]any{}})
			}
			var next any
			if page < f.imagePages {
				next = page + 1
			}
			write(w, 200, map[string]any{"images": out, "meta": map[string]any{"pagination": map[string]any{"page": page, "per_page": perPage, "previous_page": nil, "next_page": next, "last_page": f.imagePages, "total_entries": f.imagePages * perPage}}})
		case p == "/images/5" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"image": map[string]any{"id": 5, "name": nil, "description": "nightly", "type": "backup", "status": "available", "labels": map[string]any{}}})
		case p == "/images/5" && r.Method == http.MethodPut:
			write(w, 200, map[string]any{"image": map[string]any{"id": 5, "description": body["description"], "type": body["type"], "status": "available"}})
		case p == "/zones" && r.Method == http.MethodPost:
			if body["mode"] == nil {
				apiErr(w, 422, "invalid_input", "invalid input in field 'mode': is required", nil)
				return
			}
			write(w, 201, map[string]any{"zone": map[string]any{"id": 8, "name": body["name"], "mode": body["mode"], "ttl": 3600}, "action": newAction("create_zone", 0)})
		case p == "/zones/EXAMPLE.COM." && r.Method == http.MethodGet:
			// the API resolves aliases of the zone name; the guard must match the name it answers with
			write(w, 200, map[string]any{"zone": map[string]any{"id": 7, "name": "example.com", "mode": "primary", "ttl": 3600}})
		case p == "/servers" && r.Method == http.MethodGet:
			var out []map[string]any
			for _, s := range servers {
				if n := q.Get("name"); n != "" && s["name"] != n {
					continue
				}
				if st := q["status"]; len(st) > 0 && s["status"] != st[0] {
					continue
				}
				out = append(out, s)
			}
			if out == nil {
				out = []map[string]any{}
			}
			total := len(out)
			write(w, 200, map[string]any{"servers": out, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 25, "previous_page": nil, "next_page": nil, "last_page": 1, "total_entries": total}}})
		case p == "/servers/actions" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"actions": []any{newAction("create_server", 0)}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 25, "total_entries": 1}}})
		case p == "/ssh_keys" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"ssh_keys": []any{map[string]any{"id": 42, "name": "k", "fingerprint": "ab:cd"}}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 25, "total_entries": 1}}})
		case p == "/ssh_keys/42" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"ssh_key": map[string]any{"id": 42, "name": "k", "fingerprint": "ab:cd"}})
		case p == "/servers" && r.Method == http.MethodPost:
			if body["name"] == "" || body["server_type"] == nil {
				apiErr(w, 422, "invalid_input", "invalid input in field 'name': is required", map[string]any{"fields": []any{map[string]any{"name": "name", "messages": []any{"is required"}}}})
				return
			}
			s := map[string]any{"id": 3, "name": body["name"], "status": "initializing", "server_type": map[string]any{"name": body["server_type"]}, "public_net": map[string]any{"ipv4": map[string]any{"ip": "203.0.113.30"}}, "labels": map[string]any{}}
			write(w, 201, map[string]any{"server": s, "action": newAction("create_server", 1, map[string]any{"id": 3, "type": "server"}), "next_actions": []any{newAction("start_server", 2)}, "root_password": "secret-root"})
		case strings.HasPrefix(p, "/servers/") && strings.Count(p, "/") == 2:
			id := strings.TrimPrefix(p, "/servers/")
			var s map[string]any
			for _, c := range servers {
				if fmt.Sprint(c["id"]) == id {
					s = c
				}
			}
			if s == nil {
				apiErr(w, 404, "not_found", "server not found", nil)
				return
			}
			switch r.Method {
			case http.MethodGet:
				write(w, 200, map[string]any{"server": s})
			case http.MethodPut:
				if f.conflict > 0 {
					f.conflict--
					apiErr(w, 409, "conflict", "the resource changed during the request, please retry", nil)
					return
				}
				cp := map[string]any{}
				for k, v := range s {
					cp[k] = v
				}
				for k, v := range body {
					cp[k] = v
				}
				write(w, 200, map[string]any{"server": cp})
			case http.MethodDelete:
				write(w, 200, map[string]any{"action": newAction("delete_server", 1, map[string]any{"id": s["id"], "type": "server"})})
			}
		case strings.HasPrefix(p, "/servers/") && strings.Contains(p, "/actions/") && r.Method == http.MethodPost:
			name := p[strings.LastIndex(p, "/")+1:]
			switch name {
			case "poweron", "reboot", "shutdown", "poweroff", "change_type", "rebuild", "attach_to_network":
				resp := map[string]any{"action": newAction(name+"_server", 1)}
				if name == "rebuild" {
					resp["root_password"] = "new-root"
				}
				write(w, 201, resp)
			default:
				apiErr(w, 404, "not_found", "unknown action", nil)
			}
		case strings.HasPrefix(p, "/servers/") && strings.HasSuffix(p, "/actions") && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"actions": []any{newAction("start_server", 0)}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 25, "total_entries": 1}}})
		case strings.HasPrefix(p, "/servers/") && strings.HasSuffix(p, "/metrics"):
			write(w, 200, map[string]any{"metrics": map[string]any{"start": q.Get("start"), "end": q.Get("end"), "step": 60, "time_series": map[string]any{"cpu": map[string]any{"values": []any{[]any{1.0, "0.5"}}}}}})
		case p == "/actions" && r.Method == http.MethodGet:
			var out []any
			for _, id := range q["id"] {
				var n int
				fmt.Sscan(id, &n)
				polls, ok := f.actions[n]
				if !ok {
					continue
				}
				status := "running"
				if polls <= 0 {
					status = "success"
				} else {
					f.actions[n] = polls - 1
				}
				out = append(out, map[string]any{"id": n, "command": "x", "status": status, "progress": 100, "started": "s", "finished": nil, "resources": []any{}, "error": nil})
			}
			if out == nil {
				out = []any{}
			}
			write(w, 200, map[string]any{"actions": out, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 50, "total_entries": len(out)}}})
		case p == "/actions/4711":
			write(w, 200, map[string]any{"action": map[string]any{"id": 4711, "command": "create_server", "status": "error", "progress": 100, "started": "s", "finished": "f", "resources": []any{}, "error": map[string]any{"code": "action_failed", "message": "it failed"}}})
		case p == "/pricing":
			write(w, 200, map[string]any{"pricing": map[string]any{"currency": "EUR", "vat_rate": "19.00", "volume": map[string]any{"price_per_gb_month": map[string]any{"net": "0.0440", "gross": "0.0524"}},
				"server_types": []any{
					map[string]any{"id": 1, "name": "cx23", "prices": []any{map[string]any{"location": "fsn1", "price_monthly": map[string]any{"net": "3.49"}}, map[string]any{"location": "nbg1", "price_monthly": map[string]any{"net": "3.49"}}}},
					map[string]any{"id": 2, "name": "cax11", "prices": []any{map[string]any{"location": "fsn1", "price_monthly": map[string]any{"net": "3.29"}}}},
				},
				"load_balancer_types": []any{map[string]any{"id": 1, "name": "lb11", "prices": []any{map[string]any{"location": "hel1", "price_monthly": map[string]any{"net": "5.39"}}}}},
			}})
		case p == "/zones" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"zones": []any{map[string]any{"id": 7, "name": "example.com", "mode": "primary", "ttl": 3600, "status": "ok", "record_count": 3}}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 25, "total_entries": 1}}})
		case p == "/zones/example.com" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"zone": map[string]any{"id": 7, "name": "example.com", "mode": "primary", "ttl": 3600}})
		case p == "/zones/7" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"zone": map[string]any{"id": 7, "name": "example.com", "mode": "primary", "ttl": 3600}})
		case p == "/zones/example.com/rrsets" && r.Method == http.MethodGet:
			var out []any
			for _, rs := range f.rrsets {
				out = append(out, rs)
			}
			write(w, 200, map[string]any{"rrsets": out, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 25, "total_entries": len(out)}}})
		case p == "/zones/example.com/rrsets" && r.Method == http.MethodPost:
			key := fmt.Sprint(body["name"]) + "/" + fmt.Sprint(body["type"])
			if _, exists := f.rrsets[key]; exists {
				apiErr(w, 409, "uniqueness_error", "rrset already exists", nil)
				return
			}
			rs := map[string]any{"id": key, "name": body["name"], "type": body["type"], "ttl": body["ttl"], "records": body["records"], "zone": 7}
			f.rrsets[key] = rs
			write(w, 201, map[string]any{"rrset": rs, "action": newAction("create_rrset", 0)})
		case strings.HasPrefix(p, "/zones/example.com/rrsets/"):
			rest := strings.TrimPrefix(p, "/zones/example.com/rrsets/")
			parts := strings.Split(rest, "/")
			key := parts[0] + "/" + parts[1]
			rs, exists := f.rrsets[key]
			if !exists {
				apiErr(w, 404, "not_found", "rrset not found", nil)
				return
			}
			switch {
			case len(parts) == 2 && r.Method == http.MethodGet:
				write(w, 200, map[string]any{"rrset": rs})
			case len(parts) == 2 && r.Method == http.MethodPut:
				for k, v := range body {
					rs[k] = v
				}
				write(w, 200, map[string]any{"rrset": rs})
			case len(parts) == 2 && r.Method == http.MethodDelete:
				delete(f.rrsets, key)
				write(w, 201, map[string]any{"action": newAction("delete_rrset", 0)})
			case len(parts) == 4 && parts[2] == "actions":
				switch parts[3] {
				case "set_records":
					rs["records"] = body["records"]
				case "change_ttl":
					if body["ttl"] == 61.0 {
						apiErr(w, 422, "invalid_input", "invalid input in field 'ttl'", nil)
						return
					}
					rs["ttl"] = body["ttl"]
				case "add_records":
				default:
					apiErr(w, 404, "not_found", "unknown action", nil)
					return
				}
				write(w, 201, map[string]any{"action": newAction(parts[3], 0)})
			}
		case p == "/zones/example.com" && r.Method == http.MethodDelete:
			write(w, 201, map[string]any{"action": newAction("delete_zone", 0)})
		case p == "/ssh_keys" && r.Method == http.MethodPost:
			write(w, 201, map[string]any{"ssh_key": map[string]any{"id": 42, "name": body["name"], "fingerprint": "ab:cd"}})
		case p == "/firewalls" && r.Method == http.MethodPost:
			write(w, 201, map[string]any{"firewall": map[string]any{"id": 8, "name": body["name"]}, "actions": []any{}})
		case p == "/volumes" && r.Method == http.MethodPost:
			write(w, 201, map[string]any{"volume": map[string]any{"id": 9}, "action": newAction("create_volume", 0), "next_actions": []any{}})
		case p == "/ssh_keys/42" && r.Method == http.MethodDelete:
			w.Header().Set("RateLimit-Limit", "3600")
			w.Header().Set("RateLimit-Remaining", "3499")
			w.WriteHeader(204)
		case p == "/locations" && r.Method == http.MethodGet:
			write(w, 200, map[string]any{"locations": []any{map[string]any{"id": 1, "name": "fsn1"}}, "meta": map[string]any{"pagination": map[string]any{"page": 1, "per_page": 2, "total_entries": 1}}})
		case p == "/ratelimited":
			w.Header().Set("RateLimit-Reset", "1800000000")
			apiErr(w, 429, "rate_limit_exceeded", "rate limit exceeded", nil)
		case p == "/unknown_things" && r.Method == http.MethodPost:
			write(w, 201, map[string]any{"thing": map[string]any{"id": 1}})
		default:
			apiErr(w, 404, "not_found", "no route "+r.Method+" "+p, nil)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *hcFake) last() hcFakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *hcFake) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

func newTestHetznerCloud(t *testing.T, f *hcFake, mod func(*config.Connection)) *HetznerCloud {
	t.Helper()
	conn := config.Connection{Name: "hz", Type: config.TypeHetznerCloud, Token: "tok_test", URL: f.srv.URL}
	if mod != nil {
		mod(&conn)
	}
	h, err := NewHetznerCloud(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.poll = []time.Duration{time.Millisecond}
	return h
}

func hcDecode(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	text := resultText(t, res)
	if i := strings.Index(text, "\n\nrate limit:"); i >= 0 {
		text = text[:i]
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out
}

func TestHetznerCloud_New(t *testing.T) {
	if _, err := NewHetznerCloud(config.Connection{Name: "x", Type: config.TypeHetznerCloud}, nil); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("missing token: %v", err)
	}
	if _, err := NewHetznerCloud(config.Connection{Name: "x", Type: config.TypeHetznerCloud, Token: "t", ProtectedResources: []string{"sever:x"}}, nil); err == nil {
		t.Error("bad guard accepted")
	}
	h, err := NewHetznerCloud(config.Connection{Name: "x", Type: config.TypeHetznerCloud, Token: "t"}, nil)
	if err != nil || h.baseURL != config.HetznerCloudDefaultURL {
		t.Errorf("default URL: %v %q", err, h.baseURL)
	}
}

func TestHetznerCloud_ToolsByMode(t *testing.T) {
	ro := (&HetznerCloud{readOnly: true}).Tools()
	rw := (&HetznerCloud{}).Tools()
	names := func(defs []ToolDef) map[string]bool {
		m := map[string]bool{}
		for _, d := range defs {
			m[d.Tool.Name] = true
		}
		return m
	}
	roNames, rwNames := names(ro), names(rw)
	for _, n := range []string{"list", "get", "list_actions", "get_action", "wait_action", "metrics", "pricing", "list_rrsets", "get_rrset", "zonefile", "api_get"} {
		if !roNames[n] {
			t.Errorf("read-only connection lacks %s", n)
		}
	}
	for _, n := range []string{"create", "update", "delete", "action", "set_records", "create_rrset", "update_rrset", "delete_rrset", "rrset_action", "import_zonefile", "api_request"} {
		if roNames[n] {
			t.Errorf("read-only connection registers %s", n)
		}
		if !rwNames[n] {
			t.Errorf("writing connection lacks %s", n)
		}
	}
	cond := hetznerCloudConditionalToolNames("hz")
	if len(cond) != len(rw)-len(ro) {
		t.Errorf("conditional tools = %d, want %d", len(cond), len(rw)-len(ro))
	}
	for _, d := range rw {
		if len(d.Tool.Description) > 6000 {
			t.Errorf("%s: description is %d characters", d.Tool.Name, len(d.Tool.Description))
		}
	}
	// The generated descriptions name every creatable kind and every action.
	desc := map[string]string{}
	for _, d := range rw {
		desc[d.Tool.Name] = d.Tool.Description
	}
	for i := range hcResources {
		r := &hcResources[i]
		if r.Create != nil && !strings.Contains(desc["create"], r.Kind+":") && !strings.Contains(desc["create"], r.Kind+"†:") {
			t.Errorf("create description does not list %s", r.Kind)
		}
		for _, a := range r.Actions {
			if !strings.Contains(desc["action"], a.Name) {
				t.Errorf("action description does not list %s %s", r.Kind, a.Name)
			}
		}
	}
}

func TestHetznerCloud_ListCompactAndFilters(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, nil)
	ctx := context.Background()

	res, err := h.handleList(ctx, toolRequest(map[string]any{"resource": "server", "filters": map[string]any{"status": "running"}, "sort": "name:desc,created"}))
	if err != nil || res.IsError {
		t.Fatalf("list: %v / %s", err, resultText(t, res))
	}
	out := hcDecode(t, res)
	if out["count"] != 1.0 || out["total_entries"] != 1.0 {
		t.Errorf("count/total = %v/%v", out["count"], out["total_entries"])
	}
	rows := out["servers"].([]any)
	row := rows[0].(map[string]any)
	if row["name"] != "my-cell-01" || row["server_type"] != "cpx32" || row["location"] != "fsn1" || row["ipv4"] != "203.0.113.10" {
		t.Errorf("compact row = %v", row)
	}
	if _, ok := row["public_net"]; ok {
		t.Error("compact row carries the full public_net")
	}
	last := f.last()
	if !strings.Contains(last.Query, "status=running") || !strings.Contains(last.Query, "sort=name%3Adesc") || !strings.Contains(last.Query, "sort=created") {
		t.Errorf("query = %s", last.Query)
	}

	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "server", "filters": map[string]any{"colour": "red"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), `no "colour" filter`) {
		t.Errorf("unknown filter: %s", resultText(t, res))
	}
	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "ssh_key", "sort": "name"}))
	if res.IsError {
		t.Errorf("ssh_key sort refused: %s", resultText(t, res))
	}
	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "server_type", "sort": "name"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "cannot be sorted") {
		t.Errorf("server_type sort: %s", resultText(t, res))
	}
	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "droplet"}))
	if !res.IsError {
		t.Error("unknown resource accepted")
	}

	res, err = h.handleList(ctx, toolRequest(map[string]any{"resource": "server", "full": true, "per_page": float64(99)}))
	if err != nil || res.IsError {
		t.Fatal(resultText(t, res))
	}
	full := hcDecode(t, res)["servers"].([]any)[0].(map[string]any)
	if _, ok := full["public_net"]; !ok {
		t.Error("full row lacks public_net")
	}
	if !strings.Contains(f.last().Query, "per_page=50") {
		t.Errorf("per_page not capped: %s", f.last().Query)
	}
}

func TestHetznerCloud_GetByNameAndID(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, nil)
	ctx := context.Background()

	res, err := h.handleGet(ctx, toolRequest(map[string]any{"resource": "server", "id": "scratch-01"}))
	if err != nil || res.IsError {
		t.Fatalf("get by name: %v / %s", err, resultText(t, res))
	}
	if hcDecode(t, res)["server"].(map[string]any)["id"] != 2.0 {
		t.Errorf("wrong server: %s", resultText(t, res))
	}
	if last := f.last(); last.Path != "/v1/servers" || !strings.Contains(last.Query, "name=scratch-01") {
		t.Errorf("lookup = %s?%s", last.Path, last.Query)
	}
	res, _ = h.handleGet(ctx, toolRequest(map[string]any{"resource": "server", "id": "1"}))
	if res.IsError || f.last().Path != "/v1/servers/1" {
		t.Errorf("get by id: %s / %s", resultText(t, res), f.last().Path)
	}
	res, _ = h.handleGet(ctx, toolRequest(map[string]any{"resource": "server", "id": "nope"}))
	if !res.IsError || !strings.Contains(resultText(t, res), `no server named "nope"`) {
		t.Errorf("unknown name: %s", resultText(t, res))
	}
	res, _ = h.handleGet(ctx, toolRequest(map[string]any{"resource": "zone", "id": "example.com"}))
	if res.IsError || f.last().Path != "/v1/zones/example.com" {
		t.Errorf("zone by name: %s / %s", resultText(t, res), f.last().Path)
	}
	res, _ = h.handleGet(ctx, toolRequest(map[string]any{"resource": "server", "id": "999"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "Hetzner API 404 not_found") {
		t.Errorf("404: %s", resultText(t, res))
	}
}

func TestHetznerCloud_Guards(t *testing.T) {
	f := newHCFake(t)
	ctx := context.Background()

	// Writing connection without lifecycle, the cell host protected.
	h := newTestHetznerCloud(t, f, func(c *config.Connection) { c.ProtectedResources = []string{"server:my-cell-*"} })

	res, _ := h.handleCreate(ctx, toolRequest(map[string]any{"resource": "server", "body": map[string]any{"name": "x", "server_type": "cx23", "image": "debian-12"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle is off") {
		t.Errorf("create server without lifecycle: %s", resultText(t, res))
	}
	if f.count(http.MethodPost, "/v1/servers") != 0 {
		t.Error("a refused create reached the API")
	}
	res, _ = h.handleCreate(ctx, toolRequest(map[string]any{"resource": "ssh_key", "body": map[string]any{"name": "k", "public_key": "ssh-ed25519 AAAA"}}))
	if res.IsError {
		t.Errorf("create ssh_key (free) refused: %s", resultText(t, res))
	}
	res, _ = h.handleDelete(ctx, toolRequest(map[string]any{"resource": "ssh_key", "id": "42"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "deleting a ssh_key is refused") {
		t.Errorf("delete without lifecycle: %s", resultText(t, res))
	}
	res, _ = h.handleAction(ctx, toolRequest(map[string]any{"resource": "server", "id": "2", "action": "poweroff"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "poweroff on a server is refused") {
		t.Errorf("poweroff without lifecycle: %s", resultText(t, res))
	}
	res, _ = h.handleAction(ctx, toolRequest(map[string]any{"resource": "server", "id": "scratch-01", "action": "poweron"}))
	if res.IsError {
		t.Errorf("poweron refused: %s", resultText(t, res))
	}
	res, _ = h.handleAction(ctx, toolRequest(map[string]any{"resource": "server", "id": "2", "action": "rebuild", "params": map[string]any{}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "needs image") {
		t.Errorf("required action param: %s", resultText(t, res))
	}
	res, _ = h.handleUpdate(ctx, toolRequest(map[string]any{"resource": "image", "id": "5", "body": map[string]any{"type": "snapshot"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("image type conversion is lifecycle: %s", resultText(t, res))
	}

	// Protected: by name, by id (resolved with a GET), through the pass-through, through the zone.
	res, _ = h.handleUpdate(ctx, toolRequest(map[string]any{"resource": "server", "id": "my-cell-01", "body": map[string]any{"labels": map[string]any{"a": "b"}}}))
	if !res.IsError || !strings.Contains(resultText(t, res), `server "my-cell-01" is protected`) {
		t.Errorf("update protected by name: %s", resultText(t, res))
	}
	res, _ = h.handleAction(ctx, toolRequest(map[string]any{"resource": "server", "id": "1", "action": "reboot"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "is protected") {
		t.Errorf("action on protected by id: %s", resultText(t, res))
	}
	if f.count(http.MethodPost, "/v1/servers/1/actions/reboot") != 0 {
		t.Error("a refused action reached the API")
	}
	res, _ = h.handleUpdate(ctx, toolRequest(map[string]any{"resource": "server", "id": "2", "body": map[string]any{"name": "scratch-02"}}))
	if res.IsError {
		t.Errorf("update of an unprotected server refused: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "PUT", "path": "/v1/servers/1", "body": map[string]any{"name": "x"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "is protected") {
		t.Errorf("pass-through on protected: %s", resultText(t, res))
	}
	h2 := newTestHetznerCloud(t, f, func(c *config.Connection) { c.ProtectedResources = []string{"zone:example.com"} })
	res, _ = h2.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "records": []any{"203.0.113.1"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), `zone "example.com" is protected`) {
		t.Errorf("records of a protected zone: %s", resultText(t, res))
	}
	res, _ = h2.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "7", "name": "www", "type": "A", "records": []any{"203.0.113.1"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "is protected") {
		t.Errorf("records of a protected zone by id: %s", resultText(t, res))
	}

	// Snapshots and backups have no name: an image guard matches their
	// description.
	hi := newTestHetznerCloud(t, f, func(c *config.Connection) { c.AllowLifecycle = true; c.ProtectedResources = []string{"image:night*"} })
	res, _ = hi.handleDelete(ctx, toolRequest(map[string]any{"resource": "image", "id": "5"}))
	if !res.IsError || !strings.Contains(resultText(t, res), `image "nightly" is protected`) {
		t.Errorf("snapshot guarded by description: %s", resultText(t, res))
	}
	res, _ = hi.handleUpdate(ctx, toolRequest(map[string]any{"resource": "image", "id": "5", "body": map[string]any{"Type": "snapshot"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "is protected") {
		t.Errorf("snapshot update guarded: %s", resultText(t, res))
	}
	res, _ = newTestHetznerCloud(t, f, nil).handleUpdate(ctx, toolRequest(map[string]any{"resource": "image", "id": "5", "body": map[string]any{"Type": "snapshot"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("lifecycle key matched case-insensitively: %s", resultText(t, res))
	}

	// Lifecycle allowed: the switch opens the operations, the guard still holds.
	h3 := newTestHetznerCloud(t, f, func(c *config.Connection) { c.AllowLifecycle = true; c.ProtectedResources = []string{"my-cell-01"} })
	res, _ = h3.handleDelete(ctx, toolRequest(map[string]any{"resource": "server", "id": "1"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "is protected") {
		t.Errorf("delete protected with lifecycle: %s", resultText(t, res))
	}
	res, _ = h3.handleDelete(ctx, toolRequest(map[string]any{"resource": "server", "id": "scratch-01"}))
	if res.IsError {
		t.Fatalf("delete with lifecycle: %s", resultText(t, res))
	}
	out := hcDecode(t, res)
	if out["deleted"] != "server" || out["name"] != "scratch-01" || out["actions"].([]any)[0].(map[string]any)["status"] != "success" {
		t.Errorf("delete result = %s", resultText(t, res))
	}
	res, _ = h3.handleDelete(ctx, toolRequest(map[string]any{"resource": "ssh_key", "id": "42"}))
	if res.IsError || hcDecode(t, res)["deleted"] != "ssh_key" {
		t.Errorf("delete 204: %s", resultText(t, res))
	}

	// Read-only: the handlers refuse too, not only the tool list.
	h4 := newTestHetznerCloud(t, f, func(c *config.Connection) { c.ReadOnly = true })
	res, _ = h4.handleAction(ctx, toolRequest(map[string]any{"resource": "server", "id": "2", "action": "poweron"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "read-only") {
		t.Errorf("read-only action: %s", resultText(t, res))
	}
	if h4.allowLifecycle {
		t.Error("read-only connection reports lifecycle")
	}
}

func TestHetznerCloud_CreateAndActionsWait(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, func(c *config.Connection) { c.AllowLifecycle = true })
	ctx := context.Background()

	res, err := h.handleCreate(ctx, toolRequest(map[string]any{"resource": "server", "body": map[string]any{"name": "new-01", "server_type": "cx23", "image": "debian-12"}}))
	if err != nil || res.IsError {
		t.Fatalf("create: %v / %s", err, resultText(t, res))
	}
	out := hcDecode(t, res)
	if out["created"] != "server" || out["root_password"] != "secret-root" || out["server"].(map[string]any)["id"] != 3.0 {
		t.Errorf("create result = %s", resultText(t, res))
	}
	actions := out["actions"].([]any)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want create + start", len(actions))
	}
	for _, a := range actions {
		if a.(map[string]any)["status"] != "success" {
			t.Errorf("action not waited to success: %v", a)
		}
	}
	if f.count(http.MethodGet, "/v1/actions") < 2 {
		t.Errorf("polled %d times", f.count(http.MethodGet, "/v1/actions"))
	}

	// Without wait the running action comes back as is.
	res, _ = h.handleAction(ctx, toolRequest(map[string]any{"resource": "server", "id": "2", "action": "rebuild", "params": map[string]any{"image": "debian-12"}, "wait": false}))
	out = hcDecode(t, res)
	if out["root_password"] != "new-root" || out["actions"].([]any)[0].(map[string]any)["status"] != "running" {
		t.Errorf("unwaited action = %s", resultText(t, res))
	}

	// A timeout leaves a note with the ids for wait_action.
	f.mu.Lock()
	f.actions[99] = 1000
	f.mu.Unlock()
	h.poll = []time.Duration{50 * time.Millisecond}
	res, _ = h.handleWaitAction(ctx, toolRequest(map[string]any{"ids": []any{float64(99)}, "timeout": float64(0.12)}))
	out = hcDecode(t, res)
	if note, _ := out["note"].(string); !strings.Contains(note, "still running") || !strings.Contains(note, "[99]") {
		t.Errorf("timeout note = %s", resultText(t, res))
	}
	res, _ = h.handleWaitAction(ctx, toolRequest(map[string]any{"ids": []any{float64(123456)}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "unknown ids: 123456") {
		t.Errorf("unknown action: %s", resultText(t, res))
	}

	// A failed action marks the result as an error.
	res, _ = h.handleGetAction(ctx, toolRequest(map[string]any{"id": float64(4711)}))
	if res.IsError || hcDecode(t, res)["action"].(map[string]any)["status"] != "error" {
		t.Errorf("get_action = %s", resultText(t, res))
	}

	// list_actions by resource, by resource+id, by ids; and the project-wide list needs ids.
	for _, args := range []map[string]any{{"resource": "server"}, {"resource": "server", "id": "scratch-01"}, {"ids": []any{float64(1001)}}} {
		res, _ = h.handleListActions(ctx, toolRequest(args))
		if res.IsError {
			t.Errorf("list_actions %v: %s", args, resultText(t, res))
		}
	}
	res, _ = h.handleListActions(ctx, toolRequest(map[string]any{}))
	if !res.IsError {
		t.Error("project-wide list without ids accepted")
	}
}

func TestHetznerCloud_RecordsAndZones(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, nil)
	ctx := context.Background()

	// set_records on an existing set: set_records, and change_ttl because the TTL differs.
	res, err := h.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "a", "records": []any{"203.0.113.5", map[string]any{"value": "203.0.113.6", "comment": "second"}}, "ttl": float64(60)}))
	if err != nil || res.IsError {
		t.Fatalf("set_records: %v / %s", err, resultText(t, res))
	}
	out := hcDecode(t, res)
	if out["replaced"] != "rrset" || len(out["actions"].([]any)) != 2 {
		t.Errorf("set_records result = %s", resultText(t, res))
	}
	if f.count(http.MethodPost, "/v1/zones/example.com/rrsets/www/A/actions/set_records") != 1 || f.count(http.MethodPost, "/v1/zones/example.com/rrsets/www/A/actions/change_ttl") != 1 {
		t.Error("set_records did not call set_records + change_ttl once each")
	}
	f.mu.Lock()
	recs := f.rrsets["www/A"]["records"].([]any)
	f.mu.Unlock()
	if len(recs) != 2 || recs[1].(map[string]any)["comment"] != "second" {
		t.Errorf("records stored = %v", recs)
	}

	// A TTL outside the range is refused before anything is written; a
	// change_ttl that fails after the records were replaced is reported next
	// to the set_records action, as an error result.
	before := f.count(http.MethodPost, "/v1/zones/example.com/rrsets/www/A/actions/set_records")
	res, _ = h.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "records": []any{"203.0.113.9"}, "ttl": float64(30)}))
	if !res.IsError || !strings.Contains(resultText(t, res), "between 60 and") || f.count(http.MethodPost, "/v1/zones/example.com/rrsets/www/A/actions/set_records") != before {
		t.Errorf("ttl 30: %s", resultText(t, res))
	}
	res, _ = h.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "records": []any{"203.0.113.9"}, "ttl": float64(61)}))
	if !res.IsError {
		t.Fatalf("failed change_ttl not flagged: %s", resultText(t, res))
	}
	out = hcDecode(t, res)
	if out["replaced"] != "rrset" || len(out["actions"].([]any)) != 1 || !strings.Contains(out["note"].(string), "records were replaced, the TTL was not changed") || !strings.Contains(out["note"].(string), "422") {
		t.Errorf("failed change_ttl result = %s", resultText(t, res))
	}

	// set_records on a missing set creates it with the TTL.
	res, _ = h.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "api", "type": "AAAA", "records": []any{"2001:db8::1"}, "ttl": float64(120)}))
	if res.IsError || hcDecode(t, res)["created"] != "rrset" {
		t.Errorf("set_records create = %s", resultText(t, res))
	}
	if f.last().Body["ttl"] != 120.0 || f.last().Body["type"] != "AAAA" {
		t.Errorf("create body = %v", f.last().Body)
	}

	// Same TTL: no change_ttl call.
	before = f.count(http.MethodPost, "/v1/zones/example.com/rrsets/api/AAAA/actions/change_ttl")
	res, _ = h.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "api", "type": "AAAA", "records": []any{"2001:db8::2"}, "ttl": float64(120)}))
	if res.IsError || f.count(http.MethodPost, "/v1/zones/example.com/rrsets/api/AAAA/actions/change_ttl") != before {
		t.Errorf("change_ttl called for an equal TTL: %s", resultText(t, res))
	}

	res, _ = h.handleCreateRRSet(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "records": []any{"1.1.1.1"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "uniqueness_error") {
		t.Errorf("create existing rrset: %s", resultText(t, res))
	}
	res, _ = h.handleRRSetAction(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "action": "add_records", "params": map[string]any{"records": []any{"203.0.113.7"}}}))
	if res.IsError {
		t.Errorf("rrset_action add_records: %s", resultText(t, res))
	}
	if recs, _ := f.last().Body["records"].([]any); len(recs) != 1 || recs[0].(map[string]any)["value"] != "203.0.113.7" {
		t.Errorf("add_records body = %v", f.last().Body)
	}
	res, _ = h.handleRRSetAction(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "action": "change_ttl", "params": map[string]any{}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "needs ttl") {
		t.Errorf("rrset_action required param: %s", resultText(t, res))
	}
	res, _ = h.handleRRSetAction(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "action": "explode"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "no action") {
		t.Errorf("unknown rrset action: %s", resultText(t, res))
	}
	res, _ = h.handleGetRRSet(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "MX"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "404") {
		t.Errorf("get missing rrset: %s", resultText(t, res))
	}
	res, _ = h.handleGetRRSet(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "SPF"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "not one of") {
		t.Errorf("bad record type: %s", resultText(t, res))
	}
	res, _ = h.handleListRRSets(ctx, toolRequest(map[string]any{"zone": "example.com", "type": []any{"a"}}))
	if res.IsError || !strings.Contains(f.last().Query, "type=A") {
		t.Errorf("list_rrsets: %s / %s", resultText(t, res), f.last().Query)
	}
	res, _ = h.handleDeleteRRSet(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "api", "type": "AAAA"}))
	if res.IsError || hcDecode(t, res)["deleted"] != "rrset" {
		t.Errorf("delete_rrset (a plain write): %s", resultText(t, res))
	}

	// Zone delete and zone file import are lifecycle operations; zone create is not.
	res, _ = h.handleDelete(ctx, toolRequest(map[string]any{"resource": "zone", "id": "example.com"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("delete zone without lifecycle: %s", resultText(t, res))
	}
	res, _ = h.handleImportZonefile(ctx, toolRequest(map[string]any{"zone": "example.com", "zonefile": "$ORIGIN example.com.\n"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("import without lifecycle: %s", resultText(t, res))
	}
	res, _ = h.handleCreate(ctx, toolRequest(map[string]any{"resource": "zone", "body": map[string]any{"name": "new.example", "mode": "primary"}}))
	if res.IsError || hcDecode(t, res)["created"] != "zone" {
		t.Errorf("zone create (a plain write): %s", resultText(t, res))
	}
	before = f.count(http.MethodPost, "/v1/zones")
	res, _ = h.handleCreate(ctx, toolRequest(map[string]any{"resource": "zone", "body": map[string]any{"name": "new.example"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "needs mode") || f.count(http.MethodPost, "/v1/zones") != before {
		t.Errorf("zone create without mode: %s", resultText(t, res))
	}
	res, _ = h.handleRRSetAction(ctx, toolRequest(map[string]any{"zone": "example.com", "name": "www", "type": "A", "action": "update_records", "params": map[string]any{"records": []any{"203.0.113.7"}}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "comment") {
		t.Errorf("update_records without comments: %s", resultText(t, res))
	}
	for _, bad := range []map[string]any{{"zone": "example.com/rrsets/www/A", "name": "x", "type": "A"}, {"zone": "example.com", "name": "a b", "type": "A"}, {"zone": "..", "name": "x", "type": "A"}} {
		res, _ = h.handleGetRRSet(ctx, toolRequest(bad))
		if !res.IsError || !(strings.Contains(resultText(t, res), "path") || strings.Contains(resultText(t, res), "dot segment")) {
			t.Errorf("path value %v accepted: %s", bad, resultText(t, res))
		}
	}

	// list_actions on one resource refuses ids whatever the resource's name
	// looks like, and the project-wide list carries ids only.
	res, _ = h.handleListActions(ctx, toolRequest(map[string]any{"resource": "zone", "id": "example.es", "ids": []any{float64(1)}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "cannot be combined") {
		t.Errorf("ids with a resource id: %s", resultText(t, res))
	}
	res, _ = h.handleListActions(ctx, toolRequest(map[string]any{"ids": []any{float64(1001)}, "status": []any{"running"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "no effect with ids alone") {
		t.Errorf("ids with status: %s", resultText(t, res))
	}
	res, _ = h.handleListActions(ctx, toolRequest(map[string]any{"ids": []any{float64(1001)}}))
	if res.IsError || f.last().Query != "id=1001" {
		t.Errorf("project-wide list query = %q (%s)", f.last().Query, resultText(t, res))
	}
	res, _ = h.handleGetAction(ctx, toolRequest(map[string]any{"id": "4711"}))
	if res.IsError || f.last().Path != "/v1/actions/4711" {
		t.Errorf("get_action with a string id: %s", resultText(t, res))
	}
	res, _ = h.handleWaitAction(ctx, toolRequest(map[string]any{"ids": []any{float64(1001), float64(1001)}}))
	if res.IsError {
		t.Errorf("duplicate ids: %s", resultText(t, res))
	}

	// A zone id that would change the path's shape is refused by the generic
	// tools too.
	res, _ = h.handleAction(ctx, toolRequest(map[string]any{"resource": "zone", "id": "example.com/rrsets/www/A", "action": "change_ttl", "params": map[string]any{"ttl": 60}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "cannot stand in a path") {
		t.Errorf("zone id with a slash: %s", resultText(t, res))
	}

	// A protected zone stays protected under an alias of its name: the guard
	// reads the zone and matches the name the API answers with.
	hz := newTestHetznerCloud(t, f, func(c *config.Connection) { c.ProtectedResources = []string{"zone:example.com"} })
	res, _ = hz.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "EXAMPLE.COM.", "name": "www", "type": "A", "records": []any{"203.0.113.1"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), `zone "example.com" is protected`) {
		t.Errorf("alias of a protected zone: %s", resultText(t, res))
	}
	res, _ = hz.handleSetRecords(ctx, toolRequest(map[string]any{"zone": "nope.example", "name": "www", "type": "A", "records": []any{"203.0.113.1"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "404") || strings.Contains(resultText(t, res), "guard") {
		t.Errorf("unknown zone under guards reports the 404, not a guard failure: %s", resultText(t, res))
	}
}

func TestHetznerCloud_ListAllPaging(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, nil)
	ctx := context.Background()

	res, err := h.handleList(ctx, toolRequest(map[string]any{"resource": "image", "all": true}))
	if err != nil || res.IsError {
		t.Fatalf("list all: %v / %s", err, resultText(t, res))
	}
	out := hcDecode(t, res)
	if out["count"] != float64(3*hcPerPageMax) || out["total_entries"] != float64(3*hcPerPageMax) || out["note"] != nil {
		t.Errorf("all = count %v total %v note %v", out["count"], out["total_entries"], out["note"])
	}
	if f.count(http.MethodGet, "/v1/images") != 3 || !strings.Contains(f.last().Query, "page=3") || !strings.Contains(f.last().Query, "per_page=50") {
		t.Errorf("paging requests = %d, last query %s", f.count(http.MethodGet, "/v1/images"), f.last().Query)
	}

	// One page by default, with the paging figures.
	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "image", "page": float64(2), "per_page": float64(10)}))
	out = hcDecode(t, res)
	if out["count"] != 10.0 || out["page"] != 2.0 || out["next_page"] != 3.0 || out["last_page"] != 3.0 {
		t.Errorf("one page = %s", resultText(t, res))
	}

	// A listing that never ends stops at the cap and says so.
	f.mu.Lock()
	f.imagePages = 1000
	f.mu.Unlock()
	saved := hcMaxPages
	hcMaxPages = 3
	t.Cleanup(func() { hcMaxPages = saved })
	before := f.count(http.MethodGet, "/v1/images")
	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "image", "all": true}))
	out = hcDecode(t, res)
	if note, _ := out["note"].(string); !strings.Contains(note, "stopped after 3 pages") || out["count"] != float64(3*hcPerPageMax) {
		t.Errorf("cap = count %v note %q", out["count"], out["note"])
	}
	if f.count(http.MethodGet, "/v1/images")-before != 3 {
		t.Errorf("cap requests = %d", f.count(http.MethodGet, "/v1/images")-before)
	}
}

func TestConnectionAddHetznerCloud(t *testing.T) {
	isolateConfigDir(t)
	ct := NewConfigTools(&config.Config{}, nil)
	add := func(args map[string]any) (string, bool) {
		t.Helper()
		res, err := ct.handleConnectionAdd(context.Background(), toolRequest(args))
		if err != nil {
			t.Fatal(err)
		}
		return resultText(t, res), res.IsError
	}
	if text, isErr := add(map[string]any{"name": "db", "type": config.TypeMariaDB, "host": "x", "allow_lifecycle": true}); !isErr || !strings.Contains(text, "allow_lifecycle applies to type") {
		t.Errorf("hetzner field on another type: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "h0", "type": config.TypeHetznerCloud, "read_only": true, "allow_lifecycle": true}); !isErr || !strings.Contains(text, "allow_lifecycle") {
		t.Errorf("read_only with allow_lifecycle: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "h1", "type": config.TypeHetznerCloud, "protected_resources": []any{"server:prod-*", 7}}); !isErr || !strings.Contains(text, "not a string") {
		t.Errorf("list with a number: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "h1", "type": config.TypeHetznerCloud, "protected_resources": map[string]any{"a": "b"}}); !isErr || !strings.Contains(text, "list of strings") {
		t.Errorf("object instead of a list: %q", text)
	}
	if text, isErr := add(map[string]any{"name": "h1", "type": config.TypeHetznerCloud, "protected_resources": []any{"sever:x"}}); !isErr || !strings.Contains(text, "unknown kind") {
		t.Errorf("bad kind in the list: %q", text)
	}
	text, isErr := add(map[string]any{"name": "h2", "type": config.TypeHetznerCloud, "protected_resources": []any{"server:prod-*", " zone:example.com "}, "allow_lifecycle": true})
	if isErr {
		t.Fatal(text)
	}
	if !strings.Contains(text, "h2-token") {
		t.Errorf("no hint to store the token: %q", text)
	}
	c := ct.cfg.FindAnyConnection("h2")
	if c == nil || !c.AllowLifecycle || c.URL != config.HetznerCloudDefaultURL || len(c.ProtectedResources) != 2 || c.ProtectedResources[1] != "zone:example.com" {
		t.Errorf("stored connection = %+v", c)
	}
	text, isErr = add(map[string]any{"name": "h3", "type": config.TypeHetznerCloud, "protected_resources": "my-cell-01\nserver:prod-*\n", "url": "https://api.hetzner.cloud/"})
	if isErr {
		t.Fatal(text)
	}
	c = ct.cfg.FindAnyConnection("h3")
	if c == nil || c.AllowLifecycle || len(c.ProtectedResources) != 2 || c.URL != "https://api.hetzner.cloud/" {
		t.Errorf("stored connection = %+v", c)
	}
}

func TestHetznerCloud_PassThrough(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, nil)
	ctx := context.Background()

	res, err := h.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v1/locations?per_page=2"}))
	if err != nil || res.IsError || !strings.Contains(resultText(t, res), "HTTP 200") || f.last().Query != "per_page=2" {
		t.Errorf("api_get: %v / %s / %s", err, resultText(t, res), f.last().Query)
	}
	for _, bad := range []string{"", "/servers", "/v1/../x", "/v1/servers%2F1", "/v1/servers/{id}"} {
		res, _ = h.handleAPIGet(ctx, toolRequest(map[string]any{"path": bad}))
		if !res.IsError {
			t.Errorf("api_get accepted %q", bad)
		}
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "GET", "path": "/v1/servers"}))
	if !res.IsError {
		t.Error("api_request GET accepted")
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/servers", "body": map[string]any{"name": "x"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("pass-through create server: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "DELETE", "path": "/v1/ssh_keys/42"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("pass-through delete: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/servers/2/actions/poweroff"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("pass-through poweroff: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/servers/2/actions/reboot"}))
	if res.IsError || !strings.Contains(resultText(t, res), "HTTP 201") {
		t.Errorf("pass-through reboot: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/unknown_things", "body": map[string]any{"a": 1}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "not in this build's registry") {
		t.Errorf("unknown write without lifecycle: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/zones/example.com/rrsets/www/A/actions/set_records", "body": map[string]any{"records": []any{map[string]any{"value": "1.2.3.4"}}}}))
	if res.IsError {
		t.Errorf("pass-through rrset write: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "PUT", "path": "/v1/images/5", "body": map[string]any{"type": "snapshot"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("pass-through backup→snapshot conversion without lifecycle: %s", resultText(t, res))
	}
	if f.count(http.MethodPut, "/v1/images/5") != 0 {
		t.Error("the refused image PUT reached the API")
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "PUT", "path": "/v1/images/5", "body": map[string]any{"description": "renamed"}}))
	if res.IsError || !strings.Contains(resultText(t, res), "HTTP 200") {
		t.Errorf("pass-through image description: %s", resultText(t, res))
	}
	res, _ = h.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/zones/example.com/actions/import_zonefile", "body": map[string]any{"zonefile": "x"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "allow_lifecycle") {
		t.Errorf("pass-through import: %s", resultText(t, res))
	}

	h2 := newTestHetznerCloud(t, f, func(c *config.Connection) { c.AllowLifecycle = true })
	res, _ = h2.handleAPIRequest(ctx, toolRequest(map[string]any{"method": "POST", "path": "/v1/unknown_things", "body": map[string]any{"a": 1}}))
	if res.IsError || !strings.Contains(resultText(t, res), "HTTP 201") {
		t.Errorf("unknown write with lifecycle: %s", resultText(t, res))
	}
	if string(f.last().Auth) != "Bearer tok_test" || f.last().Body["a"] != 1.0 {
		t.Errorf("pass-through request = %+v", f.last())
	}
}

func TestHetznerCloud_Classify(t *testing.T) {
	cases := []struct {
		method, path string
		class        hcClass
		known        bool
		id           string
	}{
		{"GET", "/v1/servers", hcRead, true, ""},
		{"POST", "/v1/servers", hcLifecycle, true, ""},
		{"POST", "/v1/firewalls", hcWrite, true, ""},
		{"POST", "/v1/images", hcRead, false, ""},
		{"PUT", "/v1/servers/42", hcWrite, true, "42"},
		{"DELETE", "/v1/servers/42", hcLifecycle, true, "42"},
		{"DELETE", "/v1/locations/1", hcRead, false, "1"},
		{"POST", "/v1/servers/42/actions/poweron", hcWrite, true, "42"},
		{"POST", "/v1/servers/42/actions/rebuild", hcLifecycle, true, "42"},
		{"POST", "/v1/servers/42/actions/levitate", hcRead, false, "42"},
		{"POST", "/v1/volumes/9/actions/resize", hcLifecycle, true, "9"},
		{"POST", "/v1/zones", hcWrite, true, ""},
		{"DELETE", "/v1/zones/example.com", hcLifecycle, true, "example.com"},
		{"POST", "/v1/zones/example.com/actions/import_zonefile", hcLifecycle, true, "example.com"},
		{"POST", "/v1/zones/example.com/rrsets", hcWrite, true, "example.com"},
		{"PUT", "/v1/zones/example.com/rrsets/www/A", hcWrite, true, "example.com"},
		{"DELETE", "/v1/zones/example.com/rrsets/www/A", hcWrite, true, "example.com"},
		{"POST", "/v1/zones/example.com/rrsets/www/A/actions/set_records", hcWrite, true, "example.com"},
		{"POST", "/v1/zones/example.com/rrsets/www/A/actions/explode", hcRead, false, "example.com"},
		{"POST", "/v1/nonsense", hcRead, false, ""},
		{"PUT", "/v1/pricing", hcRead, false, ""},
	}
	for _, c := range cases {
		got := hcClassify(c.method, c.path, nil)
		if got.Known != c.known || (c.known && got.Class != c.class) || got.ID != c.id {
			t.Errorf("%s %s: got class %s known %v id %q; want %s %v %q", c.method, c.path, got.Class, got.Known, got.ID, c.class, c.known, c.id)
		}
	}
	// A PUT on an image is a plain write — unless the body converts a backup
	// into a billed snapshot, which the update tool treats as lifecycle.
	if got := hcClassify("PUT", "/v1/images/5", map[string]any{"description": "x"}); got.Class != hcWrite || !got.Known {
		t.Errorf("PUT image description: %+v", got)
	}
	if got := hcClassify("PUT", "/v1/images/5", map[string]any{"type": "snapshot"}); got.Class != hcLifecycle || !got.Known || got.ID != "5" {
		t.Errorf("PUT image type: %+v", got)
	}
	// Unknown write shapes still name the resource addressed, so the guard runs.
	if got := hcClassify("POST", "/v1/servers/42/magic/7", nil); got.Known || got.ID != "42" || got.Res == nil {
		t.Errorf("unknown shape: %+v", got)
	}
}

func TestHetznerCloud_ErrorsAndRetry(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, func(c *config.Connection) { c.AllowLifecycle = true })
	ctx := context.Background()

	res, _ := h.handleCreate(ctx, toolRequest(map[string]any{"resource": "server", "body": map[string]any{"server_type": "cx23", "image": "x"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "needs name in the body") || f.count(http.MethodPost, "/v1/servers") != 0 {
		t.Errorf("missing required create field: %s", resultText(t, res))
	}
	res, _ = h.handleCreate(ctx, toolRequest(map[string]any{"resource": "server", "body": map[string]any{"name": "", "server_type": "cx23", "image": "x"}}))
	text := resultText(t, res)
	if !res.IsError || !strings.Contains(text, "Hetzner API 422 invalid_input") || !strings.Contains(text, "is required") || !strings.Contains(text, `"fields"`) {
		t.Errorf("422 rendering: %s", text)
	}
	res, _ = h.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v1/ratelimited"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "HTTP 429") {
		t.Errorf("429 pass-through: %s", resultText(t, res))
	}
	if f.count(http.MethodGet, "/v1/ratelimited") != 1 {
		t.Error("a 429 was retried")
	}
	hdr := http.Header{}
	hdr.Set("RateLimit-Reset", "1800000000")
	e := h.errorOf(&hcResponse{Status: 429, Header: hdr, Body: []byte(`{"error":{"code":"rate_limit_exceeded","message":"rate limit exceeded"}}`)})
	if !strings.Contains(e.Error(), "full again at 2027-01-15") {
		t.Errorf("429 error = %s", e.Error())
	}
	e = hcParseError(502, []byte("<html>bad gateway</html>"))
	if e.Code != "" || !strings.Contains(e.Error(), "Bad Gateway") || !strings.Contains(e.Error(), "<html>") {
		t.Errorf("non-JSON error = %s", e.Error())
	}

	f.mu.Lock()
	f.conflict = 1
	f.mu.Unlock()
	res, _ = h.handleUpdate(ctx, toolRequest(map[string]any{"resource": "server", "id": "2", "body": map[string]any{"name": "retried"}}))
	if res.IsError {
		t.Errorf("conflict not retried: %s", resultText(t, res))
	}
	if f.count(http.MethodPut, "/v1/servers/2") != 2 {
		t.Errorf("PUT count = %d, want 2", f.count(http.MethodPut, "/v1/servers/2"))
	}

	// A gateway failure is retried for a GET, never for a POST; a dropped
	// connection likewise — the POST may have gone through.
	f.mu.Lock()
	f.gateway502["GET /locations"] = 1
	f.gateway502["POST /volumes"] = 1
	f.mu.Unlock()
	res, _ = h.handleAPIGet(ctx, toolRequest(map[string]any{"path": "/v1/locations"}))
	if res.IsError || f.count(http.MethodGet, "/v1/locations") != 2 {
		t.Errorf("GET after 502: %s / %d attempts", resultText(t, res), f.count(http.MethodGet, "/v1/locations"))
	}
	res, _ = h.handleCreate(ctx, toolRequest(map[string]any{"resource": "volume", "body": map[string]any{"name": "v", "size": 10, "location": "fsn1"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "502") || f.count(http.MethodPost, "/v1/volumes") != 1 {
		t.Errorf("POST after 502: %s / %d attempts", resultText(t, res), f.count(http.MethodPost, "/v1/volumes"))
	}
	f.mu.Lock()
	f.dropOnce["POST /ssh_keys"] = true
	f.mu.Unlock()
	res, _ = h.handleCreate(ctx, toolRequest(map[string]any{"resource": "ssh_key", "body": map[string]any{"name": "k2", "public_key": "ssh-ed25519 AAAA"}}))
	if !res.IsError || !strings.Contains(resultText(t, res), "request failed") || f.count(http.MethodPost, "/v1/ssh_keys") != 1 {
		t.Errorf("POST on a dropped connection: %s / %d attempts", resultText(t, res), f.count(http.MethodPost, "/v1/ssh_keys"))
	}
	if !hcRetryNetError(http.MethodGet, fmt.Errorf("reset")) || hcRetryNetError(http.MethodPost, fmt.Errorf("reset")) || hcRetryNetError(http.MethodDelete, fmt.Errorf("reset")) {
		t.Error("transport retries are for GET and PUT only")
	}
	if _, ok := hcRetryAfter(http.MethodDelete, 504, nil); ok {
		t.Error("a DELETE is not retried after a gateway timeout — the second attempt would answer 404 for a delete that went through")
	}
	f.mu.Lock()
	f.gateway502["DELETE /ssh_keys/42"] = 1
	f.mu.Unlock()
	res, _ = h.handleDelete(ctx, toolRequest(map[string]any{"resource": "ssh_key", "id": "42"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "502") || f.count(http.MethodDelete, "/v1/ssh_keys/42") != 1 {
		t.Errorf("DELETE after 502: %s / %d attempts", resultText(t, res), f.count(http.MethodDelete, "/v1/ssh_keys/42"))
	}
	if _, ok := hcRetryAfter(http.MethodPost, 409, []byte(`{"error":{"code":"conflict","message":"x"}}`)); !ok {
		t.Error("a conflict is retried for every method")
	}

	wrong := newTestHetznerCloud(t, f, func(c *config.Connection) { c.Token = "tok_wrong" })
	res, _ = wrong.handleList(ctx, toolRequest(map[string]any{"resource": "server"}))
	if !res.IsError || !strings.Contains(resultText(t, res), "401 unauthorized") || !strings.Contains(resultText(t, res), "per project") {
		t.Errorf("401: %s", resultText(t, res))
	}

	f.mu.Lock()
	f.rateLow = true
	f.mu.Unlock()
	res, _ = h.handleList(ctx, toolRequest(map[string]any{"resource": "server"}))
	if !strings.Contains(resultText(t, res), "rate limit: 12 of 3600 requests left") {
		t.Errorf("rate note missing: %s", resultText(t, res))
	}
}

func TestHetznerCloud_PricingMetricsVerify(t *testing.T) {
	f := newHCFake(t)
	h := newTestHetznerCloud(t, f, nil)
	ctx := context.Background()

	res, _ := h.handlePricing(ctx, toolRequest(map[string]any{"server_types": []any{"CX23"}, "location": "nbg1"}))
	out := hcDecode(t, res)["pricing"].(map[string]any)
	types := out["server_types"].([]any)
	if len(types) != 1 || len(types[0].(map[string]any)["prices"].([]any)) != 1 {
		t.Errorf("pricing filter = %s", resultText(t, res))
	}
	if lbs, _ := out["load_balancer_types"].([]any); len(lbs) != 0 {
		t.Errorf("load balancer types not in nbg1 kept: %v", lbs)
	}
	if out["volume"] == nil || out["currency"] != "EUR" {
		t.Error("pricing lost its scalar parts")
	}

	res, _ = h.handleMetrics(ctx, toolRequest(map[string]any{"resource": "server", "id": "1", "type": []any{"cpu", "disk"}, "start": "2026-11-08T00:00:00Z", "end": "2026-11-08T01:00:00Z", "step": float64(60)}))
	if res.IsError || !strings.Contains(f.last().Query, "type=cpu&type=disk") || !strings.Contains(f.last().Query, "step=60") {
		t.Errorf("metrics: %s / %s", resultText(t, res), f.last().Query)
	}
	res, _ = h.handleMetrics(ctx, toolRequest(map[string]any{"resource": "server", "id": "1", "type": []any{"ram"}, "start": "a", "end": "b"}))
	if !res.IsError || !strings.Contains(resultText(t, res), `no metric "ram"`) {
		t.Errorf("bad metric: %s", resultText(t, res))
	}

	info, err := h.Verify(ctx)
	if err != nil || info.Servers != 2 || info.Zones != 1 || info.RateLimit != 3600 {
		t.Errorf("verify = %+v, %v", info, err)
	}
	res, _ = h.handleZonefile(ctx, toolRequest(map[string]any{"zone": "example.com"}))
	if !res.IsError { // the fake has no zonefile route: a 404 comes back as an error
		t.Errorf("zonefile: %s", resultText(t, res))
	}
}

func TestHetznerCloud_Instructions(t *testing.T) {
	base := config.Connection{Type: config.TypeHetznerCloud}
	if s := HetznerCloudInstructions(base); !strings.Contains(s, "without lifecycle") {
		t.Errorf("default: %s", s)
	}
	ro := base
	ro.ReadOnly = true
	if s := HetznerCloudInstructions(ro); !strings.Contains(s, "Read-only connection") {
		t.Errorf("read-only: %s", s)
	}
	lc := base
	lc.AllowLifecycle = true
	lc.ProtectedResources = []string{"server:my-cell-*"}
	s := HetznerCloudInstructions(lc)
	if !strings.Contains(s, "may run lifecycle operations") || !strings.Contains(s, "server:my-cell-*") {
		t.Errorf("lifecycle+protected: %s", s)
	}
	if got := DefaultInstructions(lc); got != s {
		t.Error("DefaultInstructions does not route to HetznerCloudInstructions")
	}
}

func TestHetznerCloud_Helpers(t *testing.T) {
	obj := map[string]any{"id": 1.0, "name": "s", "server_type": map[string]any{"name": "cx23"}, "public_net": map[string]any{"ipv4": nil}}
	sum := hcSummarize(obj, []string{"id", "name", "type=server_type.name", "ipv4=public_net.ipv4.ip", "missing"})
	if sum["type"] != "cx23" || sum["id"] != 1.0 {
		t.Errorf("summary = %v", sum)
	}
	if _, ok := sum["ipv4"]; ok {
		t.Error("nil path kept")
	}
	if _, ok := sum["missing"]; ok {
		t.Error("missing field kept")
	}
	recs, err := hcRecords([]any{"a", map[string]any{"value": "b", "comment": "c", "junk": 1}})
	if err != nil || len(recs) != 2 || recs[1]["comment"] != "c" || recs[1]["junk"] != nil {
		t.Errorf("records = %v %v", recs, err)
	}
	for _, bad := range []any{[]any{}, []any{""}, []any{map[string]any{"comment": "x"}}, []any{1.0}, "a"} {
		if _, err := hcRecords(bad); err == nil {
			t.Errorf("records %v accepted", bad)
		}
	}
	actions, extra, err := hcSplitActions([]byte(`{"action":{"id":1,"status":"running"},"next_actions":[{"id":2,"status":"success"}],"root_password":"x","server":{"id":3}}`))
	if err != nil || len(actions) != 2 || extra["root_password"] != "x" || extra["server"] == nil {
		t.Errorf("split = %v %v %v", actions, extra, err)
	}
	if _, err := hcAPIPath("v1/servers?name=a"); err != nil {
		t.Errorf("path without leading slash: %v", err)
	}
}

// The registry's endpoint table has one entry per tool-reachable call and
// every path parameter is one of the three the API uses.
func TestHetznerCloud_EndpointTable(t *testing.T) {
	eps := hcEndpoints()
	if len(eps) < 170 {
		t.Errorf("only %d endpoints", len(eps))
	}
	for key, ep := range eps {
		for _, seg := range strings.Split(ep.Path, "/") {
			if strings.HasPrefix(seg, "{") && seg != "{id}" && seg != "{id_or_name}" && seg != "{rr_name}" && seg != "{rr_type}" {
				t.Errorf("%s: unexpected path parameter %s", key, seg)
			}
		}
	}
}
