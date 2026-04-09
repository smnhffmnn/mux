<script lang="ts">
  import type { TunnelInfo, SaveTunnelRequest } from '../api'
  import { SaveTunnel } from '../api'

  let { tunnel, onSave, onCancel }: { tunnel: TunnelInfo; onSave: () => void; onCancel: () => void } = $props()

  let saving = $state(false)
  let error = $state('')

  let formValues = $state<Record<string, string>>({})
  let insecureHostKey = $state(false)

  $effect(() => {
    if (tunnel.type === 'ssh') {
      formValues = {
        host: tunnel.host ?? '',
        port: tunnel.port ? String(tunnel.port) : '22',
        user: tunnel.user ?? '',
        keyFile: tunnel.keyFile ?? '',
        privateKey: '',
      }
      insecureHostKey = tunnel.insecureHostKey ?? false
    } else {
      formValues = {
        peerPublicKey: tunnel.peerPublicKey ?? '',
        peerEndpoint: tunnel.peerEndpoint ?? '',
        allowedIPs: tunnel.allowedIPs ?? '',
        tunnelAddress: tunnel.tunnelAddress ?? '',
        dns: tunnel.dns ?? '',
        mtu: tunnel.mtu ? String(tunnel.mtu) : '1420',
        keepAlive: tunnel.keepAlive ? String(tunnel.keepAlive) : '25',
        privateKey: '',
        presharedKey: '',
      }
    }
  })

  const isProvisioned = $derived(tunnel.source === 'provisioning')

  async function handleSave() {
    saving = true
    error = ''
    try {
      const req: SaveTunnelRequest = {}
      if (tunnel.type === 'ssh') {
        if (formValues.host) req.host = formValues.host
        if (formValues.port) req.port = formValues.port
        if (formValues.user) req.user = formValues.user
        if (formValues.keyFile) req.keyFile = formValues.keyFile
        if (formValues.privateKey) req.privateKey = formValues.privateKey
        req.insecureHostKey = insecureHostKey
      } else {
        if (formValues.peerPublicKey) req.peerPublicKey = formValues.peerPublicKey
        if (formValues.peerEndpoint) req.peerEndpoint = formValues.peerEndpoint
        if (formValues.allowedIPs) req.allowedIPs = formValues.allowedIPs
        if (formValues.tunnelAddress) req.tunnelAddress = formValues.tunnelAddress
        if (formValues.dns) req.dns = formValues.dns
        if (formValues.mtu) req.mtu = formValues.mtu
        if (formValues.keepAlive) req.keepAlive = formValues.keepAlive
        if (formValues.privateKey) req.privateKey = formValues.privateKey
        if (formValues.presharedKey) req.presharedKey = formValues.presharedKey
      }
      await SaveTunnel(tunnel.name, req)
      onSave()
    } catch (e: any) {
      error = e?.message ?? 'Save failed'
    } finally {
      saving = false
    }
  }
</script>

<div class="form">
  {#if tunnel.type === 'ssh'}
    <div class="form-grid">
      <div class="form-field">
        <label for="host">Host</label>
        <input id="host" type="text" placeholder="bastion.example.com" bind:value={formValues.host} disabled={isProvisioned} />
      </div>
      <div class="form-field small">
        <label for="port">Port</label>
        <input id="port" type="text" placeholder="22" bind:value={formValues.port} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="user">User</label>
        <input id="user" type="text" placeholder="ubuntu" bind:value={formValues.user} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="keyFile">Key File</label>
        <input id="keyFile" type="text" placeholder="~/.ssh/id_rsa" bind:value={formValues.keyFile} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="privateKey">Private Key (PEM)</label>
        <input id="privateKey" type="password" placeholder={tunnel.privateKeySet ? '••••• (stored)' : 'Paste PEM key...'} bind:value={formValues.privateKey} />
      </div>
      <div class="form-field checkbox-field">
        <label>
          <input type="checkbox" bind:checked={insecureHostKey} disabled={isProvisioned} />
          Skip host key verification
        </label>
      </div>
    </div>
  {:else}
    <div class="form-grid">
      <div class="form-field">
        <label for="peerEndpoint">Peer Endpoint</label>
        <input id="peerEndpoint" type="text" placeholder="vpn.example.com:51820" bind:value={formValues.peerEndpoint} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="tunnelAddress">Tunnel Address</label>
        <input id="tunnelAddress" type="text" placeholder="10.100.0.42/32" bind:value={formValues.tunnelAddress} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="peerPublicKey">Peer Public Key</label>
        <input id="peerPublicKey" type="text" placeholder="base64 public key" bind:value={formValues.peerPublicKey} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="allowedIPs">Allowed IPs</label>
        <input id="allowedIPs" type="text" placeholder="10.100.0.0/16" bind:value={formValues.allowedIPs} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="dns">DNS</label>
        <input id="dns" type="text" placeholder="10.100.0.1 (optional)" bind:value={formValues.dns} disabled={isProvisioned} />
      </div>
      <div class="form-field small">
        <label for="mtu">MTU</label>
        <input id="mtu" type="text" placeholder="1420" bind:value={formValues.mtu} disabled={isProvisioned} />
      </div>
      <div class="form-field small">
        <label for="keepAlive">Keep-Alive (s)</label>
        <input id="keepAlive" type="text" placeholder="25" bind:value={formValues.keepAlive} disabled={isProvisioned} />
      </div>
      <div class="form-field">
        <label for="privateKey">Private Key</label>
        <input id="privateKey" type="password" placeholder={tunnel.privateKeySet ? '••••• (stored)' : 'base64 private key'} bind:value={formValues.privateKey} />
      </div>
      <div class="form-field">
        <label for="presharedKey">Preshared Key</label>
        <input id="presharedKey" type="password" placeholder={tunnel.presharedKeySet ? '••••• (stored)' : 'base64 preshared key (optional)'} bind:value={formValues.presharedKey} />
      </div>
    </div>
  {/if}

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

  input, select {
    width: 100%;
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
