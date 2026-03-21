package wireguard

import (
	"log"
	"sync"

	"github.com/smnhffmnn/mux/internal/config"
)

// Manager manages a set of named WireGuard tunnels.
type Manager struct {
	mu      sync.RWMutex
	tunnels map[string]*WGTunnel
}

// NewManager creates a new tunnel manager.
func NewManager() *Manager {
	return &Manager{
		tunnels: make(map[string]*WGTunnel),
	}
}

// Start attempts to establish all configured tunnels.
// Returns a map of tunnel names to errors for any that failed.
// Tunnels that succeed are stored and available via Get().
func (m *Manager) Start(tunnels []config.TunnelConfig) map[string]error {
	errs := make(map[string]error)

	for _, cfg := range tunnels {
		if !cfg.Enabled() {
			log.Printf("[wg] Skipping tunnel %q: incomplete config", cfg.Name)
			continue
		}

		t, err := New(cfg)
		if err != nil {
			errs[cfg.Name] = err
			log.Printf("[wg] Tunnel %q failed: %v", cfg.Name, err)
			continue
		}

		m.mu.Lock()
		m.tunnels[cfg.Name] = t
		m.mu.Unlock()

		log.Printf("[wg] Tunnel %q started (%s → %s)", cfg.Name, cfg.TunnelAddress, cfg.PeerEndpoint)
	}

	return errs
}

// Get returns the tunnel with the given name, or nil if it's not available.
func (m *Manager) Get(name string) *WGTunnel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tunnels[name]
}

// IsUp reports whether the named tunnel is running.
func (m *Manager) IsUp(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tunnels[name] != nil
}

// Names returns the names of all running tunnels.
func (m *Manager) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.tunnels))
	for n := range m.tunnels {
		names = append(names, n)
	}
	return names
}

// Close shuts down all running tunnels and releases resources.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, t := range m.tunnels {
		log.Printf("[wg] Closing tunnel %q", name)
		t.Close()
	}
	m.tunnels = make(map[string]*WGTunnel)
}
