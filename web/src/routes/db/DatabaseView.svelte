<script>
  import { onMount } from 'svelte';
  import { get, post, toast, followTask, confirmDialog, formatBytes } from '../../lib/api.svelte.js';
  import { uploadForImport } from '../../lib/upload.js';
  import Modal from '../../lib/Modal.svelte';
  import Editor from '../../lib/Editor.svelte';
  import TableView from './TableView.svelte';
  import ColumnFields from './ColumnFields.svelte';
  import { formToColumn, newColumn } from './schema.js';

  let { inst, name, system = false, onback } = $props();

  const base = $derived(`/db/${encodeURIComponent(inst)}/databases/${encodeURIComponent(name)}`);
  let view = $state('tables');
  let tables = $state([]);
  let filter = $state('');
  let open = $state(null); // { table, search }
  let creating = $state(null);

  // SQL console
  let sql = $state('SELECT 1;');
  let result = $state(null);
  let sqlError = $state('');
  let running = $state(false);

  // Search
  let term = $state('');
  let found = $state(null);
  let searching = $state(false);

  // Export and import
  let scope = $state('all');
  let chosen = $state({});
  let content = $state('full');
  let compress = $state(true);
  let fileInput = $state();
  let importing = $state(null);
  let serverPath = $state('');

  const visible = $derived(tables.filter((t) => t.name.toLowerCase().includes(filter.toLowerCase())));
  const chosenTables = $derived(tables.filter((t) => chosen[t.name] && t.type !== 'VIEW').map((t) => t.name));
  const exportURL = $derived.by(() => {
    const q = new URLSearchParams({ content, gzip: compress ? '1' : '0' });
    if (scope === 'some') q.set('tables', chosenTables.join(','));
    return `/api${base}/export?${q}`;
  });

  async function loadTables() {
    try {
      tables = await get(`${base}/tables`);
    } catch (e) {
      toast(e.message, true);
    }
  }
  onMount(loadTables);

  async function runSQL() {
    running = true;
    sqlError = '';
    result = null;
    try {
      result = await post(`${base}/query`, { sql });
    } catch (e) {
      sqlError = e.message;
    }
    running = false;
  }

  async function runSearch(e) {
    e.preventDefault();
    if (!term.trim()) return;
    searching = true;
    found = null;
    try {
      found = await post(`${base}/search`, { term: term.trim() });
    } catch (err) {
      toast(err.message, true);
    }
    searching = false;
  }

  function startCreate() {
    creating = {
      name: '', engine: 'InnoDB', collation: 'utf8mb4_unicode_ci', comment: '', error: '',
      columns: [newColumn({ name: 'id', base: 'int', length: '', nullable: false, autoIncrement: true, primary: true, unsigned: true })],
    };
  }

  async function submitCreate() {
    const c = creating;
    c.error = '';
    const body = { name: c.name.trim(), engine: c.engine, collation: c.collation, comment: c.comment, columns: c.columns.map(formToColumn) };
    try {
      const preview = await post(`${base}/tables`, { ...body, dry: true });
      const ok = await confirmDialog({ title: 'Create this table?', message: preview.sql, action: 'Create', mono: true });
      if (!ok) return;
      await post(`${base}/tables`, body);
      toast('Table created');
      creating = null;
      await loadTables();
      open = { table: body.name, search: '' };
    } catch (e) {
      c.error = e.message;
    }
  }

  async function importFile(e) {
    const file = e.target.files[0];
    e.target.value = '';
    if (!file) return;
    const ok = await confirmDialog({
      title: `Import into ${name}?`,
      message: `${file.name} (${formatBytes(file.size)}) runs against this database as root. Statements in the file can overwrite data, and a dump made with --databases can switch to another database.`,
      action: 'Import',
      danger: true,
    });
    if (!ok) return;
    try {
      importing = { name: file.name, done: 0, total: file.size };
      const path = await uploadForImport(file, (done) => (importing.done = done));
      importing = null;
      const res = await post(`${base}/import`, { path });
      followTask(res.task, loadTables);
    } catch (err) {
      importing = null;
      toast(err.message, true);
    }
  }

  async function importServerFile() {
    const path = serverPath.trim();
    if (!path) return;
    const ok = await confirmDialog({ title: `Import into ${name}?`, message: `${path} runs against this database as root. Statements in the file can overwrite data.`, action: 'Import', danger: true });
    if (!ok) return;
    try {
      const res = await post(`${base}/import`, { path });
      followTask(res.task, loadTables);
    } catch (err) {
      toast(err.message, true);
    }
  }

  const clip = (v) => (v === null ? 'NULL' : v.length > 120 ? v.slice(0, 120) + '…' : v);
