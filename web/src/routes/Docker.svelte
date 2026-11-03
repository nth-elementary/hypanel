<script>
  import { onMount, onDestroy } from 'svelte';
  import { get, post, del, attempt, toast, followTask, confirmDialog, promptDialog, formatBytes, formatTime } from '../lib/api.svelte.js';
  import LogViewer from '../lib/LogViewer.svelte';

  let tab = $state('containers');
  let containers = $state([]);
  let images = $state([]);
  let networks = $state([]);
  let volumes = $state([]);
  let mirrors = $state('');
  let error = $state('');
  let search = $state('');
  let logs = $state(null);
  let stats = $state({});

  const name = (c) => c.Names[0].replace(/^\//, '');
  const visible = $derived(
    containers.filter((c) => (name(c) + ' ' + c.Image).toLowerCase().includes(search.toLowerCase())),
  );

  // docker lists each published port once per IP version, so duplicates are merged.
  function ports(c) {
    const seen = new Map();
    for (const p of c.Ports || []) {
      if (!p.PublicPort) continue;
      const scope = p.IP === '127.0.0.1' || p.IP === '::1' ? 'local' : 'public';
      seen.set(`${scope}:${p.PublicPort}:${p.PrivatePort}`, { scope, host: p.PublicPort, inner: p.PrivatePort });
    }
    return [...seen.values()];
  }

  // Unlimited containers report the whole host as their limit. CPU is a share of the host.
  const hostMemory = $derived(Math.max(0, ...Object.values(stats).map((s) => s.memLimit || 0)));

  function level(percent, warn, bad) {
    if (percent >= bad) return 'bad';
    if (percent >= warn) return 'warn';
    return '';
  }
  const cpuLevel = (st) => level(st.cpu, 50, 80);
  const memPercent = (st) => (st.memLimit ? (st.memUsed / st.memLimit) * 100 : 0);
  const memLevel = (st) => level(memPercent(st), 75, 90);

  async function load() {
    try {
      if (tab === 'containers') containers = await get('/docker/containers');
      if (tab === 'images') images = await get('/docker/images');
      if (tab === 'networks') networks = await get('/docker/networks');
      if (tab === 'volumes') volumes = (await get('/docker/volumes')).Volumes ?? [];
      if (tab === 'mirrors') mirrors = (await get('/docker/mirrors')).mirrors.join('\n');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }

  $effect(() => {
    tab;
    load();
  });

  let timer;
  let stopped = false;

  // The next sample is scheduled only after the last has returned, so a slow server just
  // updates less often rather than piling up requests.
  async function pollStats() {
    if (stopped) return;
    if (tab === 'containers' && !document.hidden) {
      try {
        stats = await get('/docker/stats');
      } catch {
        // The containers list reports connection problems; the figures just go stale.
      }
    }
    setTimeout(pollStats, 2000);
  }

  onMount(() => {
    timer = setInterval(() => {
      if (tab === 'containers') load();
    }, 5000);
    pollStats();
  });
  onDestroy(() => {
    stopped = true;
    clearInterval(timer);
  });

  async function act(c, action) {
    if (action !== 'start') {
      const ok = await confirmDialog({ title: `${action === 'stop' ? 'Stop' : 'Restart'} ${name(c)}?`, action: action === 'stop' ? 'Stop' : 'Restart', danger: action === 'stop' });
      if (!ok) return;
    }
    if (await attempt(() => post(`/docker/containers/${c.Id}/${action}`), `${name(c)}: ${action} done`)) load();
  }

  async function removeContainer(c) {
    const ok = await confirmDialog({
      title: `Remove ${name(c)}?`,
      message: 'Data in bind mounts stays on disk. A compose stack will recreate this container on its next "up".',
      action: 'Remove',
      danger: true,
      typed: name(c),
    });
    if (ok && (await attempt(() => del('/docker/containers/' + c.Id), 'Container removed'))) load();
  }

  async function loadLog(lines) {
    const res = await get(`/docker/containers/${logs.id}/logs?tail=${lines}`);
    return { text: res.logs, size: res.size };
  }

  const vacuumLog = (keepMB) => post(`/docker/containers/${logs.id}/logs/vacuum`, { keepMB });

  async function pull() {
    const image = await promptDialog({ title: 'Pull image', message: 'For example nginx:1.27-alpine', action: 'Pull' });
    if (!image) return;
    const res = await attempt(() => post('/docker/images/pull', { image }));
    if (res) followTask(res.task, () => load());
  }

  async function removeImage(i) {
    const label = i.RepoTags?.[0] ?? i.Id;
    const ok = await confirmDialog({ title: `Remove ${label}?`, action: 'Remove', danger: true });
    if (ok && (await attempt(() => del('/docker/images?id=' + encodeURIComponent(label)), 'Image removed'))) load();
  }

  async function prune() {
    const ok = await confirmDialog({ title: 'Remove unused layers?', message: 'Removes untagged image layers only. Tagged images stay.', action: 'Clean up' });
    if (!ok) return;
    const res = await attempt(() => post('/docker/images/prune'));
    if (res) {
      toast(`Freed ${formatBytes(res.reclaimed)}`);
      load();
    }
  }

  async function createNetwork() {
    const net = await promptDialog({ title: 'New network', action: 'Create' });
    if (net && (await attempt(() => post('/docker/networks', { name: net }), 'Network created'))) load();
  }

  async function removeNetwork(n) {
    const ok = await confirmDialog({ title: `Remove network ${n.Name}?`, action: 'Remove', danger: true });
    if (ok && (await attempt(() => del('/docker/networks/' + n.Id), 'Network removed'))) load();
  }

  async function removeVolume(v) {
    const ok = await confirmDialog({ title: `Delete volume ${v.Name}?`, message: 'Its data is deleted permanently.', action: 'Delete', danger: true, typed: v.Name.slice(0, 12) });
    if (ok && (await attempt(() => del('/docker/volumes/' + encodeURIComponent(v.Name)), 'Volume deleted'))) load();
  }

  async function saveMirrors() {
    const list = mirrors.split(/\s+/).filter(Boolean);
    if (await attempt(() => post('/docker/mirrors', { mirrors: list }), 'Mirrors saved, docker reloaded')) load();
  }
</script>

<div class="page">
  <div class="page-head">
    <h1>Containers</h1>
    <div class="row">
      {#if tab === 'containers'}<input placeholder="Search" bind:value={search} aria-label="Search containers" />{/if}
      {#if tab === 'images'}
        <button onclick={prune}>Clean up</button>
        <button class="primary" onclick={pull}>Pull image</button>
      {/if}
      {#if tab === 'networks'}<button class="primary" onclick={createNetwork}>New network</button>{/if}
    </div>
  </div>

  <div class="tabs" role="tablist">
    {#each [['containers', 'Containers'], ['images', 'Images'], ['networks', 'Networks'], ['volumes', 'Volumes'], ['mirrors', 'Registry mirrors']] as [id, label]}
      <button class:on={tab === id} role="tab" aria-selected={tab === id} onclick={() => (tab = id)}>{label}</button>
    {/each}
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  <div class="card">
    {#if tab === 'containers'}
      {#if !visible.length}
        <div class="empty">No containers.</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Image</th><th>Status</th><th>CPU</th><th>Memory</th><th>Ports</th><th></th></tr></thead>
            <tbody>
              {#each visible as c (c.Id)}
                <tr>
                  <td class="nowrap">{name(c)}</td>
                  <td class="mono muted break">{c.Image}</td>
                  <td>
                    <span class="badge {c.State === 'running' ? 'good' : c.State === 'restarting' ? 'warn' : ''}">{c.State === 'running' ? '●' : '○'} {c.State}</span>
                    <div class="hint nowrap">{c.Status}</div>
                  </td>
                  {#if stats[c.Id]}
                    {@const st = stats[c.Id]}
                    {@const cl = cpuLevel(st)}
                    {@const ml = memLevel(st)}
                    <td class="nowrap small stat {cl}">{cl ? '▲ ' : ''}{st.cpu.toFixed(1)}%</td>
                    <td class="nowrap small stat {ml}">
                      {ml ? '▲ ' : ''}{formatBytes(st.memUsed)}
                      {#if st.memLimit && st.memLimit < hostMemory}
                        <div class="hint">{memPercent(st).toFixed(0)}% of {formatBytes(st.memLimit)} limit</div>
                      {/if}
                    </td>
                  {:else}
                    <td class="muted small">{c.State === 'running' ? '...' : '-'}</td>
                    <td class="muted small">{c.State === 'running' ? '...' : '-'}</td>
                  {/if}
                  <td class="small">
                    {#each ports(c) as p}
                      <div class="nowrap" style:color={p.scope === 'public' ? 'var(--warn)' : ''}>{p.scope}:{p.host} <span class="hint">→ {p.inner}</span></div>
                    {/each}
                  </td>
                  <td class="actions">
                    {#if c.State === 'running'}
                      <a class="btn" href="#/terminal?container={encodeURIComponent(name(c))}">Shell</a>
                      <button onclick={() => act(c, 'restart')}>Restart</button>
                      <button onclick={() => act(c, 'stop')}>Stop</button>
                    {:else}
                      <button onclick={() => act(c, 'start')}>Start</button>
                    {/if}
                    <button onclick={() => (logs = { id: c.Id, title: name(c) })}>Logs</button>
                    <button class="danger" onclick={() => removeContainer(c)}>Remove</button>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    {:else if tab === 'images'}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Image</th><th>Size</th><th>Created</th><th></th></tr></thead>
          <tbody>
            {#each images as i (i.Id)}
              <tr>
                <td class="mono break">{i.RepoTags?.join(', ') || '<untagged> ' + i.Id.slice(7, 19)}</td>
                <td class="nowrap small">{formatBytes(i.Size)}</td>
                <td class="nowrap small muted">{formatTime(i.Created)}</td>
                <td class="actions"><button class="danger" onclick={() => removeImage(i)}>Remove</button></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else if tab === 'networks'}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Driver</th><th>Subnet</th><th></th></tr></thead>
          <tbody>
            {#each networks as n (n.Id)}
              <tr>
                <td>{n.Name}</td>
                <td class="muted">{n.Driver}</td>
                <td class="mono muted">{n.IPAM?.Config?.[0]?.Subnet ?? ''}</td>
                <td class="actions">{#if !['bridge', 'host', 'none'].includes(n.Name)}<button class="danger" onclick={() => removeNetwork(n)}>Remove</button>{/if}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else if tab === 'volumes'}
      {#if !volumes.length}
        <div class="empty">No volumes.</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Driver</th><th>Location</th><th></th></tr></thead>
            <tbody>
              {#each volumes as v (v.Name)}
                <tr>
                  <td class="mono break">{v.Name}</td>
                  <td class="muted">{v.Driver}</td>
                  <td class="mono muted break">{v.Mountpoint}</td>
                  <td class="actions"><button class="danger" onclick={() => removeVolume(v)}>Delete</button></td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    {:else}
      <div class="card-pad form">
        <label class="field">Registry mirrors, one URL per line
          <textarea rows="5" bind:value={mirrors} placeholder="https://mirror.example.org"></textarea>
          <span class="hint">Stored in /etc/docker/daemon.json. Applied by reloading docker, so running containers aren't restarted.</span>
        </label>
        <div><button class="primary" onclick={saveMirrors}>Save</button></div>
      </div>
    {/if}
  </div>
</div>

{#if logs}
  <LogViewer
    title="Logs: {logs.title}"
    load={loadLog}
    vacuum={vacuumLog}
    vacuumNote="This restarts {logs.title}, so it's briefly unavailable. Anything that depends on it is affected too. Docker can't trim the log of a running container safely."
    onclose={() => (logs = null)}
  />
{/if}
