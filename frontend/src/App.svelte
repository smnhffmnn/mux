<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { pageData, serverInfo, activeView, loading } from './lib/stores'
  import { GetPageData, GetServerInfo } from './lib/api'
  import type { PageData } from './lib/api'
  import Header from './lib/components/Header.svelte'
  import ERPSection from './lib/components/ERPSection.svelte'
  import TunnelRow from './lib/components/TunnelRow.svelte'
  import ConnectionCard from './lib/components/ConnectionCard.svelte'
  import AddConnectionModal from './lib/components/AddConnectionModal.svelte'
  import AddTunnelModal from './lib/components/AddTunnelModal.svelte'

  let showAddModal = $state(false)
  let showAddTunnelModal = $state(false)
  let serverInfoInterval: ReturnType<typeof setInterval>

  onMount(async () => {
    $loading = true
    try {
      const data = await GetPageData()
      $pageData = data
      $serverInfo = data.server
    } catch (e) {
      console.error('Failed to load page data:', e)
    } finally {
      $loading = false
    }

    // Poll server info every 10s for uptime updates
    serverInfoInterval = setInterval(async () => {
      try {
        $serverInfo = await GetServerInfo()
      } catch (e) {
        console.error('Failed to refresh server info:', e)
      }
    }, 10000)
  })

  onDestroy(() => {
    if (serverInfoInterval) clearInterval(serverInfoInterval)
  })

  async function refreshData() {
    try {
      const data = await GetPageData()
      $pageData = data
      $serverInfo = data.server
    } catch (e) {
      console.error('Failed to refresh:', e)
    }
  }

  function onConnectionAdded() {
    showAddModal = false
    refreshData()
  }

  function onTunnelAdded() {
    showAddTunnelModal = false
    refreshData()
  }

  const navItems = [
    { id: 'connections' as const, label: 'Connections', icon: '⬡' },
    { id: 'tunnels' as const, label: 'Tunnels', icon: '◈' },
    { id: 'erp' as const, label: 'Provisioning', icon: '⟐' },
    { id: 'about' as const, label: 'About', icon: '◎' },
  ]
</script>

<div class="app">
  <div class="sidebar">
    <div class="sidebar-header drag">
      <span class="logo">mux</span>
    </div>
    <nav class="sidebar-nav">
      {#each navItems as item}
        <button
          class="nav-item"
          class:active={$activeView === item.id}
          onclick={() => ($activeView = item.id)}
        >
          <span class="nav-icon">{item.icon}</span>
          <span class="nav-label">{item.label}</span>
          {#if item.id === 'connections' && $pageData}
            <span class="nav-count">{$pageData.connections.length}</span>
          {/if}
          {#if item.id === 'tunnels' && $pageData}
            <span class="nav-count">{$pageData.tunnels.length}</span>
          {/if}
        </button>
      {/each}
    </nav>
  </div>

  <main class="content">
    <Header />

    {#if $loading}
      <div class="loading">Loading...</div>
    {:else if $pageData}
      {#if $activeView === 'connections'}
        <div class="section">
          <div class="section-header">
            <h2>Connections</h2>
            <button class="primary" onclick={() => (showAddModal = true)}>+ Add</button>
          </div>
          {#if $pageData.connections.length === 0}
            <div class="empty">No connections configured. Click "+ Add" to create one.</div>
          {:else}
            <div class="card-list">
              {#each $pageData.connections as conn (conn.name)}
                <ConnectionCard {conn} tunnelNames={$pageData.tunnels.map(t => t.name)} onUpdate={refreshData} />
              {/each}
            </div>
          {/if}
        </div>

      {:else if $activeView === 'tunnels'}
        <div class="section">
          <div class="section-header">
            <h2>Tunnels</h2>
            <button class="primary" onclick={() => (showAddTunnelModal = true)}>+ Add</button>
          </div>
          {#if $pageData.tunnels.length === 0}
            <div class="empty">No tunnels configured. Click "+ Add" to create one.</div>
          {:else}
            <div class="card-list">
              {#each $pageData.tunnels as tunnel (tunnel.name)}
                <TunnelRow {tunnel} onUpdate={refreshData} />
              {/each}
            </div>
          {/if}
        </div>

      {:else if $activeView === 'erp'}
        <ERPSection erp={$pageData.erp} onSync={refreshData} />

      {:else if $activeView === 'about'}
        <div class="section">
          <div class="section-header">
            <h2>About</h2>
          </div>
          <div class="about-card">
            <div class="about-row"><span class="about-label">Version</span><span class="about-value">{$serverInfo?.version ?? '—'}</span></div>
            <div class="about-row"><span class="about-label">Build Time</span><span class="about-value">{$serverInfo?.buildTime || '—'}</span></div>
            <div class="about-row"><span class="about-label">Uptime</span><span class="about-value">{$serverInfo?.uptime ?? '—'}</span></div>
            <div class="about-row"><span class="about-label">MCP Port</span><span class="about-value">{$serverInfo?.port ?? '—'}</span></div>
          </div>
        </div>
      {/if}
    {/if}
  </main>
</div>

{#if showAddModal && $pageData}
  <AddConnectionModal types={$pageData.types} onAdd={onConnectionAdded} onClose={() => (showAddModal = false)} />
{/if}

{#if showAddTunnelModal}
  <AddTunnelModal onAdd={onTunnelAdded} onClose={() => (showAddTunnelModal = false)} />
{/if}

<style>
  .app {
    display: flex;
    height: 100%;
  }

  .sidebar {
    width: 180px;
    flex-shrink: 0;
    background: var(--bg-surface);
    border-right: 1px solid var(--border);
    display: flex;
    flex-direction: column;
  }

  .sidebar-header {
    padding: 16px;
    border-bottom: 1px solid var(--border);
  }

  .logo {
    font-size: 16px;
    font-weight: 700;
    letter-spacing: 1px;
    color: var(--accent);
  }

  .sidebar-nav {
    padding: 8px;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .nav-item {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 10px;
    border: none;
    background: none;
    border-radius: 6px;
    color: var(--text-secondary);
    font-size: 13px;
    text-align: left;
    width: 100%;
  }

  .nav-item:hover {
    background: var(--bg-surface-hover);
    color: var(--text);
  }

  .nav-item.active {
    background: var(--bg-inset);
    color: var(--text);
    font-weight: 500;
  }

  .nav-icon {
    font-size: 14px;
    width: 18px;
    text-align: center;
  }

  .nav-label {
    flex: 1;
  }

  .nav-count {
    font-size: 11px;
    color: var(--text-muted);
    background: var(--bg-inset);
    padding: 1px 6px;
    border-radius: 10px;
    min-width: 18px;
    text-align: center;
  }

  .content {
    flex: 1;
    overflow-y: auto;
    padding: 20px 24px;
  }

  .section {
    margin-top: 16px;
  }

  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 12px;
  }

  .section-header h2 {
    font-size: 15px;
    font-weight: 600;
  }

  .card-list {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .empty {
    color: var(--text-muted);
    padding: 24px;
    text-align: center;
    border: 1px dashed var(--border);
    border-radius: var(--radius);
  }

  .loading {
    color: var(--text-muted);
    padding: 48px;
    text-align: center;
  }

  .about-card {
    background: var(--bg-surface);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }

  .about-row {
    display: flex;
    justify-content: space-between;
    align-items: center;
  }

  .about-label {
    color: var(--text-secondary);
    font-size: 12px;
  }

  .about-value {
    font-family: var(--font-mono);
    font-size: 12px;
  }
</style>
