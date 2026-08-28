// Package health answers one question: is this instance actually serving what
// it was configured to serve?
//
// The distinction matters because mux fails silently in a way a port check
// cannot see. When mux starts before DNS is up — routine in a container that
// boots alongside its network — the provisioning fetch fails, mux logs it and
// carries on. The HTTP server listens, /mcp answers, and the provisioned
// connections are simply absent. Nothing is broken enough to crash; the tools
// are just missing, and that surfaces much later as "the agent says it has no
// youtrack-index tool".
//
// So the check compares intent against reality: every enabled provisioning
// endpoint must have delivered a config, every configured tunnel must be up,
// and every enabled connection must have its tools registered.
package health

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/smnhffmnn/mux/internal/config"
)

// Status is the overall verdict. Only two values: either everything the config
// asks for is in place, or something is missing and the instance is degraded.
// There is deliberately no "unhealthy" tier — a mux that cannot serve HTTP at
// all does not answer this endpoint in the first place.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
)

// Registry reports which connections currently have MCP tools registered.
// Both the headless reloader and the desktop App track this, and both satisfy
// this interface.
type Registry interface {
	RegisteredConnections() []string
}

// Tunnels reports whether a named tunnel is up.
type Tunnels interface {
	IsUp(name string) bool
}

// EndpointReport is the state of one provisioning endpoint.
type EndpointReport struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Delivered is false when this endpoint has not answered since startup.
	// That is the silent-failure case this package exists for.
	Delivered   bool `json:"delivered"`
	Tunnels     int  `json:"tunnels"`
	Connections int  `json:"connections"`
}

// TunnelReport is the state of one configured tunnel.
type TunnelReport struct {
	Name string `json:"name"`
	Up   bool   `json:"up"`
}

// ConnectionReport is the state of one configured connection.
//
// The shape carries more than the boot check needs on purpose: a collective
// connection test (MUX-23) reports per connection too, and should extend this
// struct rather than introduce a second, differently-shaped answer to
// "how is connection X doing".
type ConnectionReport struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Source     string `json:"source"`
	Tunnel     string `json:"tunnel,omitempty"`
	Registered bool   `json:"registered"`
	// Detail explains a false Registered where the reason is known.
	Detail string `json:"detail,omitempty"`
}

// Report is the full answer, returned as the JSON body of GET /health.
type Report struct {
	Status       Status             `json:"status"`
	Version      string             `json:"version"`
	UptimeSecs   int64              `json:"uptimeSeconds"`
	Provisioning []EndpointReport   `json:"provisioning"`
	Tunnels      []TunnelReport     `json:"tunnels"`
	Connections  []ConnectionReport `json:"connections"`
	// Problems lists, in plain language, every reason Status is degraded.
	// Empty when Status is ok. This is what a human reads first.
	Problems []string `json:"problems"`
}

// Checker builds Reports from live state.
type Checker struct {
	cfg       *config.Config
	registry  Registry
	tunnels   Tunnels
	version   string
	startTime time.Time
	// now is swappable for tests.
	now func() time.Time
}

// NewChecker wires a Checker to the live config, tool registry and tunnel manager.
func NewChecker(cfg *config.Config, registry Registry, tunnels Tunnels, version string, startTime time.Time) *Checker {
	return &Checker{
		cfg:       cfg,
		registry:  registry,
		tunnels:   tunnels,
		version:   version,
		startTime: startTime,
		now:       time.Now,
	}
}

// Check inspects current state and returns the report.
func (c *Checker) Check() Report {
	rep := Report{
		Status:       StatusOK,
		Version:      c.version,
		UptimeSecs:   int64(c.now().Sub(c.startTime).Seconds()),
		Provisioning: []EndpointReport{},
		Tunnels:      []TunnelReport{},
		Connections:  []ConnectionReport{},
		Problems:     []string{},
	}

	for _, p := range c.cfg.Provisioning {
		if !p.Enabled() {
			continue
		}
		name := p.Name
		if name == "" {
			name = "default"
		}
		delivered := c.cfg.ProvisionedFrom(p.Name)
		tunnels, conns := c.cfg.ProvisionedCountFor(p.Name)
		rep.Provisioning = append(rep.Provisioning, EndpointReport{
			Name:        name,
			URL:         p.Endpoint,
			Delivered:   delivered,
			Tunnels:     tunnels,
			Connections: conns,
		})
		if !delivered {
			rep.Problems = append(rep.Problems,
				fmt.Sprintf("provisioning endpoint %q has not delivered a config", name))
		}
	}

	for _, t := range c.cfg.AllTunnels() {
		up := c.tunnels != nil && c.tunnels.IsUp(t.Name)
		rep.Tunnels = append(rep.Tunnels, TunnelReport{Name: t.Name, Up: up})
		if !up {
			rep.Problems = append(rep.Problems, fmt.Sprintf("tunnel %q is not up", t.Name))
		}
	}

	registered := make(map[string]bool)
	if c.registry != nil {
		for _, name := range c.registry.RegisteredConnections() {
			registered[name] = true
		}
	}

	for _, conn := range c.cfg.AllConnections() {
		if !conn.Enabled() {
			continue
		}
		cr := ConnectionReport{
			Name:       conn.Name,
			Type:       conn.Type,
			Source:     conn.Source,
			Tunnel:     conn.Tunnel,
			Registered: registered[conn.Name],
		}
		if !cr.Registered {
			// An OAuth proxy that was never logged in registers nothing. That is
			// a real gap, but not one a restart or a wait fixes — it needs a
			// human at a browser. Reporting it as degraded would leave the
			// endpoint permanently red on any instance with an unused OAuth
			// connection, and a health check that is always red gets ignored.
			if config.IsProxyType(conn.Type) && conn.OAuth {
				cr.Detail = "awaiting oauth login"
			} else {
				cr.Detail = "no tools registered"
				rep.Problems = append(rep.Problems,
					fmt.Sprintf("connection %q has no registered tools", conn.Name))
			}
		}
		rep.Connections = append(rep.Connections, cr)
	}

	if len(rep.Problems) > 0 {
		rep.Status = StatusDegraded
	}
	return rep
}

// Handler serves the report at GET /health.
//
// Degraded answers with 503 so container orchestrators, which read the status
// code and not the body, hold dependents back until the instance is actually
// serving its connections.
func (c *Checker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rep := c.Check()

		code := http.StatusOK
		if rep.Status != StatusOK {
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			// Header is already written; nothing left but to stop.
			return
		}
	}
}

// Mount registers the health route on mux.
func (c *Checker) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", c.Handler())
}
