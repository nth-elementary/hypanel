<script>
  import { onMount } from 'svelte';
  import { get, post, del, attempt, followTask, confirmDialog } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';

  let jobs = $state([]);
  let error = $state('');
  let editing = $state(null);

  async function load() {
    try {
      jobs = await get('/cron');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  async function save() {
    editing.error = '';
    try {
      await post('/cron', { id: editing.id, name: editing.name, spec: editing.spec, command: editing.command });
      editing = null;
      load();
    } catch (e) {
      editing.error = e.message;
    }
  }

  async function run(j) {
    const res = await attempt(() => post(`/cron/${j.id}/run`));
    if (res) followTask(res.task);
  }

  async function toggle(j) {
    if (await attempt(() => post(`/cron/${j.id}/${j.enabled ? 'disable' : 'enable'}`))) load();
  }

  async function remove(j) {
    const ok = await confirmDialog({ title: `Delete "${j.name}"?`, action: 'Delete', danger: true });
    if (ok && (await attempt(() => del('/cron/' + j.id), 'Job deleted'))) load();
  }

  const presets = [
    ['Every 5 minutes', '*/5 * * * *'],
    ['Hourly', '0 * * * *'],
    ['Daily at 03:30', '30 3 * * *'],
    ['Weekly, Monday 04:00', '0 4 * * 1'],
  ];
</script>

<div class="page">
  <div class="page-head">
    <h1>Scheduled jobs</h1>
    <button class="primary" onclick={() => (editing = { id: '', name: '', spec: '30 3 * * *', command: '', error: '' })}>New job</button>
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  <div class="card">
    {#if !jobs.length}
      <div class="empty">No scheduled jobs.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Name</th><th>Schedule</th><th>Command</th><th>Status</th><th></th></tr></thead>
          <tbody>
            {#each jobs as j (j.id)}
              <tr>
                <td>{j.name}</td>
                <td class="mono nowrap">{j.spec}</td>
                <td class="mono muted break">{j.command}</td>
                <td>{#if j.enabled}<span class="badge good">● On</span>{:else}<span class="badge">○ Off</span>{/if}</td>
                <td class="actions">
                  <button onclick={() => run(j)}>Run now</button>
                  <button onclick={() => (editing = { ...j, error: '' })}>Edit</button>
                  <button onclick={() => toggle(j)}>{j.enabled ? 'Turn off' : 'Turn on'}</button>
                  <button class="danger" onclick={() => remove(j)}>Delete</button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>

{#if editing}
  <Modal title={editing.id ? 'Edit job' : 'New job'} onclose={() => (editing = null)}>
    <form class="form" id="job" onsubmit={(e) => { e.preventDefault(); save(); }}>
      <label class="field">Name <input bind:value={editing.name} required /></label>
      <label class="field">Schedule (minute hour day month weekday)
        <input class="mono" bind:value={editing.spec} required />
      </label>
      <div class="row">
        {#each presets as [label, spec]}<button type="button" onclick={() => (editing.spec = spec)}>{label}</button>{/each}
      </div>
      <label class="field">Command
        <input class="mono" bind:value={editing.command} placeholder="docker exec mycontainer php job.php" required />
        <span class="hint">Runs as root on this server. Server time zone applies.</span>
      </label>
      {#if editing.error}<div class="error">{editing.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (editing = null)}>Cancel</button>
      <button class="primary" type="submit" form="job">Save</button>
    {/snippet}
  </Modal>
{/if}
