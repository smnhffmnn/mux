<script lang="ts">
  import type { TunnelInfo } from '../api'
  import { DeleteTunnel } from '../api'
  import TunnelForm from './TunnelForm.svelte'

  let { tunnel, onUpdate }: { tunnel: TunnelInfo; onUpdate: () => void } = $props()

  let expanded = $state(false)

  const tunnelSummary = $derived.by(() => {
    if (tunnel.type === 'ssh') {
      const port = tunnel.port && tunnel.port !== 22 ? `:${tunnel.port}` : ''
      return tunnel.user && tunnel.host ? `${tunnel.user}@${tunnel.host}${port}` : tunnel.host || ''
    }
    const parts: string[] = []
    if (tunnel.tunnelAddress) parts.push(tunnel.tunnelAddress)
    if (tunnel.peerEndpoint) parts.push(tunnel.peerEndpoint)
    return parts.join(' \u2192 ')
  })

  async function handleDelete() {
    try {
      await DeleteTunnel(tunnel.name)
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
      <span class="dot" class:green={tunnel.connected} class:gray={!tunnel.connected}></span>
      <button class="card-name" onclick={() => (expanded = !expanded)}>
        {tunnel.name}
      </button>
      <span class="badge">{tunnel.type === 'ssh' ? 'SSH' : 'WG'}</span>
      {#if tunnel.source === 'erp'}
        <span class="badge erp">PROVISIONED</span>
      {/if}
    </div>
    <div class="card-right">
      {#if tunnelSummary}
        <span class="summary">{tunnelSummary}</span>
      {/if}
      <div class="card-actions">
        {#if tunnel.source !== 'erp'}
          <button class="danger" onclick={handleDelete}>&times;</button>
        {/if}
      </div>
    </div>
  </div>

  {#if expanded}
    <div class="card-detail">
      <TunnelForm {tunnel} onSave={handleSaved} onCancel={() => (expanded = false)} />
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
    max-width: 260px;
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

  .card-detail {
    border-top: 1px solid var(--border);
    padding: 14px;
    background: var(--bg-inset);
  }
</style>
