<script>
  import { onMount } from 'svelte';
  import { ui, get, post, attempt } from './lib/api.svelte.js';
  import Modal from './lib/Modal.svelte';
  import Login from './routes/Login.svelte';
  import Dashboard from './routes/Dashboard.svelte';
  import Files from './routes/Files.svelte';
  import Terminal from './routes/Terminal.svelte';
  import Websites from './routes/Websites.svelte';
  import SSL from './routes/SSL.svelte';
  import Docker from './routes/Docker.svelte';
  import Stacks from './routes/Stacks.svelte';
  import Runtimes from './routes/Runtimes.svelte';
  import Database from './routes/Database.svelte';
  import Firewall from './routes/Firewall.svelte';
  import Cron from './routes/Cron.svelte';
  import Settings from './routes/Settings.svelte';
  import Guide from './routes/Guide.svelte';

  // To add a page: add a line here. The path after # selects the route.
  const routes = [
    { path: 'dashboard', label: 'Dashboard', page: Dashboard, icon: 'M3 13h7V3H3zM14 21h7V11h-7zM14 7h7V3h-7zM3 21h7v-4H3z' },
    { path: 'websites', label: 'Websites', page: Websites, icon: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3 12h18M12 3c2.5 2.7 3.8 5.7 3.8 9s-1.3 6.3-3.8 9c-2.5-2.7-3.8-5.7-3.8-9S9.5 5.7 12 3z' },
    { path: 'ssl', label: 'SSL', page: SSL, icon: 'M6 11h12v10H6zM8.5 11V7.5a3.5 3.5 0 0 1 7 0V11' },
    { path: 'docker', label: 'Containers', page: Docker, icon: 'M21 8l-9-5-9 5v8l9 5 9-5zM3.5 8.3L12 13l8.5-4.7M12 13v8' },
    { path: 'stacks', label: 'Compose', page: Stacks, icon: 'M12 3l9 5-9 5-9-5zM3 13l9 5 9-5M3 17.5l9 5 9-5' },
    { path: 'runtimes', label: 'Environments', page: Runtimes, icon: 'M8 7l-5 5 5 5M16 7l5 5-5 5M14 4l-4 16' },
    { path: 'database', label: 'Databases', page: Database, icon: 'M12 8c4.4 0 8-1.3 8-3s-3.6-3-8-3-8 1.3-8 3 3.6 3 8 3zM4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3' },
    { path: 'files', label: 'Files', page: Files, icon: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z' },
    { path: 'terminal', label: 'Terminal', page: Terminal, icon: 'M4 5h16v14H4zM7.5 9.5l3 2.5-3 2.5M13 15h4' },
    { path: 'firewall', label: 'Firewall', page: Firewall, icon: 'M12 3l8 3v6c0 4.5-3.2 7.8-8 9-4.8-1.2-8-4.5-8-9V6z' },
    { path: 'cron', label: 'Scheduled jobs', page: Cron, icon: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3.5 2' },
    { path: 'guide', label: 'Guide', page: Guide, icon: 'M4 4.5A1.5 1.5 0 0 1 5.5 3H20v16H5.5A1.5 1.5 0 0 0 4 20.5zM4 20.5A1.5 1.5 0 0 0 5.5 22H20v-3M8 7h8M8 11h6' },
    { path: 'settings', label: 'Settings', page: Settings, icon: 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM4 12h2M18 12h2M12 4v2M12 18v2M6.3 6.3l1.4 1.4M16.3 16.3l1.4 1.4M6.3 17.7l1.4-1.4M16.3 7.7l1.4-1.4' },
  ];

  // Optional pages: each file in extensions/ exports a route, with after naming the page it
  // follows. A build without any is still complete.
  for (const { route } of Object.values(import.meta.glob('./extensions/*.js', { eager: true }))) {
    routes.splice(routes.findIndex((r) => r.path === route.after) + 1, 0, route);
  }

  function parseHash() {
    const raw = location.hash.replace(/^#\/?/, '');
    const [path, query] = raw.split('?');
    return { path: path || 'dashboard', params: new URLSearchParams(query || '') };
  }

  let hash = $state(parseHash());
  const route = $derived(routes.find((r) => r.path === hash.path) ?? routes[0]);
  // A hidden Guide is only left out of the menu; its address still works.
  const menu = $derived(routes.filter((r) => r.path !== 'guide' || ui.guideMenu));

  $effect(() => {
    if (!ui.authed) return;
    get('/guide')
      .then((g) => {
        ui.guideMenu = g.showMenu;
        ui.guideDashboard = g.showDashboard;
      })
      .catch(() => {});
  });

  onMount(async () => {
    const onHash = () => (hash = parseHash());
    addEventListener('hashchange', onHash);
    try {
      const me = await get('/auth/me');
      ui.authed = me.authenticated;
      ui.user = me.user;
    } catch {
      ui.authed = false;
    }
    return () => removeEventListener('hashchange', onHash);
  });

  function closeDialog(result) {
    const d = ui.dialog;
    ui.dialog = null;
    d?.resolve(result);
  }

  const dialogReady = $derived(!ui.dialog?.typed || ui.dialog.input === ui.dialog.typed);

  function dialogKey(e) {
    if (ui.dialog?.enter && e.key === 'Enter' && !e.repeat) {
      e.preventDefault();
      closeDialog(true);
    }
  }

  async function logout() {
    await attempt(() => post('/auth/logout'));
    ui.authed = false;
  }
</script>

<svelte:window onkeydown={dialogKey} />

{#if ui.authed === false}
  <Login />
{:else if ui.authed}
  <div class="shell">
    <nav class="side" aria-label="Sections">
      <div class="brand">
        <svg viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="var(--accent)" /><path d="M10 8v16M22 8v16M10 16h12" stroke="#fff" stroke-width="3.5" stroke-linecap="round" /></svg>
        <span>HyPanel</span>
      </div>
      {#each menu as r}
        <a href="#/{r.path}" class:on={r === route} aria-current={r === route ? 'page' : undefined} title={r.label}>
          <svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d={r.icon} /></svg>
          <span>{r.label}</span>
        </a>
      {/each}
      <div class="grow side-gap"></div>
      <button class="link small" style="padding:7px 9px;justify-content:flex-start;color:var(--text-3)" onclick={logout}>Log out {ui.user}</button>
    </nav>
    <main class="main">
      {#key route.path}
        <route.page params={hash.params} />
      {/key}
    </main>
  </div>
{/if}

{#if ui.task}
  <Modal title={ui.task.title} wide onclose={() => (ui.task = null)}>
    <div class="row">
      {#if !ui.task.finished}
        <span class="badge">● Running</span>
        <span class="hint">You can close this; the task keeps running on the server.</span>
      {:else if ui.task.error}
        <span class="badge bad">✕ Failed</span>
        <span class="error">{ui.task.error}</span>
      {:else}
        <span class="badge good">✓ Finished</span>
      {/if}
    </div>
    <pre class="log" style="flex:1">{ui.task.log || 'Starting...'}</pre>
  </Modal>
{/if}

{#if ui.dialog}
  <Modal title={ui.dialog.title} onclose={() => closeDialog(ui.dialog.prompt ? null : false)}>
    <form class="form" id="dialog-form" onsubmit={(e) => { e.preventDefault(); if (dialogReady) closeDialog(ui.dialog.prompt ? ui.dialog.input : true); }}>
      {#if ui.dialog.message}<div class:mono={ui.dialog.mono} class="dialog-text">{ui.dialog.message}</div>{/if}
      {#if ui.dialog.enter}<div class="hint">Press Enter to continue, Esc to cancel.</div>{/if}
      {#if ui.dialog.typed}
        <label class="field">Type <b>{ui.dialog.typed}</b> to confirm
          <!-- svelte-ignore a11y_autofocus -->
          <input bind:value={ui.dialog.input} autofocus autocomplete="off" />
        </label>
      {:else if ui.dialog.prompt}
        <!-- svelte-ignore a11y_autofocus -->
        <input type={ui.dialog.type} bind:value={ui.dialog.input} autofocus style="width:100%" />
      {/if}
    </form>
    {#snippet foot()}
      <button onclick={() => closeDialog(ui.dialog.prompt ? null : false)}>Cancel</button>
      <button class={ui.dialog.danger ? 'danger' : 'primary'} type="submit" form="dialog-form" disabled={!dialogReady}>{ui.dialog.action}</button>
    {/snippet}
  </Modal>
{/if}

{#if ui.toast}
  <div class="toast" class:bad={ui.toast.bad} role="status">{ui.toast.message}</div>
{/if}
