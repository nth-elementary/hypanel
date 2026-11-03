<script>
  import { onMount } from 'svelte';
  import { get, post, attempt, toast, followTask, confirmDialog } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';
  import Editor from '../lib/Editor.svelte';
  import LogViewer from '../lib/LogViewer.svelte';

  let stacks = $state([]);
  let error = $state('');
  let editing = $state(null);
  let logs = $state(null);
  let creating = $state(null);

  const template = `services:
  app:
    image: nginx:alpine
    restart: unless-stopped
    ports:
      - 127.0.0.1:8080:80
`;

  async function load() {
    try {
      stacks = await get('/stacks');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  async function action(stack, act) {
    if (act === 'down') {
      const ok = await confirmDialog({ title: `Stop and remove ${stack.name}'s containers?`, message: 'Files and data directories stay on disk.', action: 'Down', danger: true });
      if (!ok) return;
    }
    const res = await attempt(() => post(`/stacks/${encodeURIComponent(stack.name)}/${act}`));
    if (res) followTask(res.task, () => load());
  }

  async function edit(stack) {
    const res = await attempt(() => get(`/stacks/${encodeURIComponent(stack.name)}/files`));
    if (!res) return;
    const names = Object.keys(res.files);
    editing = { name: stack.name, path: res.path, files: res.files, file: names[0] ?? 'docker-compose.yml', error: '', key: 0 };
  }

  function addOverride() {
    editing.files['docker-compose.override.yml'] = 'services:\n';
    editing.file = 'docker-compose.override.yml';
  }

  async function saveFile() {
    editing.error = '';
    try {
      await post(`/stacks/${encodeURIComponent(editing.name)}/files`, { file: editing.file, content: editing.files[editing.file] });
      toast('Saved. Use Up to apply changes.');
    } catch (e) {
      editing.error = e.message;
    }
  }

  async function loadLog(lines) {
    const res = await get(`/stacks/${encodeURIComponent(logs.title)}/logs?lines=${lines}`);
    return { text: res.logs, size: null };
  }

  async function create() {
    creating.error = '';
    try {
      await post('/stacks', { name: creating.name, compose: creating.compose });
      toast('Stack created. Use Up to start it.');
      creating = null;
      load();
    } catch (e) {
      creating.error = e.message;
    }
  }

  const badge = { running: ['good', '● Running'], partial: ['warn', '▲ Partial'], stopped: ['', '○ Stopped'] };
</script>

<div class="page">
  <div class="page-head">
    <h1>Compose stacks</h1>
    <button class="primary" onclick={() => (creating = { name: '', compose: template, error: '' })}>New stack</button>
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  <div class="card">
    {#if !stacks.length}
      <div class="empty">No stacks in /opt/stacks.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Stack</th><th>Status</th><th>Containers</th><th></th></tr></thead>
          <tbody>
            {#each stacks as s (s.name)}
              <tr>
                <td class="nowrap">{s.name}<div class="hint mono">{s.path}</div></td>
                <td><span class="badge {badge[s.status][0]}">{badge[s.status][1]}</span></td>
                <td class="small">
                  {#each s.containers as c}<div class="nowrap">{c.name} <span class="muted">{c.status}</span></div>{/each}
                </td>
                <td class="actions">
                  <button onclick={() => edit(s)}>Edit</button>
                  <button onclick={() => (logs = { title: s.name })}>Logs</button>
                  <button onclick={() => action(s, 'pull')}>Pull</button>
                  <button onclick={() => action(s, 'up')}>Up</button>
                  <button onclick={() => action(s, 'restart')}>Restart</button>
                  <button class="danger" onclick={() => action(s, 'down')}>Down</button>
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
  <Modal title={editing.path} wide onclose={() => (editing = null)}>
    <div class="tabs">
      {#each Object.keys(editing.files) as f}
        <button class:on={editing.file === f} onclick={() => (editing.file = f)}>{f}</button>
      {/each}
      {#if !editing.files['docker-compose.override.yml']}<button onclick={addOverride}>+ override</button>{/if}
    </div>
    {#key editing.file}
      <Editor bind:value={editing.files[editing.file]} filename={editing.file} />
    {/key}
    {#if editing.error}<div class="error">{editing.error}</div>{/if}
    {#snippet foot()}
      <span class="hint grow">Checked with docker compose config; a rejected file isn't saved.</span>
      <button onclick={() => (editing = null)}>Close</button>
      <button class="primary" onclick={saveFile}>Save {editing.file}</button>
    {/snippet}
  </Modal>
{/if}

{#if creating}
  <Modal title="New stack" wide onclose={() => (creating = null)}>
    <label class="field">Name <input bind:value={creating.name} placeholder="my-app" /></label>
    <Editor bind:value={creating.compose} filename="docker-compose.yml" />
    {#if creating.error}<div class="error">{creating.error}</div>{/if}
    {#snippet foot()}
      <button onclick={() => (creating = null)}>Cancel</button>
      <button class="primary" onclick={create}>Create</button>
    {/snippet}
  </Modal>
{/if}

{#if logs}
  <LogViewer title="Logs: {logs.title}" load={loadLog} onclose={() => (logs = null)} />
{/if}
