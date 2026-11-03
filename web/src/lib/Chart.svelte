<script>
  // A line chart on one y-axis. Thin lines, hairline grid, crosshair tooltip, and a data
  // table so nothing depends on seeing the lines. Each measure gets its own chart.
  let { title, times = [], series = [], max = null, format = (v) => v } = $props();

  const pad = { top: 10, right: 12, bottom: 22, left: 58 };
  const height = 170;
  let width = $state(400);
  let hover = $state(null);

  const innerW = $derived(Math.max(10, width - pad.left - pad.right));
  const innerH = height - pad.top - pad.bottom;

  // Rounds up to a tidy ceiling, so byte-rate axes read 1 MB rather than 973 KB.
  function niceCeil(v) {
    if (v <= 0) return 1;
    const base = v >= 1024 ? 1024 ** Math.floor(Math.log(v) / Math.log(1024)) : 1;
    const scaled = v / base;
    const step = [1, 2, 5, 10, 20, 50, 100, 200, 500, 1024].find((s) => s >= scaled) ?? scaled;
    return step * base;
  }

  const yMax = $derived.by(() => {
    if (max !== null) return max;
    let top = 0;
    for (const s of series) for (const v of s.values) if (v > top) top = v;
    return niceCeil(top);
  });
  const t0 = $derived(times[0] ?? 0);
  const t1 = $derived(times[times.length - 1] ?? 1);
  const x = (t) => pad.left + ((t - t0) / Math.max(1, t1 - t0)) * innerW;
  const y = (v) => pad.top + innerH - (Math.min(v, yMax) / yMax) * innerH;

  function path(values) {
    return values.map((v, i) => `${i ? 'L' : 'M'}${x(times[i]).toFixed(1)} ${y(v).toFixed(1)}`).join('');
  }

  const ticks = $derived([0, 0.5, 1].map((f) => f * yMax));

  function clock(ts) {
    const d = new Date(ts * 1000);
    if (t1 - t0 > 2 * 86400) return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
    return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  }

  function move(e) {
    if (times.length < 2) return;
    const box = e.currentTarget.getBoundingClientRect();
    const target = t0 + ((e.clientX - box.left - pad.left) / innerW) * (t1 - t0);
    let best = 0;
    for (let i = 1; i < times.length; i++) {
      if (Math.abs(times[i] - target) < Math.abs(times[best] - target)) best = i;
    }
    hover = best;
  }

  const recent = $derived(times.map((t, i) => i).slice(-12).reverse());
</script>

<div class="card card-pad">
  <div class="spread" style="margin-bottom: 8px">
    <h2>{title}</h2>
    {#if series.length > 1}
      <div class="row small">
        {#each series as s}
          <span class="row" style="gap:5px"><i style="width:10px;height:2px;border-radius:2px;background:{s.color}"></i>{s.name}</span>
        {/each}
      </div>
    {/if}
  </div>

  <div style="position:relative" bind:clientWidth={width}>
    <!-- svelte-ignore a11y_no_static_element_interactions -->
    <svg {width} {height} role="img" aria-label={title} onmousemove={move} onmouseleave={() => (hover = null)} style="display:block">
      {#each ticks as tick}
        <line x1={pad.left} x2={pad.left + innerW} y1={y(tick)} y2={y(tick)} stroke="var(--grid)" stroke-width="1" />
        <text x={pad.left - 8} y={y(tick) + 4} text-anchor="end" font-size="11" fill="var(--text-3)">{format(tick)}</text>
      {/each}
      {#if times.length > 1}
        <text x={pad.left} y={height - 6} font-size="11" fill="var(--text-3)">{clock(t0)}</text>
        <text x={pad.left + innerW} y={height - 6} text-anchor="end" font-size="11" fill="var(--text-3)">{clock(t1)}</text>
        {#each series as s}
          <path d={path(s.values)} fill="none" stroke={s.color} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
        {/each}
        {#if hover !== null}
          <line x1={x(times[hover])} x2={x(times[hover])} y1={pad.top} y2={pad.top + innerH} stroke="var(--text-3)" stroke-width="1" />
          {#each series as s}
            <circle cx={x(times[hover])} cy={y(s.values[hover])} r="4" fill={s.color} stroke="var(--surface)" stroke-width="2" />
          {/each}
        {/if}
      {:else}
        <text x={pad.left + innerW / 2} y={height / 2} text-anchor="middle" font-size="12" fill="var(--text-3)">Collecting data</text>
      {/if}
    </svg>
    {#if hover !== null}
      {@const left = x(times[hover])}
      <div class="small" style="position:absolute;top:8px;{left > width / 2 ? `right:${width - left + 10}px` : `left:${left + 10}px`};background:var(--surface);border:1px solid var(--border);border-radius:6px;padding:5px 9px;pointer-events:none;white-space:nowrap;box-shadow:0 2px 8px rgb(0 0 0 / .12)">
        <div class="muted">{new Date(times[hover] * 1000).toLocaleString()}</div>
        {#each series as s}
          <div class="row" style="gap:6px;justify-content:space-between">
            <span class="row" style="gap:5px"><i style="width:8px;height:8px;border-radius:50%;background:{s.color}"></i>{s.name}</span>
            <b>{format(s.values[hover])}</b>
          </div>
        {/each}
      </div>
    {/if}
  </div>

  <details class="small muted" style="margin-top:6px">
    <summary style="cursor:pointer">Show recent values</summary>
    <div class="table-wrap">
      <table>
        <thead><tr><th>Time</th>{#each series as s}<th>{s.name}</th>{/each}</tr></thead>
        <tbody>
          {#each recent as i}
            <tr><td>{new Date(times[i] * 1000).toLocaleTimeString()}</td>{#each series as s}<td>{format(s.values[i])}</td>{/each}</tr>
          {/each}
        </tbody>
      </table>
    </div>
  </details>
</div>
