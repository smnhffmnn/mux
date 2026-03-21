<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { StartDeviceAuth, GetDeviceAuthStatus } from '../api'
  import { Browser } from '@wailsio/runtime'

  let { name, onComplete }: { name: string; onComplete: () => void } = $props()

  let userCode = $state('')
  let verificationURI = $state('')
  let status = $state('Requesting device code...')
  let error = $state('')
  let pollInterval: ReturnType<typeof setInterval>

  onMount(async () => {
    try {
      const result = await StartDeviceAuth(name)
      userCode = result.userCode
      verificationURI = result.verificationURI
      status = 'Enter the code below at the verification URL'
      startPolling()
    } catch (e: any) {
      error = e?.message ?? 'Device auth start failed'
    }
  })

  onDestroy(() => {
    if (pollInterval) clearInterval(pollInterval)
  })

  function openVerification() {
    if (verificationURI) Browser.OpenURL(verificationURI)
  }

  function startPolling() {
    pollInterval = setInterval(async () => {
      try {
        const s = await GetDeviceAuthStatus(name)
        if (s.completed) {
          clearInterval(pollInterval)
          onComplete()
        } else {
          status = s.message
        }
      } catch {
        // ignore poll errors
      }
    }, 5000)
  }
</script>

<div class="device-flow">
  {#if error}
    <div class="flow-error">
      <span class="dot red"></span>
      {error}
    </div>
  {:else if userCode}
    <div class="flow-content">
      <div class="code-display">
        <span class="code-label">Code:</span>
        <span class="code-value">{userCode}</span>
      </div>
      <button onclick={openVerification}>Open {verificationURI}</button>
      <div class="flow-status">
        <span class="spinner"></span>
        {status}
      </div>
    </div>
  {:else}
    <div class="flow-status">
      <span class="spinner"></span>
      {status}
    </div>
  {/if}
</div>

<style>
  .device-flow {
    padding: 10px 14px;
    border-top: 1px solid var(--border);
    font-size: 12px;
  }

  .flow-content {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }

  .code-display {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .code-label {
    color: var(--text-secondary);
  }

  .code-value {
    font-family: var(--font-mono);
    font-size: 18px;
    font-weight: 700;
    color: var(--accent);
    letter-spacing: 2px;
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
