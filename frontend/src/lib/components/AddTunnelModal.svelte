<script lang="ts">
  import { AddTunnel } from '../api'

  let { onAdd, onClose }: { onAdd: () => void; onClose: () => void } = $props()

  let name = $state('')
  let selectedType = $state('wireguard')
  let adding = $state(false)
  let error = $state('')

  async function handleAdd() {
    if (!name.trim()) {
      error = 'Name is required'
      return
    }
    adding = true
    error = ''
    try {
      await AddTunnel(name.trim(), selectedType)
      onAdd()
    } catch (e: any) {
      error = e?.message ?? 'Add failed'
    } finally {
      adding = false
    }
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') onClose()
    if (e.key === 'Enter') handleAdd()
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
<div class="overlay" role="presentation" onclick={onClose}>
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
  <div class="modal" role="presentation" onclick={(e) => e.stopPropagation()}>
    <div class="modal-header">
      <h3>Add Tunnel</h3>
      <button class="close-btn" onclick={onClose}>&times;</button>
    </div>
    <div class="modal-body">
      <div class="field">
        <label for="tunnel-name">Name</label>
        <!-- svelte-ignore a11y_autofocus — focus the name field when the modal opens -->
        <input
          id="tunnel-name"
          type="text"
          placeholder="office-vpn"
          bind:value={name}
          autofocus
        />
      </div>
      <div class="field">
        <label for="tunnel-type">Type</label>
        <select id="tunnel-type" bind:value={selectedType}>
          <option value="wireguard">WireGuard</option>
          <option value="ssh">SSH</option>
        </select>
      </div>
      {#if error}
        <div class="error">{error}</div>
      {/if}
    </div>
    <div class="modal-footer">
      <button class="primary" onclick={handleAdd} disabled={adding}>
        {adding ? 'Adding...' : 'Add'}
      </button>
      <button onclick={onClose}>Cancel</button>
    </div>
  </div>
</div>

<style>
  .overlay {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.5);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 100;
  }

  .modal {
    background: var(--bg-surface);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    width: 380px;
    box-shadow: var(--shadow-lg);
  }

  .modal-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 14px 16px;
    border-bottom: 1px solid var(--border);
  }

  .modal-header h3 {
    font-size: 14px;
    font-weight: 600;
  }

  .close-btn {
    background: none;
    border: none;
    font-size: 18px;
    color: var(--text-muted);
    padding: 0 4px;
    line-height: 1;
  }

  .modal-body {
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 12px;
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

  .field input, .field select {
    width: 100%;
  }

  .error {
    color: var(--red);
    font-size: 12px;
  }

  .modal-footer {
    display: flex;
    gap: 6px;
    padding: 12px 16px;
    border-top: 1px solid var(--border);
    justify-content: flex-end;
  }
</style>
