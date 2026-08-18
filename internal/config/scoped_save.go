package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/BurntSushi/toml"
)

// configFileMu serializes read-modify-write cycles on the config file within
// this process. Cross-process races are narrowed (not eliminated) by scoping
// each write to a single entry against a freshly read disk state — see
// mutateOnDisk.
var configFileMu sync.Mutex

// configPath resolves the file this Config was loaded from.
func (cfg *Config) configPath() string {
	if cfg.path != "" {
		return cfg.path
	}
	return DefaultConfigPath()
}

// mutateOnDisk re-reads the config file, applies mutate to that fresh disk
// view, and writes the result back atomically. Writers used to encode their
// whole in-memory Config, so concurrent full instances (desktop + headless,
// stdio_proxy = "never", two standalone starts) silently discarded each
// other's changes — last writer wins. Scoping the write to the caller's own
// entry keeps foreign changes intact. The in-memory Config is not refreshed
// here: the caller has already applied its change to it, and reconciling the
// rest of the runtime state (registered tools, tunnels) has its own reload
// paths.
func (cfg *Config) mutateOnDisk(mutate func(disk *Config)) error {
	configFileMu.Lock()
	defer configFileMu.Unlock()

	path := cfg.configPath()
	disk, err := readDiskConfig(path)
	if err != nil {
		return err
	}
	mutate(disk)
	return writeConfigFile(disk, path)
}

// readDiskConfig loads the current on-disk state of the config file without
// Load's runtime processing (no env overlay, no secret resolution, no
// connection defaults) — the result is written back to disk, and baking
// runtime values into the file would persist things that do not belong
// there. Two exceptions mirror Load deliberately: the server port default is
// seeded (Server.Port has no omitzero, so encoding a zero port would make
// the next Load bind port 0), and legacy-format sections are migrated so a
// scoped save on a not-yet-rewritten legacy file cannot drop them.
func readDiskConfig(path string) (*Config, error) {
	disk := &Config{
		Server: ServerConfig{Port: DefaultPort},
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return disk, nil // first save — start from an empty file view
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	migrated := migrateLegacyProvisioningHeader(content)
	if _, err := toml.Decode(string(migrated), disk); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	var legacy legacyConfigFile
	toml.Decode(string(migrated), &legacy)
	migrateFromLegacy(disk, &legacy)
	return disk, nil
}

// writeConfigFile encodes cfg and atomically replaces the config file
// (temp file + rename), so a crash or full disk mid-write can no longer
// leave a truncated config.toml behind. Mirrors writeSecretsFile.
func writeConfigFile(cfg *Config, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create config file %s: %w", tmp, err)
	}

	encErr := toml.NewEncoder(f).Encode(cfg)
	closeErr := f.Close()

	if encErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("write config: %w", encErr)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return fmt.Errorf("flush config file: %w", closeErr)
	}

	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config file: %w", err)
	}

	return nil
}

// SaveConnectionEntry persists one connection: it replaces (or appends) the
// entry with this name in the on-disk connection list and leaves everything
// else as the file currently has it.
func (cfg *Config) SaveConnectionEntry(conn Connection) error {
	return cfg.mutateOnDisk(func(disk *Config) {
		for i := range disk.Connections {
			if disk.Connections[i].Name == conn.Name {
				disk.Connections[i] = conn
				return
			}
		}
		disk.Connections = append(disk.Connections, conn)
	})
}

// DeleteConnectionEntry removes one connection from the on-disk list.
// Deleting an entry that is already gone is not an error.
func (cfg *Config) DeleteConnectionEntry(name string) error {
	return cfg.mutateOnDisk(func(disk *Config) {
		kept := disk.Connections[:0]
		for _, c := range disk.Connections {
			if c.Name != name {
				kept = append(kept, c)
			}
		}
		disk.Connections = kept
	})
}

// SaveTunnelEntry persists one tunnel — same scoping as SaveConnectionEntry.
func (cfg *Config) SaveTunnelEntry(t TunnelConfig) error {
	return cfg.mutateOnDisk(func(disk *Config) {
		for i := range disk.Tunnels {
			if disk.Tunnels[i].Name == t.Name {
				disk.Tunnels[i] = t
				return
			}
		}
		disk.Tunnels = append(disk.Tunnels, t)
	})
}

// DeleteTunnelEntry removes one tunnel from the on-disk list.
func (cfg *Config) DeleteTunnelEntry(name string) error {
	return cfg.mutateOnDisk(func(disk *Config) {
		kept := disk.Tunnels[:0]
		for _, t := range disk.Tunnels {
			if t.Name != name {
				kept = append(kept, t)
			}
		}
		disk.Tunnels = kept
	})
}

// SaveProvisioningEntry upserts one provisioning endpoint, keyed by Name
// (the empty name is the default endpoint).
func (cfg *Config) SaveProvisioningEntry(p ProvisioningConfig) error {
	return cfg.mutateOnDisk(func(disk *Config) {
		for i := range disk.Provisioning {
			if disk.Provisioning[i].Name == p.Name {
				disk.Provisioning[i] = p
				return
			}
		}
		disk.Provisioning = append(disk.Provisioning, p)
	})
}
