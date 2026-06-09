<script lang="ts">
  import type { ProvisioningInfo, ProvisioningEndpointInfo } from '../api'
  import { SetupProvisioning, SyncProvisioning } from '../api'

  let { provisioning, onSync }: { provisioning: ProvisioningInfo; onSync: () => void } = $props()

  // Defensive: the backend should always send an array, but a nil Go slice
  // marshals to JSON `null`. Guard so a single render-throw can't freeze the
  // whole window (the tab swap aborts and the previous view stays on screen).
  const endpoints = $derived(provisioning?.endpoints ?? [])

  // Selected endpoint to edit. Empty string = the default (unnamed) endpoint,
  // which matches how the backend treats the legacy single-endpoint case.
  let selectedName = $state<string>('')
  let endpoint = $state('')
  let token = $state('')
  let saving = $state(false)
  let syncing = $state(false)
  let resultMsg = $state('')
  let resultSuccess = $state(false)

  $effect(() => {
    // Reset form values when switching endpoint selection
    const sel = endpoints.find(e => e.name === selectedName)
    endpoint = sel?.endpoint ?? ''
    token = ''
  })

  function displayName(e: ProvisioningEndpointInfo): string {
    return e.name === '' ? 'Default' : e.name
  }

  async function handleSetup() {
    saving = true
    resultMsg = ''
    try {
      // Backend's SetupProvisioning currently updates the default (unnamed) endpoint only.
      // Named endpoints are managed via config.toml or the MCP provisioning_set tool.
      const result = await SetupProvisioning(endpoint, token)
      resultMsg = result.resultMessage ?? 'Saved'
      resultSuccess = result.resultSuccess ?? true
      token = ''
    } catch (e: any) {
      resultMsg = e?.message ?? 'Save failed'
      resultSuccess = false
    } finally {
      saving = false
    }
  }

  async function handleSync() {
    syncing = true
    resultMsg = ''
    try {
      await SyncProvisioning()
      onSync()
      resultMsg = 'Sync complete'
      resultSuccess = true
    } catch (e: any) {
      resultMsg = e?.message ?? 'Sync failed'
      resultSuccess = false
    } finally {
      syncing = false
    }
  }
</script>

<div class="section">
  <div class="section-header">
    <h2>Provisioning</h2>
  </div>

  <div class="provisioning-card">
    {#if endpoints.length > 0}
      <div class="endpoint-list">
        {#each endpoints as ep, i (ep.name || `__default_${i}`)}
          <div class="endpoint-row" class:active={ep.name === selectedName}>
            <button class="endpoint-label" onclick={() => (selectedName = ep.name)}>
              <span class="dot" class:green={ep.tokenSet && ep.endpoint} class:gray={!ep.tokenSet || !ep.endpoint}></span>
              <span class="name">{displayName(ep)}</span>
              <span class="url">{ep.endpoint || '(no endpoint)'}</span>
            </button>
            <span class="stats">{ep.tunnels}T · {ep.connections}C</span>
          </div>
        {/each}
      </div>
    {/if}

    <div class="form-grid">
      <div class="field">
        <label for="provisioning-endpoint">Endpoint (Default)</label>
        <input id="provisioning-endpoint" type="text" placeholder="https://provisioning.example.com/api/mux/provision" bind:value={endpoint} />
      </div>
      <div class="field">
        <label for="provisioning-token">Token (Default)</label>
        <input id="provisioning-token" type="password" placeholder={endpoints.find(e => e.name === '')?.tokenSet ? '••••• (stored)' : 'Bearer token'} bind:value={token} />
      </div>
    </div>
    <p class="hint">
      Additional endpoints can be configured via <code>[[provisioning]]</code> blocks in <code>~/.mux/config.toml</code>
      or through the <code>provisioning_set</code> MCP tool with a <code>name</code> parameter.
    </p>

    <div class="provisioning-actions">
      <button onclick={handleSetup} disabled={saving}>{saving ? 'Saving...' : 'Save Default'}</button>
      <button class="primary" onclick={handleSync} disabled={syncing || !provisioning.configured}>{syncing ? 'Syncing...' : 'Sync All'}</button>
    </div>

    {#if provisioning.configured}
      <div class="provisioning-status">
        <span>Total: {provisioning.tunnels} tunnels, {provisioning.connections} connections across {endpoints.length} endpoint{endpoints.length === 1 ? '' : 's'}</span>
      </div>
    {/if}

    {#if resultMsg}
      <div class="provisioning-result" class:success={resultSuccess} class:error={!resultSuccess}>
        {resultMsg}
      </div>
    {/if}
  </div>
</div>

<style>
  .section {
    margin-top: 16px;
  }

  .section-header {
    margin-bottom: 12px;
  }

  .section-header h2 {
    font-size: 15px;
    font-weight: 600;
  }

  .provisioning-card {
    background: var(--bg-surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 16px;
  }

  .endpoint-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-bottom: 12px;
    padding-bottom: 12px;
    border-bottom: 1px solid var(--border);
  }

  .endpoint-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 4px 6px;
    border-radius: 4px;
  }

  .endpoint-row.active {
    background: var(--bg-inset);
  }

  .endpoint-label {
    display: flex;
    align-items: center;
    gap: 8px;
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    flex: 1;
    min-width: 0;
    text-align: left;
  }

  .endpoint-label .name {
    font-weight: 500;
    font-size: 12px;
  }

  .endpoint-label .url {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-muted);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stats {
    font-size: 11px;
    color: var(--text-muted);
    font-family: var(--font-mono);
    flex-shrink: 0;
  }

  .form-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 10px;
    margin-bottom: 8px;
  }

  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .field label {
    font-size: 11px;
    font-weight: 500;
    color: var(--text-secondary);
    text-transform: uppercase;
    letter-spacing: 0.3px;
  }

  .field input {
    width: 100%;
  }

  .hint {
    font-size: 11px;
    color: var(--text-muted);
    margin: 0 0 10px;
  }

  .hint code {
    font-family: var(--font-mono);
    font-size: 10px;
    background: var(--bg-inset);
    padding: 1px 4px;
    border-radius: 3px;
    border: 1px solid var(--border);
  }

  .provisioning-actions {
    display: flex;
    gap: 6px;
    margin-bottom: 10px;
  }

  .provisioning-status {
    font-size: 12px;
    color: var(--text-secondary);
  }

  .provisioning-result {
    margin-top: 8px;
    font-size: 12px;
    padding: 6px 10px;
    border-radius: 6px;
  }
  .provisioning-result.success {
    background: var(--green-bg);
    color: var(--green);
  }
  .provisioning-result.error {
    background: var(--red-bg);
    color: var(--red);
  }
</style>
