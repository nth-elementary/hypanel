<script>
  import { onMount } from 'svelte';
  import { get, post, del, attempt, toast, confirmDialog, promptDialog, formatBytes } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';
  import DatabaseView from './db/DatabaseView.svelte';

  let instances = $state([]);
  let inst = $state('');
  let error = $state('');
  let tab = $state('databases');
  let databases = $state([]);
  let users = $state([]);
  let current = $state(null); // the open database
  let filter = $state('');
  let creating = $state(null);
  let userForm = $state(null);

  const base = $derived('/db/' + encodeURIComponent(inst));
  const visible = $derived(databases.filter((d) => d.name.toLowerCase().includes(filter.toLowerCase())));
  const currentInfo = $derived(databases.find((d) => d.name === current));

  async function loadInstances() {
    try {
      instances = await get('/db/instances');
      if (instances.length) {
        inst = instances[0].name;
        loadDatabases();
      }
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(loadInstances);

  async function loadDatabases() {
    try {
      databases = await get(base + '/databases');
      error = '';
    } catch (e) {
      error = e.message;
    }
  }

  async function loadUsers() {
    try {
      users = await get(base + '/users');
    } catch (e) {
      error = e.message;
    }
  }

  function switchInstance() {
    current = null;
    loadDatabases();
    if (tab === 'users') loadUsers();
  }

  function setTab(next) {
    tab = next;
    if (next === 'users') loadUsers();
  }

  async function createDatabase() {
    creating.error = '';
    try {
      await post(base + '/databases', { name: creating.name, user: creating.user, password: creating.password });
      toast('Database created');
      creating = null;
      loadDatabases();
    } catch (e) {
      creating.error = e.message;
    }
  }

  function randomPassword() {
    const bytes = crypto.getRandomValues(new Uint8Array(18));
    return btoa(String.fromCharCode(...bytes)).replace(/[+/=]/g, '');
  }

  async function dropDatabase(d) {
    const ok = await confirmDialog({
      title: `Drop database ${d.name}?`,
      message: `This permanently deletes ${d.tables} tables. Export it first if you may need it.`,
      action: 'Drop',
      danger: true,
      typed: d.name,
    });
    if (ok && (await attempt(() => del(`${base}/databases/${encodeURIComponent(d.name)}?confirm=${encodeURIComponent(d.name)}`), 'Database dropped'))) loadDatabases();
  }

  async function createUser() {
    userForm.error = '';
    try {
      await post(base + '/users', { user: userForm.user, host: userForm.host, password: userForm.password });
      if (userForm.database) await post(base + '/users/grant', { user: userForm.user, host: userForm.host, database: userForm.database });
      toast('User created');
      userForm = null;
      loadUsers();
    } catch (e) {
      userForm.error = e.message;
    }
  }

  async function changePassword(u) {
    const password = await promptDialog({ title: `New password for ${u.user}@${u.host}`, value: randomPassword(), action: 'Change' });
    if (password) await attempt(() => post(base + '/users/password', { user: u.user, host: u.host, password }), 'Password changed');
  }

  async function grant(u, revoke = false) {
    const database = await promptDialog({ title: `${revoke ? 'Revoke' : 'Grant'} all privileges on database`, action: revoke ? 'Revoke' : 'Grant' });
    if (database && (await attempt(() => post(base + '/users/grant', { user: u.user, host: u.host, database, revoke }), 'Done'))) loadUsers();
  }

  async function dropUser(u) {
    const ok = await confirmDialog({ title: `Drop ${u.user}@${u.host}?`, message: 'Sites using this account lose database access.', action: 'Drop', danger: true });
    if (ok && (await attempt(() => post(base + '/users/drop', { user: u.user, host: u.host }), 'User dropped'))) loadUsers();
  }
</script>

<div class="page">
  {#if current}
    {#key inst + current}
      <DatabaseView {inst} name={current} system={currentInfo?.system ?? false} onback={() => { current = null; loadDatabases(); }} />
    {/key}
  {:else}
    <div class="page-head">
      <h1>Databases</h1>
      <div class="row">
        {#if instances.length > 1}
          <select bind:value={inst} onchange={switchInstance} aria-label="Database server">
            {#each instances as i}<option value={i.name}>{i.name}</option>{/each}
          </select>
        {:else if inst}<span class="badge">{inst}</span>{/if}
        {#if tab === 'databases' && inst}
          <input type="search" placeholder="Filter databases" bind:value={filter} aria-label="Filter databases" />
          <button class="primary" onclick={() => (creating = { name: '', user: '', password: randomPassword(), error: '' })}>New database</button>
        {/if}
        {#if tab === 'users' && inst}<button class="primary" onclick={() => (userForm = { user: '', host: '%', password: randomPassword(), database: '', error: '' })}>New user</button>{/if}
      </div>
    </div>

    {#if error}<div class="notice bad">{error}</div>{/if}
    {#if !instances.length && !error}
      <div class="card empty">No running MariaDB containers. <a href="#/runtimes">Create one</a>.</div>
    {/if}

    {#if inst}
      <div class="tabs">
        <button class:on={tab === 'databases'} onclick={() => setTab('databases')}>Databases</button>
        <button class:on={tab === 'users'} onclick={() => setTab('users')}>Users</button>
      </div>

      {#if tab === 'databases'}
        <div class="card table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Tables</th><th>Size</th><th>Collation</th><th></th></tr></thead>
            <tbody>
              {#each visible as d (d.name)}
                <tr>
                  <td><button class="link" onclick={() => (current = d.name)}>{d.name}</button>{#if d.system} <span class="badge">system</span>{/if}</td>
                  <td class="small">{d.tables}</td>
                  <td class="small nowrap">{formatBytes(d.size)}</td>
                  <td class="small muted">{d.collation}</td>
                  <td class="actions">{#if !d.system}<button class="danger" onclick={() => dropDatabase(d)}>Drop</button>{/if}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="card table-wrap">
          <table>
            <thead><tr><th>User</th><th>Host</th><th>Databases</th><th></th></tr></thead>
            <tbody>
              {#each users as u (u.user + '@' + u.host)}
                <tr>
                  <td>{u.user}</td>
                  <td class="mono muted">{u.host}</td>
                  <td class="small">{u.databases.join(', ')}</td>
                  <td class="actions">
                    {#if u.user !== 'root'}
                      <button onclick={() => grant(u)}>Grant</button>
                      <button onclick={() => grant(u, true)}>Revoke</button>
                      <button onclick={() => changePassword(u)}>Password</button>
                      <button class="danger" onclick={() => dropUser(u)}>Drop</button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    {/if}
  {/if}
</div>

{#if creating}
  <Modal title="New database" onclose={() => (creating = null)}>
    <form class="form" id="db-form" onsubmit={(e) => { e.preventDefault(); createDatabase(); }}>
      <label class="field">Database name <input bind:value={creating.name} required /></label>
      <label class="field">Create a user for it (optional) <input bind:value={creating.user} placeholder="Leave empty to skip" /></label>
      {#if creating.user}
        <label class="field">Password <input class="mono" bind:value={creating.password} required />
          <span class="hint">Copy this now; it can't be shown later.</span>
        </label>
      {/if}
      {#if creating.error}<div class="error">{creating.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (creating = null)}>Cancel</button>
      <button class="primary" type="submit" form="db-form">Create</button>
    {/snippet}
  </Modal>
{/if}

{#if userForm}
  <Modal title="New user" onclose={() => (userForm = null)}>
    <form class="form" id="user-form" onsubmit={(e) => { e.preventDefault(); createUser(); }}>
      <div class="form-grid">
        <label class="field">User <input bind:value={userForm.user} required /></label>
        <label class="field">Host <input class="mono" bind:value={userForm.host} required /></label>
      </div>
      <label class="field">Password <input class="mono" bind:value={userForm.password} required /></label>
      <label class="field">Grant all privileges on (optional)
        <select bind:value={userForm.database}>
          <option value="">None</option>
          {#each databases.filter((d) => !d.system) as d}<option value={d.name}>{d.name}</option>{/each}
        </select>
      </label>
      {#if userForm.error}<div class="error">{userForm.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (userForm = null)}>Cancel</button>
      <button class="primary" type="submit" form="user-form">Create</button>
    {/snippet}
  </Modal>
{/if}
