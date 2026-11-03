<script>
  // Log window shared by websites, containers and stacks. While live it refreshes on a
  // timer and follows new lines, unless the reader has scrolled up to look at something.
  import { tick, untrack } from 'svelte';
  import { attempt, toast, confirmDialog, formatBytes } from './api.svelte.js';
  import Modal from './Modal.svelte';

  // load(lines) resolves to { text, size }. size may be null. vacuum(keepMB) resolves to
  // { freed, size }; leave it out where a log can't be trimmed. vacuumNote is added to the
  // confirmation, for side effects. key reloads when it changes.
  let { title, load, vacuum = null, vacuumNote = '', onclose, key = '', controls = null } = $props();

  const INTERVAL = 3000;
  let text = $state('');
  let size = $state(null);
  let error = $state('');
  let live = $state(true);
  let lines = $state(500);
  let keepMB = $state(100);
  let box = $state();
  let inflight = false;

  async function refresh() {
    if (inflight) return;
    inflight = true;
    try {
      const follow = !box || box.scrollHeight - box.scrollTop - box.clientHeight < 40;
      const res = await load(lines);
      text = res.text;
      size = res.size ?? null;
      error = '';
      if (follow) {
        await tick();
        if (box) box.scrollTop = box.scrollHeight;
      }
    } catch (e) {
      error = e.message;
    }
    inflight = false;
  }

  $effect(() => {
    key;
    lines;
    untrack(refresh);
  });

  $effect(() => {
    if (!live) return;
    const timer = setInterval(() => {
      if (!document.hidden) refresh();
    }, INTERVAL);
    return () => clearInterval(timer);
  });

  async function runVacuum() {
    const mb = Number(keepMB);
    if (!(mb >= 0)) {
      toast('Enter a size in MB', true);
      return;
    }
    const ok = await confirmDialog({
      title: 'Vacuum this log?',
      message: (mb === 0 ? 'All of this log will be deleted.' : `Older entries will be deleted, keeping the newest ${mb} MB.`) + (vacuumNote ? `\n\n${vacuumNote}` : ''),
      action: 'Vacuum',
      danger: true,
    });
    if (!ok) return;
    const res = await attempt(() => vacuum(mb));
    if (res) {
      toast(`Freed ${formatBytes(res.freed)}, log is now ${formatBytes(res.size)}`);
      refresh();
    }
  }
</script>

<Modal {title} wide {onclose}>
  <div class="spread">
    <div class="row">
      {@render controls?.()}
      <label class="row small">Lines
        <select bind:value={lines} aria-label="Lines to show">
          {#each [200, 500, 1000, 5000] as n}<option value={n}>{n}</option>{/each}
        </select>
      </label>
      <label class="row small" title="Refreshes every 3 seconds. Turn off to hold the view still.">
        <input type="checkbox" bind:checked={live} /> Auto-refresh
      </label>
      {#if live}<span class="badge good">● Live</span>{:else}<span class="badge">■ Locked</span>{/if}
      <button onclick={refresh} disabled={live}>Refresh</button>
    </div>
    <div class="row small">
      {#if size !== null}<span>Log size <b>{formatBytes(size)}</b></span>{/if}
      {#if vacuum}
        <label class="row">Keep
          <input type="number" min="0" bind:value={keepMB} style="width:76px" aria-label="Megabytes to keep" /> MB
        </label>
        <button class="danger" onclick={runVacuum}>Vacuum</button>
      {/if}
    </div>
  </div>
  {#if error}<div class="error">{error}</div>{/if}
  <pre class="log" style="flex:1" bind:this={box}>{text || 'No output.'}</pre>
</Modal>
