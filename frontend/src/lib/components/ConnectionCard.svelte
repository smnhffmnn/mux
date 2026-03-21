<script lang="ts">
  import type { ConnInfo, TestResult } from '../api'
  import { TestConnection, DeleteConnection } from '../api'
  import ConnectionForm from './ConnectionForm.svelte'
  import TestResultComponent from './TestResult.svelte'
  import OAuthFlow from './OAuthFlow.svelte'
  import DeviceAuthFlow from './DeviceAuthFlow.svelte'

  let { conn, tunnelNames = [], onUpdate }: { conn: ConnInfo; tunnelNames?: string[]; onUpdate: () => void } = $props()

  let expanded = $state(false)
  let testing = $state(false)
  let testResult = $state<TestResult | null>(null)
  let showOAuth = $state(false)
  let showDeviceAuth = $state(false)

  async function handleTest() {
    testing = true
    testResult = null
    try {
      testResult = await TestConnection(conn.name)
    } catch (e: any) {
      testResult = { connection: conn.name, connected: false, message: e?.message ?? 'Test failed' }
    } finally {
      testing = false
    }
  }

  async function handleDelete() {
    try {
      await DeleteConnection(conn.name)
      onUpdate()
    } catch (e: any) {
      alert(e?.message ?? 'Delete failed')
    }
  }

  function handleSaved() {
    expanded = false
    onUpdate()
  }
</script>

<div class="card">
  <div class="card-header">
    <div class="card-left">
      <span class="dot" class:green={conn.configured} class:gray={!conn.configured}></span>
      <button class="card-name" onclick={() => (expanded = !expanded)}>
        {conn.name}
      </button>
      <span class="badge">{conn.typeLabel}</span>
      {#if conn.source === 'erp'}
        <span class="badge erp">ERP</span>
      {/if}
      {#if conn.tunnel}
        <span class="badge tunnel">🔒 {conn.tunnel}</span>
      {/if}
    </div>
    <div class="card-right">
      {#if conn.summary}
        <span class="summary">{conn.summary}</span>
      {/if}
      <div class="card-actions">
        <button onclick={handleTest} disabled={testing}>
          {testing ? '...' : 'Test'}
        </button>
        {#if conn.isOAuth && !conn.oauthOK}
          <button class="primary" onclick={() => (showOAuth = true)}>Authorize</button>
        {/if}
        {#if conn.isDeviceAuth && !conn.deviceAuthOK}
          <button class="primary" onclick={() => (showDeviceAuth = true)}>Authenticate</button>
        {/if}
        {#if !conn.isERP}
          <button class="danger" onclick={handleDelete}>×</button>
        {/if}
      </div>
    </div>
  </div>

  {#if testResult}
    <TestResultComponent result={testResult} />
  {/if}

  {#if showOAuth}
    <OAuthFlow name={conn.name} onComplete={() => { showOAuth = false; onUpdate() }} />
  {/if}

  {#if showDeviceAuth}
    <DeviceAuthFlow name={conn.name} onComplete={() => { showDeviceAuth = false; onUpdate() }} />
  {/if}

  {#if expanded}
    <div class="card-detail">
      <ConnectionForm {conn} {tunnelNames} onSave={handleSaved} onCancel={() => (expanded = false)} />
    </div>
  {/if}
</div>

<style>
  .card {
    background: var(--bg-surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }

  .card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 10px 14px;
    gap: 12px;
  }

  .card-left {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
  }

  .card-name {
    font-weight: 500;
    background: none;
    border: none;
    padding: 0;
    color: var(--text);
    cursor: pointer;
    font-size: 13px;
  }
  .card-name:hover {
    color: var(--accent);
    background: none;
  }

  .card-right {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-shrink: 0;
  }

  .summary {
    font-size: 11px;
    font-family: var(--font-mono);
    color: var(--text-muted);
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .card-actions {
    display: flex;
    gap: 4px;
  }

  .badge.erp {
    background: var(--yellow-bg);
    color: var(--yellow);
    border-color: var(--yellow);
  }

  .badge.tunnel {
    font-size: 9px;
  }

  .card-detail {
    border-top: 1px solid var(--border);
    padding: 14px;
    background: var(--bg-inset);
  }
</style>
