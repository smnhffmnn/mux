<script lang="ts">
  import type { TunnelInfo } from '../api'

  let { tunnel }: { tunnel: TunnelInfo } = $props()
</script>

<div class="tunnel-row">
  <div class="tunnel-left">
    <span class="dot" class:green={tunnel.connected} class:gray={!tunnel.connected}></span>
    <span class="tunnel-name">{tunnel.name}</span>
    <span class="badge">{tunnel.source === 'erp' ? 'ERP' : 'LOCAL'}</span>
  </div>
  <div class="tunnel-right">
    <span class="tunnel-detail">{tunnel.tunnelAddress}</span>
    <span class="tunnel-sep">→</span>
    <span class="tunnel-detail">{tunnel.peerEndpoint}</span>
  </div>
</div>

<style>
  .tunnel-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 10px 14px;
    background: var(--bg-surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  .tunnel-left {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .tunnel-name {
    font-weight: 500;
  }

  .tunnel-right {
    display: flex;
    align-items: center;
    gap: 6px;
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--text-secondary);
  }

  .tunnel-sep {
    color: var(--text-muted);
  }

  .tunnel-detail {
    color: var(--text-secondary);
  }
</style>
