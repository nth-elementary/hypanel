<script>
  import { onMount } from 'svelte';
  import { ui, get, formatBytes, formatDuration } from '../lib/api.svelte.js';
  import Chart from '../lib/Chart.svelte';

  let now = $state(null);
  let range = $state('live');
  let history = $state([]);
  let error = $state('');
  let procs = $state(null);
  let procTotal = $state(0);
  let procSort = $state('cpu');
  let preview = $state('');

  const GUIDE_LINES = 6;

  async function loadGuide() {
    try {
      const g = await get('/guide');
      const lines = g.text.split('\n').slice(0, GUIDE_LINES);
      // A heading cut off from its text would just dangle at the end of the preview.
      while (lines.length && /^\s*(#{1,6}\s.*)?$/.test(lines.at(-1))) lines.pop();
      const shown = lines.join('\n');
      if (!shown.trim()) return;
      const { renderMarkdown } = await import('../lib/markdown.js');
      preview = renderMarkdown(shown);
    } catch {
      // The preview is a convenience; the rest of the dashboard doesn't depend on it.
    }
  }

  // The next request is scheduled after the last returns, and nothing runs while the tab is
  // hidden, so an open dashboard costs little on a busy server.
  let stopped = false;
  async function pollProcs() {
    if (stopped) return;
    if (!document.hidden) {
      try {
        const res = await get('/monitor/top?sort=' + procSort);
        procs = res.processes;
        procTotal = res.total;
      } catch {
        // Connection problems are already reported by the figures above.
      }
    }
    setTimeout(pollProcs, 3000);
  }

  function sortProcs(by) {
    procSort = by;
    if (procs) {
      procs = [...procs].sort((a, b) => (by === 'cpu' ? b.cpu - a.cpu : b.memory - a.memory));
    }
  }

  function highlight(percent, warn, bad) {
    if (percent >= bad) return 'bad';
    if (percent >= warn) return 'warn';
    return '';
  }

  const samples = $derived(range === 'live' ? (now?.live ?? []) : history);
  const latest = $derived(now?.live?.[now.live.length - 1]);
  const times = $derived(samples.map((s) => s.ts));
  const pct = (v) => `${Math.round(v)}%`;
  const rate = (v) => `${formatBytes(v)}/s`;

  async function refresh() {
    try {
      now = await get('/monitor/current');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }

  async function loadHistory() {
    if (range === 'live') return;
    try {
      history = await get('/monitor/history?range=' + range);
    } catch (e) {
      error = e.message;
    }
  }

  $effect(() => {
    range;
    loadHistory();
  });

  onMount(() => {
    refresh();
    pollProcs();
    if (ui.guideDashboard) loadGuide();
    const fast = setInterval(refresh, 2000);
    const slow = setInterval(loadHistory, 60000);
    return () => {
      clearInterval(fast);
      clearInterval(slow);
      stopped = true;
    };
  });

  function level(v) {
    if (v >= 90) return { cls: 'bad', text: '▲ Critical' };
    if (v >= 75) return { cls: 'warn', text: '▲ High' };
    return null;
  }
</script>

{#snippet tile(label, value, sub, percent = null)}
  {@const lvl = percent === null ? null : level(percent)}
  <div class="card tile">
    <div class="spread">
      <div class="label">{label}</div>
      {#if lvl}<span class="badge {lvl.cls}">{lvl.text}</span>{/if}
    </div>
    <div class="value">{value}</div>
    <div class="sub">{sub}</div>
  </div>
{/snippet}

<div class="page">
  <div class="page-head">
    <div>
      <h1>{now?.hostname ?? 'Dashboard'}</h1>
      {#if now}<div class="muted small">{now.os} · kernel {now.kernel} · up {formatDuration(now.uptime ?? 0)}</div>{/if}
    </div>
    <div class="row" role="group" aria-label="Time range">
      {#each [['live', 'Live'], ['1h', '1 hour'], ['24h', '24 hours'], ['7d', '7 days']] as [id, label]}
        <button class:primary={range === id} aria-pressed={range === id} onclick={() => (range = id)}>{label}</button>
      {/each}
    </div>
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  {#if ui.guideDashboard && preview}
    <div class="card card-pad">
      <div class="guide-preview markdown">{@html preview}</div>
      <a class="small" href="#/guide">Open the guide</a>
    </div>
  {/if}

  <div class="tiles">
    {@render tile('CPU', latest ? pct(latest.cpu) : '-', `${now?.cores ?? '-'} cores · load ${now?.load?.[0]?.toFixed(2) ?? '-'}`, latest?.cpu ?? null)}
    {@render tile('Memory', latest ? pct(latest.mem) : '-', `${formatBytes(now?.memUsed)} of ${formatBytes(now?.memTotal)}`, latest?.mem ?? null)}
    {@render tile('Disk', latest ? pct(latest.disk) : '-', `${formatBytes(now?.diskUsed)} of ${formatBytes(now?.diskTotal)}`, latest?.disk ?? null)}
    {@render tile('Swap', latest ? pct(latest.swap) : '-', `${formatBytes(now?.swapUsed)} of ${formatBytes(now?.swapTotal)}`)}
    {@render tile('Network in', latest ? rate(latest.netRx) : '-', `out ${latest ? rate(latest.netTx) : '-'}`)}
    {@render tile('Disk read', latest ? rate(latest.diskRead) : '-', `write ${latest ? rate(latest.diskWrite) : '-'}`)}
  </div>

  <div class="charts">
    <Chart title="CPU usage" {times} max={100} format={pct} series={[{ name: 'CPU', color: 'var(--series-1)', values: samples.map((s) => s.cpu) }]} />
    <Chart title="Memory usage" {times} max={100} format={pct} series={[{ name: 'Memory', color: 'var(--series-1)', values: samples.map((s) => s.mem) }]} />
    <Chart title="Network traffic" {times} format={rate} series={[
      { name: 'In', color: 'var(--series-1)', values: samples.map((s) => s.netRx) },
      { name: 'Out', color: 'var(--series-2)', values: samples.map((s) => s.netTx) },
    ]} />
    <Chart title="Disk activity" {times} format={rate} series={[
      { name: 'Read', color: 'var(--series-1)', values: samples.map((s) => s.diskRead) },
      { name: 'Write', color: 'var(--series-2)', values: samples.map((s) => s.diskWrite) },
    ]} />
  </div>

  <div class="card">
    <div class="card-head">
      <h2>Top processes</h2>
      <span class="small muted">{procs ? `Busiest ${procs.length} of ${procTotal}, updated every few seconds` : 'Measuring...'}</span>
    </div>
    {#if procs}
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>PID</th>
              <th>User</th>
              <th aria-sort={procSort === 'cpu' ? 'descending' : 'none'}><button class="link" onclick={() => sortProcs('cpu')}>CPU{procSort === 'cpu' ? ' ↓' : ''}</button></th>
              <th aria-sort={procSort === 'memory' ? 'descending' : 'none'}><button class="link" onclick={() => sortProcs('memory')}>Memory{procSort === 'memory' ? ' ↓' : ''}</button></th>
              <th>Container</th>
              <th>Command</th>
            </tr>
          </thead>
          <tbody>
            {#each procs as p (p.pid)}
              {@const cl = highlight(p.cpu, 50, 80)}
              {@const ml = highlight(p.memoryPct, 25, 50)}
              <tr>
                <td class="small muted">{p.pid}</td>
                <td class="small nowrap">{p.user}</td>
                <td class="small nowrap stat {cl}">{cl ? '▲ ' : ''}{p.cpu.toFixed(1)}%</td>
                <td class="small nowrap stat {ml}">{ml ? '▲ ' : ''}{formatBytes(p.memory)} <span class="hint">{p.memoryPct.toFixed(1)}%</span></td>
                <td class="small nowrap">{p.container}</td>
                <td class="mono small muted command" title={p.command}>{p.command}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <div class="hint" style="padding:8px 14px">CPU is a share of the whole server, so one busy core on this 2-core machine shows as 50%.</div>
    {/if}
  </div>
</div>
