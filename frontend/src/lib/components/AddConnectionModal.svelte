<script lang="ts">
  import type { TypeListEntry } from '../api'
  import { AddConnection, GetSetupDoc } from '../api'
  import { marked } from 'marked'
  import DOMPurify from 'dompurify'

  let { types, onAdd, onClose }: { types: TypeListEntry[]; onAdd: () => void; onClose: () => void } = $props()

  let name = $state('')
  let selectedType = $state(types[0]?.type ?? '')
  let adding = $state(false)
  let error = $state('')
  let setupDoc = $state('')

  $effect(() => {
    const current = selectedType
    if (!current) {
      setupDoc = ''
      return
    }
    GetSetupDoc(current)
      .then(doc => { if (current === selectedType) setupDoc = doc })
      .catch(() => { if (current === selectedType) setupDoc = '' })
  })

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
      {#if setupDoc}
        <div class="setup-doc">
          {@html DOMPurify.sanitize(marked(setupDoc) as string)}
        </div>
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
    transition: width 0.15s ease;
  }

  .modal:has(.setup-doc) {
    width: 520px;
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

  .setup-doc {
    max-height: 300px;
    overflow-y: auto;
    padding: 12px;
    background: var(--bg-inset);
    border: 1px solid var(--border);
    border-radius: 6px;
    font-size: 12px;
    line-height: 1.6;
    color: var(--text-secondary);
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
    background: var(--bg-surface);
    padding: 1px 4px;
    border-radius: 3px;
    border: 1px solid var(--border);
  }

  .setup-doc :global(pre) {
    background: var(--bg-surface);
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
    background: var(--bg-surface);
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
