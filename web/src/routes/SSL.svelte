<script>
  import { onMount } from 'svelte';
  import { get, post, del, attempt, toast, followTask, confirmDialog, formatTime } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';

  let { params } = $props();

  let certs = $state([]);
  let sites = $state([]);
  let accounts = $state([]);
  let providers = $state({});
  let error = $state('');
  let issue = $state(null);
  let upload = $state(null);
  let account = $state(null);

  async function load() {
    try {
      [certs, sites, accounts, providers] = await Promise.all([get('/ssl/certs'), get('/websites'), get('/ssl/dns'), get('/ssl/providers')]);
      error = '';
    } catch (e) {
      error = e.message;
    }
  }

  onMount(async () => {
    await load();
    if (params.get('site')) {
      issue = { site: params.get('site'), domains: (params.get('domains') || '').replaceAll(',', ' '), method: 'http', email: '', error: '' };
    }
  });

  function status(c) {
    if (c.error) return { cls: 'bad', text: '✕ Unreadable' };
    if (c.daysLeft < 0) return { cls: 'bad', text: '✕ Expired' };
    if (c.daysLeft < 15) return { cls: 'bad', text: `▲ ${c.daysLeft} days left` };
    if (c.daysLeft < 30) return { cls: 'warn', text: `▲ ${c.daysLeft} days left` };
    return { cls: 'good', text: `✓ ${c.daysLeft} days left` };
  }

  async function submitIssue() {
    issue.error = '';
    try {
      const res = await post('/ssl/issue', {
        site: issue.site,
        domains: issue.domains.trim().split(/[\s,]+/).filter(Boolean),
        method: issue.method,
        email: issue.email,
      });
      issue = null;
      followTask(res.task, () => load());
    } catch (e) {
      issue.error = e.message;
    }
  }

  async function renew(cert) {
    const domain = cert.domains.find((d) => !d.startsWith('*.')) ?? cert.domains[0];
    const res = await attempt(() => post('/ssl/renew', { domain }));
    if (res) followTask(res.task, () => load());
  }

  async function submitUpload() {
    upload.error = '';
    try {
      await post('/ssl/upload', { site: upload.site, cert: upload.cert, key: upload.key });
      toast('Certificate installed');
      upload = null;
      load();
    } catch (e) {
      upload.error = e.message;
    }
  }

  async function saveAccount() {
    account.error = '';
    try {
      await post('/ssl/dns', { name: account.name, provider: account.provider, creds: account.creds });
      toast('DNS account saved');
      account = null;
      load();
    } catch (e) {
      account.error = e.message;
    }
  }

  async function removeAccount(a) {
    const ok = await confirmDialog({ title: `Remove ${a.name}?`, message: 'Existing certificates keep working.', action: 'Remove', danger: true });
    if (ok && (await attempt(() => del('/ssl/dns/' + encodeURIComponent(a.name)), 'Removed'))) load();
  }

  const siteNames = $derived(sites.map((s) => s.name));
</script>

<div class="page">
  <div class="page-head">
    <h1>SSL certificates</h1>
    <div class="row">
      <button onclick={() => (upload = { site: siteNames[0] ?? '', cert: '', key: '', error: '' })}>Upload</button>
      <button class="primary" onclick={() => (issue = { site: siteNames[0] ?? '', domains: '', method: 'http', email: '', error: '' })}>Issue certificate</button>
    </div>
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  <div class="card">
    <div class="card-head"><h2>In use by websites</h2></div>
    {#if !certs.length}
      <div class="empty">No website uses a certificate yet.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Domains</th><th>Used by</th><th>Issuer</th><th>Expires</th><th>Status</th><th></th></tr></thead>
          <tbody>
            {#each certs as c (c.path)}
              {@const st = status(c)}
              <tr>
                <td class="break">{c.domains.join(', ') || c.path}</td>
                <td class="small muted">{c.sites.join(', ')}</td>
                <td class="small nowrap">{c.issuer}</td>
                <td class="small nowrap">{formatTime(c.notAfter)}</td>
                <td><span class="badge {st.cls}">{st.text}</span></td>
                <td class="actions">{#if c.domains.length}<button onclick={() => renew(c)}>Renew</button>{/if}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>

  <div class="card">
    <div class="card-head">
      <div><h2>DNS accounts</h2><div class="hint">Needed for wildcard certificates and for sites behind a proxy like Cloudflare.</div></div>
      <button onclick={() => (account = { name: '', provider: 'cloudflare', creds: {}, error: '' })}>Add account</button>
    </div>
    {#if !accounts.length}
      <div class="empty">No DNS accounts.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <tbody>
            {#each accounts as a (a.name)}
              <tr><td>{a.name}</td><td class="muted">{a.provider}</td><td class="actions"><button class="danger" onclick={() => removeAccount(a)}>Remove</button></td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>

{#if issue}
  <Modal title="Issue a Let's Encrypt certificate" onclose={() => (issue = null)}>
    <form class="form" id="issue" onsubmit={(e) => { e.preventDefault(); submitIssue(); }}>
      <label class="field">Website
        <select bind:value={issue.site}>{#each siteNames as n}<option value={n}>{n}</option>{/each}</select>
        <span class="hint">The certificate is installed here, and HTTPS is switched on for it.</span>
      </label>
      <label class="field">Domains
        <input bind:value={issue.domains} placeholder="example.org *.example.org" required />
      </label>
      <label class="field">Validation
        <select bind:value={issue.method}>
          <option value="http">HTTP (domain must already point at this server)</option>
          {#each accounts as a}<option value="dns:{a.name}">DNS: {a.name}</option>{/each}
        </select>
      </label>
      <label class="field">Email (first certificate only)
        <input type="email" bind:value={issue.email} placeholder="For expiry notices from Let's Encrypt" />
      </label>
      {#if issue.error}<div class="error">{issue.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (issue = null)}>Cancel</button>
      <button class="primary" type="submit" form="issue">Issue</button>
    {/snippet}
  </Modal>
{/if}

{#if upload}
  <Modal title="Upload a certificate" onclose={() => (upload = null)}>
    <form class="form" id="upload" onsubmit={(e) => { e.preventDefault(); submitUpload(); }}>
      <label class="field">Website
        <select bind:value={upload.site}>{#each siteNames as n}<option value={n}>{n}</option>{/each}</select>
      </label>
      <label class="field">Certificate (full chain, PEM) <textarea rows="6" bind:value={upload.cert} required></textarea></label>
      <label class="field">Private key (PEM) <textarea rows="6" bind:value={upload.key} required></textarea></label>
      {#if upload.error}<div class="error">{upload.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (upload = null)}>Cancel</button>
      <button class="primary" type="submit" form="upload">Install</button>
    {/snippet}
  </Modal>
{/if}

{#if account}
  <Modal title="Add DNS account" onclose={() => (account = null)}>
    <form class="form" id="account" onsubmit={(e) => { e.preventDefault(); saveAccount(); }}>
      <label class="field">Name <input bind:value={account.name} placeholder="cloudflare-main" required /></label>
      <label class="field">Provider
        <select bind:value={account.provider} onchange={() => (account.creds = {})}>
          {#each Object.keys(providers) as p}<option value={p}>{p}</option>{/each}
        </select>
      </label>
      {#each providers[account.provider] ?? [] as key}
        <label class="field">{key} <input type="password" autocomplete="off" bind:value={account.creds[key]} required /></label>
      {/each}
      {#if account.error}<div class="error">{account.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (account = null)}>Cancel</button>
      <button class="primary" type="submit" form="account">Save</button>
    {/snippet}
  </Modal>
{/if}
