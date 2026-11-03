<script>
  import { onMount } from 'svelte';
  import { get, post, del, attempt, toast, confirmDialog } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';
  import Editor from '../lib/Editor.svelte';
  import LogViewer from '../lib/LogViewer.svelte';

  let sites = $state([]);
  let error = $state('');
  let search = $state('');
  let creating = $state(null);
  let formError = $state('');
  let coverage = $state(null);
  let editing = $state(null);
  let logs = $state(null);

  const visible = $derived(
    sites.filter((s) => (s.name + ' ' + s.domains.join(' ')).toLowerCase().includes(search.toLowerCase())),
  );
  const splitDomains = (text) => text.trim().split(/[\s,]+/).filter(Boolean);
  const coveredDomains = $derived(coverage?.domains?.filter((d) => d.covered) ?? []);

  async function load() {
    try {
      sites = await get('/websites');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  function startCreate() {
    formError = '';
    coverage = null;
    creating = {
      kind: 'proxy', domains: '', name: '', target: 'http://127.0.0.1:',
      hostMode: '$host', customHost: '', cache: false, cacheMinutes: 10,
      addDns: true, proxied: true, busy: false,
    };
  }

  // Shows what will happen with DNS and HTTPS while the domains are being typed.
  $effect(() => {
    const text = creating?.domains;
    if (!text?.trim()) {
      coverage = null;
      return;
    }
    const timer = setTimeout(async () => {
      try {
        coverage = await get('/websites/dns-coverage?domains=' + encodeURIComponent(splitDomains(text).join(',')));
      } catch {
        coverage = null;
      }
    }, 500);
    return () => clearTimeout(timer);
  });

  // The site name defaults to the first domain.
  const createName = $derived(creating ? creating.name || splitDomains(creating.domains)[0] || '' : '');

  function reportDNS(results = []) {
    const created = results.filter((r) => r.status === 'created').map((r) => r.domain);
    const failed = results.filter((r) => r.status === 'failed');
    if (failed.length) {
      toast(`Website created, but DNS failed for ${failed.map((r) => `${r.domain}: ${r.detail}`).join('; ')}`, true);
    } else if (created.length) {
      toast(`Website created, DNS record added for ${created.join(', ')}`);
    } else {
      toast('Website created');
    }
  }

  async function create() {
    formError = '';
    creating.busy = true;
    try {
      const isProxy = creating.kind === 'proxy';
      const res = await post('/websites', {
        name: createName,
        domains: splitDomains(creating.domains),
        kind: creating.kind,
        target: creating.kind === 'static' ? '' : creating.target,
        proxyHost: !isProxy ? '' : creating.hostMode === 'custom' ? creating.customHost : creating.hostMode,
        cache: isProxy && creating.cache,
        cacheMinutes: isProxy && creating.cache ? Number(creating.cacheMinutes) : 0,
        addDns: creating.addDns && coveredDomains.length > 0,
        proxied: creating.proxied,
      });
      reportDNS(res.dns);
      creating = null;
      load();
    } catch (e) {
      formError = e.message;
      creating.busy = false;
    }
  }

  async function edit(site) {
    const res = await attempt(() => get(`/websites/${encodeURIComponent(site.name)}/conf`));
    if (res) editing = { name: site.name, content: res.content, path: res.path, error: '' };
  }

  async function saveConf() {
    editing.error = '';
    try {
      await post(`/websites/${encodeURIComponent(editing.name)}/conf`, { content: editing.content });
      toast('Saved and reloaded');
      editing = null;
      load();
    } catch (e) {
      editing.error = e.message;
    }
  }

  async function toggle(site) {
    const action = site.enabled ? 'disable' : 'enable';
    if (await attempt(() => post(`/websites/${encodeURIComponent(site.name)}/${action}`), `Site ${action}d`)) load();
  }

  async function remove(site) {
    const ok = await confirmDialog({
      title: `Delete ${site.name}?`,
      message: 'This removes the nginx config. Files in /www/sites and any DNS records stay in place.',
      action: 'Delete',
      danger: true,
      typed: site.name,
    });
    if (ok && (await attempt(() => del(`/websites/${encodeURIComponent(site.name)}`), 'Website deleted'))) load();
  }

  async function loadLog(lines) {
    const res = await get(`/websites/${encodeURIComponent(logs.site.name)}/log?kind=${logs.kind}&lines=${lines}`);
    return { text: res.log, size: res.size };
  }

  const vacuumLog = (keepMB) => post(`/websites/${encodeURIComponent(logs.site.name)}/log/vacuum`, { keepMB });

  async function reload() {
    await attempt(() => post('/websites/reload'), 'nginx reloaded');
  }

  const kinds = { proxy: 'Reverse proxy', php: 'PHP', static: 'Static' };
</script>

<div class="page">
  <div class="page-head">
    <h1>Websites</h1>
    <div class="row">
      <input placeholder="Search" bind:value={search} aria-label="Search websites" />
      <button onclick={reload}>Reload nginx</button>
      <button class="primary" onclick={startCreate}>New website</button>
    </div>
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  <div class="card">
    {#if !visible.length}
      <div class="empty">{sites.length ? 'No matches.' : 'No websites yet.'}</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Domains</th><th>Type</th><th>Target</th><th>HTTPS</th><th>Status</th><th></th></tr></thead>
          <tbody>
            {#each visible as s (s.name)}
              <tr>
                <td>
                  {#each s.domains as d}
                    <div class="break"><a href={(s.https ? 'https://' : 'http://') + d} target="_blank" rel="noreferrer">{d}</a></div>
                  {/each}
                  {#if s.domains[0] !== s.name}<div class="muted small">{s.name}</div>{/if}
                </td>
                <td class="nowrap">{kinds[s.kind]}</td>
                <td class="mono muted break">{s.target}</td>
                <td>
                  {#if s.https}<span class="badge good">✓ On</span>
                  {:else}<a class="btn" href="#/ssl?site={encodeURIComponent(s.name)}&domains={encodeURIComponent(s.domains.join(','))}">Enable</a>{/if}
                </td>
                <td>
                  {#if s.enabled}<span class="badge good">● Enabled</span>{:else}<span class="badge">○ Disabled</span>{/if}
                </td>
                <td class="actions">
                  <a class="btn" href="#/files?path=/www/sites/{encodeURIComponent(s.name)}">Files</a>
                  <button onclick={() => (logs = { site: s, kind: 'access' })}>Logs</button>
                  <button onclick={() => edit(s)}>Config</button>
                  <button onclick={() => toggle(s)}>{s.enabled ? 'Disable' : 'Enable'}</button>
                  <button class="danger" onclick={() => remove(s)}>Delete</button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>

{#if creating}
  <Modal title="New website" onclose={() => (creating = null)}>
    <form class="form" id="new-site" onsubmit={(e) => { e.preventDefault(); create(); }}>
      <label class="field">Domains
        <input bind:value={creating.domains} placeholder="example.org www.example.org" required />
        <span class="hint">Separate several domains with spaces.</span>
      </label>

      {#if coveredDomains.length}
        <div class="card card-pad stack" style="padding:12px">
          <label class="row small"><input type="checkbox" bind:checked={creating.addDns} /> Add DNS records in Cloudflare, pointing at {coverage.serverIp || 'this server'}</label>
          {#if creating.addDns}
            <label class="row small"><input type="checkbox" bind:checked={creating.proxied} /> Proxy through Cloudflare (hides this server's address)</label>
            <div class="hint">
              {#each coverage.domains as d}
                <div>
                  {d.domain}:
                  {#if !d.covered}not in any stored account, add its record yourself
                  {:else if d.existing}already has a record ({d.existing}), left unchanged
                  {:else}will be added to {d.zone}{/if}
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {:else if coverage}
        <div class="hint">No stored Cloudflare account covers this domain, so point its DNS at {coverage.serverIp || 'this server'} yourself.</div>
      {/if}
      {#if coverage?.cert}<div class="hint">✓ HTTPS will be on from the start, using a wildcard certificate already on this server.</div>{/if}

      <label class="field">Type
        <select bind:value={creating.kind}>
          <option value="proxy">Reverse proxy (Node, Python, a container)</option>
          <option value="php">PHP (PHP-FPM environment)</option>
          <option value="static">Static files</option>
        </select>
      </label>
      {#if creating.kind === 'proxy'}
        <label class="field">Proxy to <input class="mono" bind:value={creating.target} placeholder="http://127.0.0.1:3000" required /></label>
        <div class="form-grid">
          <label class="field">Host header sent upstream
            <select bind:value={creating.hostMode}>
              <option value="$host">The visitor's host ($host)</option>
              <option value="$proxy_host">The target's host ($proxy_host)</option>
              <option value="custom">Custom...</option>
            </select>
          </label>
          {#if creating.hostMode === 'custom'}
            <label class="field">Custom host <input class="mono" bind:value={creating.customHost} placeholder="upstream.example.org" required /></label>
          {/if}
        </div>
        <label class="row small"><input type="checkbox" bind:checked={creating.cache} /> Cache responses</label>
        {#if creating.cache}
          <label class="field">Keep cached responses for (minutes)
            <input type="number" min="1" max="43200" bind:value={creating.cacheMinutes} required />
            <span class="hint">Requests carrying cookies or a login skip the cache, so signed-in pages stay private.</span>
          </label>
        {/if}
      {:else if creating.kind === 'php'}
        <label class="field">PHP-FPM address
          <input class="mono" bind:value={creating.target} placeholder="127.0.0.1:9000" required />
          <span class="hint">The port of a PHP environment. <a href="#/runtimes" onclick={() => (creating = null)}>Create one</a> if you need to.</span>
        </label>
      {/if}
      <label class="field">Site name (optional) <input bind:value={creating.name} placeholder={createName} /></label>
      {#if creating.kind !== 'proxy'}<div class="hint">Files go in <code>/www/sites/{createName || '<name>'}/index</code>.</div>{/if}
      {#if formError}<div class="error">{formError}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (creating = null)}>Cancel</button>
      <button class="primary" type="submit" form="new-site" disabled={creating.busy}>{creating.busy ? 'Creating...' : 'Create'}</button>
    {/snippet}
  </Modal>
{/if}

{#if editing}
  <Modal title={editing.path} wide onclose={() => (editing = null)}>
    <Editor bind:value={editing.content} filename="nginx.conf" />
    {#if editing.error}<div class="error">{editing.error}</div>{/if}
    {#snippet foot()}
      <span class="hint grow">Tested with nginx -t before reload; a rejected config is rolled back.</span>
      <button onclick={() => (editing = null)}>Close</button>
      <button class="primary" onclick={saveConf}>Save and reload</button>
    {/snippet}
  </Modal>
{/if}

{#if logs}
  <LogViewer title="Logs: {logs.site.name}" key={logs.kind} load={loadLog} vacuum={vacuumLog} onclose={() => (logs = null)}>
    {#snippet controls()}
      <button class:primary={logs.kind === 'access'} onclick={() => (logs.kind = 'access')}>Access</button>
      <button class:primary={logs.kind === 'error'} onclick={() => (logs.kind = 'error')}>Error</button>
    {/snippet}
  </LogViewer>
{/if}
