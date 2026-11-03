<script>
  import { onMount } from 'svelte';
  import { get, post, del, attempt, toast, confirmDialog } from '../lib/api.svelte.js';

  let status = $state(null);
  let error = $state('');
  let form = $state({ action: 'allow', port: '', proto: 'tcp', from: '' });
  let formError = $state('');
  let block = $state('');

  async function load() {
    try {
      status = await get('/firewall/status');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  async function add(e) {
    e.preventDefault();
    formError = '';
    try {
      await post('/firewall/rules', { action: form.action, port: form.port, proto: form.port ? form.proto : '', from: form.from });
      toast('Rule added');
      form = { ...form, port: '', from: '' };
      load();
    } catch (err) {
      formError = err.message;
    }
  }

  async function blockIP(e) {
    e.preventDefault();
    if (await attempt(() => post('/firewall/rules', { action: 'deny', from: block }), `Blocked ${block}`)) {
      block = '';
      load();
    }
  }

  async function remove(rule) {
    const ok = await confirmDialog({ title: 'Delete this rule?', message: rule.raw, action: 'Delete', danger: true });
    if (ok && (await attempt(() => del(`/firewall/rules/${rule.num}?raw=${encodeURIComponent(rule.raw)}`), 'Rule deleted'))) load();
    else if (ok) load();
  }

  async function toggle() {
    if (status.active) {
      const ok = await confirmDialog({ title: 'Turn the firewall off?', message: 'All ports become reachable unless your cloud security group blocks them.', action: 'Turn off', danger: true });
      if (ok && (await attempt(() => post('/firewall/disable'), 'Firewall off'))) load();
    } else {
      const ok = await confirmDialog({ title: 'Turn the firewall on?', message: 'SSH is allowed automatically, so you keep access. Check that 80 and 443 are allowed too.', action: 'Turn on' });
      if (ok && (await attempt(() => post('/firewall/enable'), 'Firewall on'))) load();
    }
  }
</script>

<div class="page">
  <div class="page-head">
    <div class="row">
      <h1>Firewall</h1>
      {#if status}
        {#if status.active}<span class="badge good">● Active</span>{:else}<span class="badge warn">▲ Inactive</span>{/if}
      {/if}
    </div>
    {#if status}<button class:danger={status.active} class:primary={!status.active} onclick={toggle}>{status.active ? 'Turn off' : 'Turn on'}</button>{/if}
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  {#if status?.dockerPorts?.length}
    <div class="notice">
      <b>▲ These container ports are public, and this firewall can't block them</b>, because docker sets its own rules. To close one, bind it to 127.0.0.1 in its compose file.
      <ul style="margin:6px 0 0;padding-left:18px">
        {#each status.dockerPorts as p}<li>{p.container}: <code>{p.port}</code></li>{/each}
      </ul>
    </div>
  {/if}

  <div class="charts">
    <form class="card card-pad form" onsubmit={add}>
      <h2>Add a rule</h2>
      <div class="form-grid">
        <label class="field">Action
          <select bind:value={form.action}><option value="allow">Allow</option><option value="deny">Deny</option></select>
        </label>
        <label class="field">Port or range <input class="mono" bind:value={form.port} placeholder="8080 or 3000:3010" /></label>
        <label class="field">Protocol
          <select bind:value={form.proto}><option value="tcp">TCP</option><option value="udp">UDP</option><option value="">Both</option></select>
        </label>
        <label class="field">From <input class="mono" bind:value={form.from} placeholder="Anywhere, or IP / CIDR" /></label>
      </div>
      {#if formError}<div class="error">{formError}</div>{/if}
      <div><button class="primary">Add rule</button></div>
    </form>

    <form class="card card-pad form" onsubmit={blockIP}>
      <h2>Block an address</h2>
      <label class="field">IP or CIDR range <input class="mono" bind:value={block} placeholder="203.0.113.7" required /></label>
      <div><button class="danger">Block</button></div>
    </form>
  </div>

  <div class="card">
    {#if !status?.rules?.length}
      <div class="empty">No rules.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>#</th><th>Port</th><th>Action</th><th>From</th><th>IP version</th><th></th></tr></thead>
          <tbody>
            {#each status.rules as r (r.num)}
              <tr>
                <td class="muted small">{r.num}</td>
                <td class="mono break">{r.to}</td>
                <td><span class="badge {r.action.startsWith('ALLOW') ? 'good' : 'bad'}">{r.action.startsWith('ALLOW') ? '✓' : '✕'} {r.action}</span></td>
                <td class="mono">{r.from}</td>
                <td class="small muted">{r.v6 ? 'IPv6' : 'IPv4'}</td>
                <td class="actions"><button class="danger" onclick={() => remove(r)}>Delete</button></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>
