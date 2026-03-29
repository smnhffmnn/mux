package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/smnhffmnn/mux/internal/config"
	"github.com/smnhffmnn/mux/internal/vault"
)

// VaultTools provides MCP tools for vault management.
type VaultTools struct {
	vault *vault.Vault
	cfg   *config.Config
}

// NewVaultTools creates vault MCP tools.
func NewVaultTools(v *vault.Vault, cfg *config.Config) *VaultTools {
	return &VaultTools{vault: v, cfg: cfg}
}

// Tools returns all vault MCP tool definitions.
func (vt *VaultTools) Tools() []ToolDef {
	return []ToolDef{
		vt.statusTool(),
		vt.initTool(),
		vt.unlockTool(),
		vt.lockTool(),
		vt.migrateTool(),
	}
}

func (vt *VaultTools) statusTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("vault_status",
			mcp.WithDescription("Show vault state: uninitialized, sealed, or unlocked. Includes secret count, registered WebAuthn credentials, and remaining inactivity time."),
		),
		Handler: vt.handleStatus,
	}
}

func (vt *VaultTools) initTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("vault_init",
			mcp.WithDescription("Initialize the encrypted vault with a passphrase. This creates the vault file and auth key. The vault is unlocked after init. Existing secrets from keyring/file are NOT automatically migrated — use vault_migrate for that."),
			mcp.WithString("passphrase", mcp.Required(),
				mcp.Description("Master passphrase for the vault. Used for Argon2id key derivation. Choose a strong passphrase."),
			),
		),
		Handler: vt.handleInit,
	}
}

func (vt *VaultTools) unlockTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("vault_unlock",
			mcp.WithDescription("Unlock the vault with the master passphrase. After unlocking, secrets are accessible in memory until the inactivity timeout (30 min) triggers auto-lock. WebAuthn unlock uses the HTTP API, not this tool."),
			mcp.WithString("passphrase", mcp.Required(),
				mcp.Description("Master passphrase."),
			),
		),
		Handler: vt.handleUnlock,
	}
}

func (vt *VaultTools) lockTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("vault_lock",
			mcp.WithDescription("Lock the vault immediately. Wipes the decryption key from memory. All vault-backed secrets become inaccessible until the next unlock."),
		),
		Handler: vt.handleLock,
	}
}

func (vt *VaultTools) migrateTool() ToolDef {
	return ToolDef{
		Tool: mcp.NewTool("vault_migrate",
			mcp.WithDescription("Migrate all existing secrets from the OS keychain / file store into the encrypted vault. The vault must be unlocked. After migration, secrets are encrypted at rest. Original keychain/file entries are left in place (delete manually if desired)."),
		),
		Handler: vt.handleMigrate,
	}
}

// --- Handlers ---

func (vt *VaultTools) handleStatus(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	status := vt.vault.Status()
	data, _ := json.MarshalIndent(status, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}

func (vt *VaultTools) handleInit(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	passphrase, err := req.RequireString("passphrase")
	if err != nil {
		return mcp.NewToolResultError("passphrase required"), nil
	}

	if len(passphrase) < 8 {
		return mcp.NewToolResultError("passphrase must be at least 8 characters"), nil
	}

	if err := vt.vault.Init(passphrase); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	status := vt.vault.Status()
	data, _ := json.MarshalIndent(status, "", "  ")
	return mcp.NewToolResultText(fmt.Sprintf("Vault initialized and unlocked.\n%s", data)), nil
}

func (vt *VaultTools) handleUnlock(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	passphrase, err := req.RequireString("passphrase")
	if err != nil {
		return mcp.NewToolResultError("passphrase required"), nil
	}

	if err := vt.vault.UnlockWithPassphrase(passphrase); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	status := vt.vault.Status()
	data, _ := json.MarshalIndent(status, "", "  ")
	return mcp.NewToolResultText(fmt.Sprintf("Vault unlocked.\n%s", data)), nil
}

func (vt *VaultTools) handleLock(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	vt.vault.Lock()
	return mcp.NewToolResultText("Vault locked. All secrets are now inaccessible."), nil
}

func (vt *VaultTools) handleMigrate(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if vt.vault.State() != vault.StateUnlocked {
		return mcp.NewToolResultError("vault must be unlocked to migrate secrets"), nil
	}

	// Collect all secret keys that might exist
	var keysToTry []string

	// Provisioning token
	keysToTry = append(keysToTry, "provisioning-token")

	// Connection secrets
	for _, c := range vt.cfg.AllConnections() {
		keysToTry = append(keysToTry,
			c.Name+"-password",
			c.Name+"-token",
			c.Name+"-oauth-token",
			c.Name+"-oauth-refresh-token",
			c.Name+"-oauth-client-id",
			c.Name+"-oauth-client-secret",
		)
	}

	// Tunnel secrets
	for _, t := range vt.cfg.AllTunnels() {
		keysToTry = append(keysToTry,
			"tunnel-"+t.Name+"-private-key",
			"tunnel-"+t.Name+"-preshared-key",
		)
	}

	migrated := 0
	skipped := 0
	var errors []string

	for _, key := range keysToTry {
		// Skip if already in vault
		if vt.vault.HasSecret(key) {
			skipped++
			continue
		}

		// Try to read from keychain/file
		value, err := config.GetSecret(key)
		if err != nil {
			continue // secret doesn't exist in keychain/file
		}

		// Migrate into vault
		if err := vt.vault.SetSecret(key, value); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		migrated++
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Migration complete: %d secrets migrated, %d already in vault", migrated, skipped)
	if len(errors) > 0 {
		fmt.Fprintf(&sb, ", %d errors:\n%s", len(errors), strings.Join(errors, "\n"))
	}

	return mcp.NewToolResultText(sb.String()), nil
}
