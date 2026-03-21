<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { StartOAuth, GetOAuthStatus } from '../api'
  import { Events, Browser } from '@wailsio/runtime'

  let { name, onComplete }: { name: string; onComplete: () => void } = $props()

  let status = $state('Starting OAuth...')
  let error = $state('')
  let polling = $state(false)
  let pollInterval: ReturnType<typeof setInterval>

  onMount(async () => {
    try {
      const result = await StartOAuth(name)
      Browser.OpenURL(result.authURL)
      status = 'Waiting for authorization in browser...'
      startPolling()
    } catch (e: any) {
      error = e?.message ?? 'OAuth start failed'
    }

    // Listen for Wails event from OAuth callback
    Events.On('oauth:complete', (event: any) => {
      if (event.data === name) {
        stopPolling()
        onComplete()
      }
    })
  })

  onDestroy(() => {
    stopPolling()
    Events.Off('oauth:complete')
  })

  function startPolling() {
    polling = true
    pollInterval = setInterval(async () => {
      try {
        const s = await GetOAuthStatus(name)
        if (s.authorized) {
          stopPolling()
          onComplete()
        }
      } catch {
        // ignore poll errors
      }
    }, 2000)
  }

  function stopPolling() {
    polling = false
    if (pollInterval) clearInterval(pollInterval)
  }
</script>

<div class="oauth-flow">
  {#if error}
    <div class="flow-error">
      <span class="dot red"></span>
      {error}
    </div>
  {:else}
    <div class="flow-status">
      <span class="spinner"></span>
      {status}
    </div>
  {/if}
</div>

<style>
  .oauth-flow {
    padding: 10px 14px;
    border-top: 1px solid var(--border);
    font-size: 12px;
  }

  .flow-status {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--text-secondary);
  }

  .flow-error {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--red);
  }

  .spinner {
    width: 12px;
    height: 12px;
    border: 2px solid var(--border);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 0.8s linear infinite;
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }
</style>
