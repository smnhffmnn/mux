<script lang="ts">
  import type { ConnInfo, SaveConnectionRequest } from '../api'
  import { SaveConnection } from '../api'

  let { conn, tunnelNames = [], onSave, onCancel }: { conn: ConnInfo; tunnelNames?: string[]; onSave: () => void; onCancel: () => void } = $props()

  let saving = $state(false)
  let error = $state('')

  // Build form values from current fields
  let formValues = $state<Record<string, string>>({})

  $effect(() => {
    const vals: Record<string, string> = {}
    for (const f of conn.fields) {
      vals[f.key] = f.value ?? ''
    }
    vals['tunnel'] = conn.tunnel ?? ''
    vals['instructions'] = conn.instructions ?? ''
    formValues = vals
  })

  // Map from TypeField keys (snake_case, user-facing) to the SaveConnectionRequest
  // JSON property names (camelCase). Fields whose TypeField key and JSON name match
  // do not need to be listed.
  const fieldKeyMap: Record<string, string> = {
    client_id: 'clientId',
    token_header: 'tokenHeader',
    token_scheme: 'tokenScheme',
    basic_suffix: 'basicSuffix',
    protected_fields: 'protectedFields',
    set_once_fields: 'setOnceFields',
    key_file: 'keyFile',
    host_key: 'hostKey',
    allow_commands: 'allowCommands',
    deny_commands: 'denyCommands',
    private_key: 'privateKey',
    smtp_host: 'smtpHost',
    smtp_port: 'smtpPort',
    from_name: 'fromName',
    allow_send: 'allowSend',
    smtp_saves_sent: 'smtpSavesSent',
  }

  async function handleSave() {
    saving = true
    error = ''
    try {
      const req: SaveConnectionRequest = {}
      for (const f of conn.fields) {
        const val = formValues[f.key]
        if (f.checkbox) {
          // Always sent, so unticking switches the option off
          const jsonKey = fieldKeyMap[f.key] ?? f.key
          ;(req as any)[jsonKey] = val === 'true'
        } else if (val) {
          const jsonKey = fieldKeyMap[f.key] ?? f.key
          ;(req as any)[jsonKey] = val
        } else if (f.multiline && !f.secret) {
          // Multiline fields (headers) are always sent so they can be cleared;
          // a secret is only sent when the user typed one
          const jsonKey = fieldKeyMap[f.key] ?? f.key
          ;(req as any)[jsonKey] = ''
        }
      }
      req.tunnel = formValues['tunnel'] ?? ''
      req.instructions = formValues['instructions'] ?? ''
      await SaveConnection(conn.name, req)
      onSave()
    } catch (e: any) {
      error = e?.message ?? 'Save failed'
    } finally {
      saving = false
    }
  }
</script>

<div class="form">
  <div class="form-grid">
    {#each conn.fields as field (field.key)}
      <div class="form-field" class:small={field.small} class:full={field.multiline} class:checkbox-field={field.checkbox}>
        {#if !field.checkbox}
          <label for={field.key}>{field.label}</label>
        {/if}
        {#if field.checkbox}
          <label>
            <input
              type="checkbox"
              checked={formValues[field.key] === 'true'}
              onchange={(e) => (formValues[field.key] = e.currentTarget.checked ? 'true' : '')}
              disabled={conn.isProvisioned}
            />
            {field.label}
          </label>
        {:else if field.secret && field.multiline}
          <!-- A PEM key spans many lines; a single-line password input would
               drop the newlines on paste and hand mux an unparsable key. -->
          <textarea
            id={field.key}
            class="secret"
            rows="3"
            autocomplete="off"
            spellcheck="false"
            placeholder={field.secretStored ? '••••• (stored — paste to replace)' : field.placeholder}
            bind:value={formValues[field.key]}
          ></textarea>
        {:else if field.secret}
          <input
            id={field.key}
            type="password"
            placeholder={field.secretStored ? '••••• (stored)' : field.placeholder}
            bind:value={formValues[field.key]}
            disabled={conn.isProvisioned && !field.secret}
          />
        {:else if field.multiline}
          <textarea
            id={field.key}
            rows="2"
            placeholder={field.placeholder}
            bind:value={formValues[field.key]}
            disabled={conn.isProvisioned}
          ></textarea>
        {:else}
          <input
            id={field.key}
            type="text"
            placeholder={field.placeholder}
            bind:value={formValues[field.key]}
            disabled={conn.isProvisioned}
          />
        {/if}
      </div>
    {/each}

    {#if tunnelNames.length > 0 || conn.tunnel}
      <div class="form-field">
        <label for="tunnel">Tunnel</label>
        <select
          id="tunnel"
          bind:value={formValues['tunnel']}
          disabled={conn.isProvisioned}
        >
          <option value="">None</option>
          {#each tunnelNames as name}
            <option value={name}>{name}</option>
          {/each}
          {#if conn.tunnel && !tunnelNames.includes(conn.tunnel)}
            <option value={conn.tunnel}>{conn.tunnel} (missing)</option>
          {/if}
        </select>
      </div>
    {/if}

    <div class="form-field full">
      <label for="instructions">Instructions (MCP)</label>
      <textarea
        id="instructions"
        rows="2"
        placeholder="Custom instructions for AI agents..."
        bind:value={formValues['instructions']}
        disabled={conn.isProvisioned}
      ></textarea>
    </div>
  </div>

  {#if error}
    <div class="form-error">{error}</div>
  {/if}

  <div class="form-actions">
    <button class="primary" onclick={handleSave} disabled={saving}>
      {saving ? 'Saving...' : 'Save'}
    </button>
    <button onclick={onCancel}>Cancel</button>
  </div>
</div>

<style>
  .form-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 10px;
    margin-bottom: 12px;
  }

  .form-field {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .form-field.small {
    max-width: 120px;
  }

  .form-field.full {
    grid-column: 1 / -1;
  }

  .checkbox-field {
    justify-content: flex-end;
  }

  .checkbox-field label {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--text-secondary);
    text-transform: none;
    letter-spacing: normal;
    font-weight: 400;
    cursor: pointer;
  }

  .checkbox-field input[type="checkbox"] {
    width: auto;
    padding: 0;
  }

  label {
    font-size: 11px;
    font-weight: 500;
    color: var(--text-secondary);
    text-transform: uppercase;
    letter-spacing: 0.3px;
  }

  input, select, textarea {
    width: 100%;
  }

  textarea {
    resize: vertical;
    min-height: 40px;
  }

  textarea.secret {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 11px;
    -webkit-text-security: disc;
  }

  .form-error {
    color: var(--red);
    font-size: 12px;
    margin-bottom: 8px;
  }

  .form-actions {
    display: flex;
    gap: 6px;
  }
</style>