</script>

<div class="page-head">
  <h1><button class="link" style="font-size:20px" onclick={onback}>Databases</button> / {name}</h1>
  {#if system}<span class="badge">system database, read only here</span>{/if}
</div>

{#if open}
  {#key open.table}
    <TableView {base} db={name} table={open.table} initialSearch={open.search} readonly={system} onback={() => { open = null; loadTables(); }} onchanged={loadTables} />
  {/key}
{:else}
  <div class="tabs">
    <button class:on={view === 'tables'} onclick={() => (view = 'tables')}>Tables</button>
    <button class:on={view === 'search'} onclick={() => (view = 'search')}>Search</button>
    <button class:on={view === 'sql'} onclick={() => (view = 'sql')}>SQL console</button>
    <button class:on={view === 'transfer'} onclick={() => (view = 'transfer')}>Import / export</button>
  </div>

  {#if view === 'tables'}
    <div class="card">
      <div class="card-head">
        <input type="search" placeholder="Filter tables" bind:value={filter} aria-label="Filter tables" />
        <div class="row">
          <span class="small muted">{tables.length} tables</span>
          {#if !system}<button class="primary" onclick={startCreate}>New table</button>{/if}
        </div>
      </div>
      {#if !visible.length}
        <div class="empty">{tables.length ? 'No matches.' : 'No tables yet.'}</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Table</th><th>Rows (approx.)</th><th>Size</th><th>Engine</th></tr></thead>
            <tbody>
              {#each visible as t (t.name)}
                <tr>
                  <td><button class="link" onclick={() => (open = { table: t.name, search: '' })}>{t.name}</button>{#if t.type === 'VIEW'} <span class="badge">view</span>{/if}</td>
                  <td class="small">{t.rows}</td>
                  <td class="small nowrap">{formatBytes(t.size)}</td>
                  <td class="small muted">{t.engine}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>
  {:else if view === 'search'}
    <div class="card card-pad stack">
      <form class="row" onsubmit={runSearch}>
        <input type="search" placeholder="Find text anywhere in this database" bind:value={term} aria-label="Search this database" style="width:320px;max-width:100%" />
        <button class="primary" disabled={searching}>{searching ? 'Searching...' : 'Search'}</button>
      </form>
      <div class="hint">Looks in every text and number column of every table. Binary columns are skipped. Large databases can take up to a minute.</div>
      {#if found}
        {#if found.incomplete}<div class="notice">▲ Stopped after a minute with {found.searched} of {found.tables} tables searched, so there may be more matches.</div>{/if}
        {#if found.matches.length}
          <div class="table-wrap">
            <table>
              <thead><tr><th>Table</th><th>Matching rows</th></tr></thead>
              <tbody>
                {#each found.matches as m (m.table)}
                  <tr>
                    <td><button class="link" onclick={() => (open = { table: m.table, search: term.trim() })}>{m.table}</button></td>
                    <td class="small">{m.count}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {:else}
          <div class="empty">No matches in the {found.searched} tables searched.</div>
        {/if}
      {/if}
    </div>
  {:else if view === 'sql'}
    <div class="card card-pad stack">
      <div class="sql-box"><Editor bind:value={sql} filename="q.sql" /></div>
      <div class="row">
        <button class="primary" disabled={running} onclick={runSQL}>{running ? 'Running...' : 'Run'}</button>
        <span class="hint">Runs one statement as root in {name}. Every statement goes in the audit log.</span>
      </div>
      {#if sqlError}<div class="error">{sqlError}</div>{/if}
      {#if result}
        <div class="hint">{result.rows.length} rows in {result.ms} ms{result.truncated ? ' (showing the first 1000)' : ''}</div>
        {#if result.columns.length}
          <div class="table-wrap">
            <table class="grid">
              <thead><tr>{#each result.columns as c}<th>{c}</th>{/each}</tr></thead>
              <tbody>
                {#each result.rows as row}
                  <tr>{#each row as v}<td class="small value" class:muted={v === null} title={v ?? 'NULL'}>{clip(v)}</td>{/each}</tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
      {/if}
    </div>
  {:else}
    <div class="charts">
      <div class="card card-pad form">
        <h2>Export</h2>
        <label class="field">What to export
          <select bind:value={scope}>
            <option value="all">The whole database</option>
            <option value="some">Chosen tables</option>
          </select>
        </label>
        {#if scope === 'some'}
          <div class="stack small" style="gap:4px;max-height:220px;overflow:auto;border:1px solid var(--border);border-radius:7px;padding:8px">
            {#each tables.filter((t) => t.type !== 'VIEW') as t (t.name)}
              <label class="row"><input type="checkbox" bind:checked={chosen[t.name]} /> <span class="mono">{t.name}</span></label>
            {/each}
          </div>
        {/if}
        <label class="field">Include
          <select bind:value={content}>
            <option value="full">Structure and data</option>
            <option value="structure">Structure only</option>
            <option value="data">Data only</option>
          </select>
        </label>
        <label class="row small"><input type="checkbox" bind:checked={compress} /> Compress (.sql.gz)</label>
        <div>
          {#if scope === 'some' && !chosenTables.length}
            <button disabled>Choose at least one table</button>
          {:else}
            <a class="btn" href={exportURL}>Download export</a>
          {/if}
        </div>
      </div>

      <div class="card card-pad form">
        <h2>Import</h2>
        {#if system}
          <div class="hint">System databases can't be imported into from here.</div>
        {:else}
          <div class="hint">Runs a .sql or .sql.gz file. Big files are fine: they upload in pieces and import in the background.</div>
          <div>
            <button class="primary" disabled={!!importing} onclick={() => fileInput.click()}>
              {importing ? `Uploading ${formatBytes(importing.done)} of ${formatBytes(importing.total)}` : 'Choose file from this computer'}
            </button>
            <input bind:this={fileInput} type="file" accept=".sql,.gz" hidden onchange={importFile} />
          </div>
          <label class="field">Or a file already on the server
            <input class="mono" bind:value={serverPath} placeholder="/srv/backup.sql.gz" />
          </label>
          <div><button disabled={!serverPath.trim()} onclick={importServerFile}>Import from server</button></div>
          <div class="hint">To import a CSV, open a table and use its Import / export tab.</div>
        {/if}
      </div>
    </div>
  {/if}
{/if}

{#if creating}
  <Modal title="New table in {name}" wide onclose={() => (creating = null)}>
    <form class="form" id="table-form" onsubmit={(e) => { e.preventDefault(); submitCreate(); }}>
      <div class="form-grid">
        <label class="field">Table name <input class="mono" bind:value={creating.name} required autocomplete="off" /></label>
        <label class="field">Engine
          <select bind:value={creating.engine}>
            <option>InnoDB</option>
            <option>Aria</option>
            <option>MyISAM</option>
          </select>
        </label>
        <label class="field">Collation
          <select bind:value={creating.collation}>
            <option>utf8mb4_unicode_ci</option>
            <option>utf8mb4_general_ci</option>
            <option>utf8mb4_bin</option>
          </select>
        </label>
        <label class="field">Comment (optional) <input bind:value={creating.comment} /></label>
      </div>
      {#each creating.columns as _, i}
        <div class="card" style="padding:14px">
          <div class="spread small" style="margin-bottom:8px">
            <b>Column {i + 1}</b>
            {#if creating.columns.length > 1}
              <button type="button" class="danger" onclick={() => creating.columns.splice(i, 1)}>Remove</button>
            {/if}
          </div>
          <ColumnFields bind:column={creating.columns[i]} showPrimary />
        </div>
      {/each}
      <div><button type="button" onclick={() => creating.columns.push(newColumn())}>Add another column</button></div>
      {#if creating.error}<div class="error">{creating.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (creating = null)}>Cancel</button>
      <button class="primary" type="submit" form="table-form">Review and create</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .sql-box { display: flex; height: 220px; min-height: 120px; resize: vertical; overflow: hidden; }
</style>
