<script lang="ts">
  import { serverInfo } from '../stores'
  import { SelfUpdate } from '../api'
  import type { UpdateResult } from '../api'

  let updating = $state(false)
  let updateResult = $state<UpdateResult | null>(null)

  async function handleUpdate() {
    updating = true
    updateResult = null
    try {
      updateResult = await SelfUpdate()
    } catch (e: any) {
      updateResult = { success: false, message: e?.message ?? 'Update failed' }
    } finally {
      updating = false
    }
  }
</script>

<header class="header drag">
  <div class="header-left">
    <span class="uptime-badge">
      {#if $serverInfo}
        <span class="dot green"></span>
        {$serverInfo.uptime}
      {/if}
    </span>
  </div>
  <div class="header-right">
    {#if updateResult}
      <span class="update-msg" class:success={updateResult.success} class:error={!updateResult.success}>
        {updateResult.message}
      </span>
    {/if}
    <button onclick={handleUpdate} disabled={updating}>
      {updating ? 'Updating...' : 'Check Update'}
    </button>
  </div>
</header>

<style>
  .header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding-bottom: 16px;
    border-bottom: 1px solid var(--border);
  }

  .header-left {
    display: flex;
    align-items: center;
    gap: 12px;
  }

  .header-right {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .uptime-badge {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--text-secondary);
    background: var(--bg-surface);
    border: 1px solid var(--border);
    padding: 4px 10px;
    border-radius: 20px;
  }

  .update-msg {
    font-size: 11px;
  }
  .update-msg.success { color: var(--green); }
  .update-msg.error { color: var(--red); }
</style>
