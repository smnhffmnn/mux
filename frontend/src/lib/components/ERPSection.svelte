<script lang="ts">
  import type { ERPInfo } from '../api'
  import { SetupERP, SyncERP } from '../api'

  let { erp, onSync }: { erp: ERPInfo; onSync: () => void } = $props()

  let endpoint = $state(erp.endpoint ?? '')
  let token = $state('')
  let saving = $state(false)
  let syncing = $state(false)
  let resultMsg = $state(erp.resultMessage ?? '')
  let resultSuccess = $state(erp.resultSuccess ?? false)

  async function handleSetup() {
    saving = true
    resultMsg = ''
    try {
      const result = await SetupERP(endpoint, token)
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
      await SyncERP()
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
    <h2>ERP Provisioning</h2>
  </div>

  <div class="erp-card">
    <div class="form-grid">
      <div class="field">
        <label for="erp-endpoint">Endpoint</label>
        <input id="erp-endpoint" type="text" placeholder="https://erp.example.com/api/mux" bind:value={endpoint} />
      </div>
      <div class="field">
        <label for="erp-token">Token</label>
        <input id="erp-token" type="password" placeholder={erp.tokenSet ? '••••• (stored)' : 'Bearer token'} bind:value={token} />
      </div>
    </div>

    <div class="erp-actions">
      <button onclick={handleSetup} disabled={saving}>{saving ? 'Saving...' : 'Save'}</button>
      <button class="primary" onclick={handleSync} disabled={syncing || !erp.configured}>{syncing ? 'Syncing...' : 'Sync from ERP'}</button>
    </div>

    {#if erp.configured}
      <div class="erp-status">
        <span>{erp.tunnels} tunnels, {erp.connections} connections from ERP</span>
      </div>
    {/if}

    {#if resultMsg}
      <div class="erp-result" class:success={resultSuccess} class:error={!resultSuccess}>
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

  .erp-card {
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

  .erp-actions {
    display: flex;
    gap: 6px;
    margin-bottom: 10px;
  }

  .erp-status {
    font-size: 12px;
    color: var(--text-secondary);
  }

  .erp-result {
    margin-top: 8px;
    font-size: 12px;
    padding: 6px 10px;
    border-radius: 6px;
  }
  .erp-result.success {
    background: var(--green-bg);
    color: var(--green);
  }
  .erp-result.error {
    background: var(--red-bg);
    color: var(--red);
  }
</style>
