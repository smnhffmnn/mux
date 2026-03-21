<script lang="ts">
  import type { TypeListEntry } from '../api'
  import { AddConnection } from '../api'

  let { types, onAdd, onClose }: { types: TypeListEntry[]; onAdd: () => void; onClose: () => void } = $props()

  let name = $state('')
  let selectedType = $state(types[0]?.type ?? '')
  let adding = $state(false)
  let error = $state('')

  async function handleAdd() {
    if (!name.trim() || !selectedType) {
      error = 'Name and type are required'
      return
    }
    adding = true
    error = ''
    try {
      await AddConnection(name.trim(), selectedType)
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
<div class="overlay" onclick={onClose}>
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
  <div class="modal" onclick={(e) => e.stopPropagation()}>
    <div class="modal-header">
      <h3>Add Connection</h3>
      <button class="close-btn" onclick={onClose}>×</button>
    </div>
    <div class="modal-body">
      <div class="field">
        <label for="conn-name">Name</label>
        <input
          id="conn-name"
          type="text"
          placeholder="my-database"
          bind:value={name}
          autofocus
        />
      </div>
      <div class="field">
        <label for="conn-type">Type</label>
        <select id="conn-type" bind:value={selectedType}>
          {#each types as t (t.type)}
            <option value={t.type}>{t.label}</option>
          {/each}
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
