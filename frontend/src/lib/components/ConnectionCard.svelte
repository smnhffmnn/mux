<script lang="ts">
  import type { ConnInfo, TestResult } from '../api'
  import { TestConnection, DeleteConnection, GetSetupDoc } from '../api'
  import { marked } from 'marked'
  import DOMPurify from 'dompurify'
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
  let showSetupDoc = $state(false)
  let setupDoc = $state('')

  $effect(() => {
    const current = conn.type
    if (!current) {
      setupDoc = ''
      return
    }
    GetSetupDoc(current)
      .then(doc => { if (current === conn.type) setupDoc = doc })
      .catch(() => { if (current === conn.type) setupDoc = '' })
  })

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
      {#if conn.source === 'provisioning'}
        <span class="badge provisioned">PROVISIONED</span>
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
        {#if !conn.isProvisioned}
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
      {#if setupDoc}
        <div class="setup-guide-toggle">
          <button class="link-btn" onclick={() => (showSetupDoc = !showSetupDoc)}>
            {showSetupDoc ? '▾ Setup Guide' : '▸ Setup Guide'}
          </button>
        </div>
        {#if showSetupDoc}
          <div class="setup-doc">
            {@html DOMPurify.sanitize(marked(setupDoc) as string)}
          </div>
        {/if}
      {/if}
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

  .badge.provisioned {
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

  .setup-guide-toggle {
    margin-top: 10px;
    padding-top: 10px;
    border-top: 1px solid var(--border);
  }

  .link-btn {
    background: none;
    border: none;
    color: var(--text-secondary);
    font-size: 11px;
    font-weight: 500;
    padding: 0;
    cursor: pointer;
  }
  .link-btn:hover {
    color: var(--accent);
    background: none;
  }

  .setup-doc {
    margin-top: 8px;
    padding: 12px;
    background: var(--bg-surface);
    border: 1px solid var(--border);
    border-radius: 6px;
    font-size: 12px;
    line-height: 1.6;
    color: var(--text-secondary);
    max-height: 400px;
    overflow-y: auto;
  }

  .setup-doc :global(h1) {
    font-size: 14px;
    font-weight: 600;
    color: var(--text);
    margin: 0 0 8px;
  }

  .setup-doc :global(h2) {
    font-size: 12px;
    font-weight: 600;
    color: var(--text);
    margin: 12px 0 4px;
  }

  .setup-doc :global(h3) {
    font-size: 11px;
    font-weight: 600;
    color: var(--text);
    margin: 10px 0 4px;
  }

  .setup-doc :global(p) {
    margin: 0 0 8px;
  }

  .setup-doc :global(ul), .setup-doc :global(ol) {
    margin: 0 0 8px;
    padding-left: 18px;
  }

  .setup-doc :global(li) {
    margin: 2px 0;
  }

  .setup-doc :global(code) {
    font-family: var(--font-mono);
    font-size: 11px;
    background: var(--bg-inset);
    padding: 1px 4px;
    border-radius: 3px;
    border: 1px solid var(--border);
  }

  .setup-doc :global(pre) {
    background: var(--bg-inset);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 8px;
    overflow-x: auto;
    margin: 0 0 8px;
  }

  .setup-doc :global(pre code) {
    background: none;
    border: none;
    padding: 0;
  }

  .setup-doc :global(table) {
    width: 100%;
    border-collapse: collapse;
    margin: 0 0 8px;
    font-size: 11px;
  }

  .setup-doc :global(th), .setup-doc :global(td) {
    border: 1px solid var(--border);
    padding: 4px 8px;
    text-align: left;
  }

  .setup-doc :global(th) {
    background: var(--bg-inset);
    font-weight: 600;
    color: var(--text);
  }

  .setup-doc :global(a) {
    color: var(--accent);
    text-decoration: none;
  }

  .setup-doc :global(a:hover) {
    text-decoration: underline;
  }
</style>
