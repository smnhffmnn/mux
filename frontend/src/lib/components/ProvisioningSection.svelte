<script lang="ts">
  import type { ProvisioningInfo } from '../api'
  import { SetupProvisioning, SyncProvisioning } from '../api'

  let { provisioning, onSync }: { provisioning: ProvisioningInfo; onSync: () => void } = $props()

  let endpoint = $state(provisioning.endpoint ?? '')
  let token = $state('')
  let saving = $state(false)
  let syncing = $state(false)
  let resultMsg = $state(provisioning.resultMessage ?? '')
  let resultSuccess = $state(provisioning.resultSuccess ?? false)

  async function handleSetup() {
    saving = true
    resultMsg = ''
    try {
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
    <div class="form-grid">
      <div class="field">
        <label for="provisioning-endpoint">Endpoint</label>
        <input id="provisioning-endpoint" type="text" placeholder="https://provisioning.example.com/api/mux/config" bind:value={endpoint} />
      </div>
      <div class="field">
        <label for="provisioning-token">Token</label>
        <input id="provisioning-token" type="password" placeholder={provisioning.tokenSet ? '••••• (stored)' : 'Bearer token'} bind:value={token} />
      </div>
    </div>

    <div class="provisioning-actions">
      <button onclick={handleSetup} disabled={saving}>{saving ? 'Saving...' : 'Save'}</button>
      <button class="primary" onclick={handleSync} disabled={syncing || !provisioning.configured}>{syncing ? 'Syncing...' : 'Sync'}</button>
    </div>

    {#if provisioning.configured}
      <div class="provisioning-status">
        <span>{provisioning.tunnels} tunnels, {provisioning.connections} connections provisioned</span>
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

  .form-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 10px;
    margin-bottom: 12px;
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
