<script lang="ts">
  import type { TestResult } from '../api'

  let { result }: { result: TestResult } = $props()
</script>

<div class="test-result" class:success={result.connected} class:error={!result.connected}>
  <span class="dot" class:green={result.connected} class:red={!result.connected}></span>
  <span class="message">{result.message}</span>
  {#if result.latency}
    <span class="latency">{result.latency}</span>
  {/if}
</div>

<style>
  .test-result {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 8px 14px;
    font-size: 12px;
    border-top: 1px solid var(--border);
  }

  .test-result.success {
    background: var(--green-bg);
  }

  .test-result.error {
    background: var(--red-bg);
  }

  .message {
    flex: 1;
  }

  .latency {
    color: var(--text-muted);
    font-family: var(--font-mono);
    font-size: 11px;
  }
</style>
