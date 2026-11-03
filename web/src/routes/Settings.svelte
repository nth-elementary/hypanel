<script>
  import { onMount } from 'svelte';
  import { ui, get, post, toast, attempt, formatTime } from '../lib/api.svelte.js';

  let current = $state('');
  let next = $state('');
  let error = $state('');
  let audit = $state([]);
  let theme = $state(storedTheme());

  function storedTheme() {
    try {
      return localStorage.getItem('theme') || 'auto';
    } catch {
      return 'auto';
    }
  }

  function applyTheme() {
    if (theme === 'auto') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem('theme', theme);
    } catch {
      // Private windows may block storage; the theme still applies for this visit.
    }
  }

  onMount(async () => {
    try {
      audit = await get('/auth/audit');
    } catch (e) {
      error = e.message;
    }
  });

  async function saveGuidePrefs() {
    await attempt(() => post('/guide/prefs', { showMenu: ui.guideMenu, showDashboard: ui.guideDashboard }));
  }

  async function change(e) {
    e.preventDefault();
    error = '';
    try {
      await post('/auth/password', { current, next });
      toast('Password changed');
      current = next = '';
    } catch (err) {
      error = err.message;
    }
  }
</script>

<div class="page">
  <div class="page-head"><h1>Settings</h1></div>

  <div class="charts">
    <form class="card card-pad form" onsubmit={change}>
      <h2>Change password</h2>
      <label class="field">Current password <input type="password" bind:value={current} autocomplete="current-password" required /></label>
      <label class="field">New password (12 characters or more) <input type="password" bind:value={next} autocomplete="new-password" minlength="12" required /></label>
      {#if error}<div class="error">{error}</div>{/if}
      <div><button class="primary">Change password</button></div>
      <div class="hint">To reset the password or authenticator, run <code>hypanel admin &lt;username&gt;</code> on the server.</div>
    </form>

    <div class="card card-pad form">
      <h2>Appearance</h2>
      <label class="field">Theme
        <select bind:value={theme} onchange={applyTheme}>
          <option value="auto">Match system</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>
      </label>
      <h2 style="margin-top:8px">Guide</h2>
      <label class="row small"><input type="checkbox" bind:checked={ui.guideMenu} onchange={saveGuidePrefs} /> Show Guide in the menu</label>
      <label class="row small"><input type="checkbox" bind:checked={ui.guideDashboard} onchange={saveGuidePrefs} /> Show its first lines on the Dashboard</label>
      <div class="hint">When hidden, it's still at <a href="#/guide">this address</a>.</div>
    </div>
  </div>

  <div class="card">
    <div class="card-head"><h2>Audit log</h2><span class="hint">Most recent 500 actions</span></div>
    {#if !audit.length}
      <div class="empty">Nothing recorded yet.</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Time</th><th>Address</th><th>Action</th><th>Detail</th></tr></thead>
          <tbody>
            {#each audit as a}
              <tr>
                <td class="small nowrap">{formatTime(a.ts)}</td>
                <td class="mono muted nowrap">{a.ip}</td>
                <td class="small nowrap">{a.action}</td>
                <td class="small muted break">{a.detail}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>
