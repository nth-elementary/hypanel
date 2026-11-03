<script>
  import { onMount } from 'svelte';
  import { get, post, followTask, attempt, toast } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';

  let catalog = $state(null);
  let kind = $state('php');
  let form = $state({ name: '', version: '', port: 9100, code: '/srv/', command: '', appPort: 3000, extensions: '' });
  let error = $state('');
  let created = $state('');
  let guide = $state(null);

  async function openGuide() {
    const res = await attempt(() => get('/guide/agents'));
    if (res) guide = res.text;
  }

  async function copyGuide() {
    try {
      await navigator.clipboard.writeText(guide);
      toast('Copied');
    } catch {
      toast('Copying needs HTTPS. Select the text and copy it instead.', true);
    }
  }

  const cards = {
    php: { title: 'PHP', text: 'PHP-FPM for WordPress, MediaWiki and similar. Serves /www/sites.' },
    node: { title: 'Node.js', text: 'Runs your Node app from a folder on this server.' },
    python: { title: 'Python', text: 'Runs your Python app from a folder on this server.' },
    mariadb: { title: 'MariaDB', text: 'A separate database server, managed under Databases.' },
  };
  const defaults = {
    php: { port: 9100 },
    node: { port: 3100, appPort: 3000, command: 'npm ci && npm start' },
    python: { port: 8100, appPort: 8000, command: 'pip install -r requirements.txt && python main.py' },
    mariadb: { port: 3307 },
  };

  function choose(next) {
    kind = next;
    error = '';
    created = '';
    form = { ...form, ...defaults[next], version: catalog?.[next]?.versions?.[0] ?? '' };
  }

  onMount(async () => {
    try {
      catalog = await get('/runtimes/catalog');
      choose('php');
    } catch (e) {
      error = e.message;
    }
  });

  async function submit(e) {
    e.preventDefault();
    error = '';
    try {
      const res = await post('/runtimes', {
        kind,
        name: form.name,
        version: form.version,
        port: Number(form.port),
        code: form.code,
        command: form.command,
        appPort: Number(form.appPort),
        extensions: form.extensions.split(/[\s,]+/).filter(Boolean),
      });
      const name = form.name;
      followTask(res.task, (ok) => {
        if (ok) created = name;
      });
    } catch (err) {
      error = err.message;
    }
  }

  const usesCode = $derived(kind === 'node' || kind === 'python');
</script>

<div class="page">
  <div class="page-head">
    <div>
      <h1>Environments</h1>
      <div class="muted small">Each one is a normal compose stack, listed under <a href="#/stacks">Compose</a> after it's created.</div>
    </div>
    <button onclick={openGuide}>Guide for coding agents</button>
  </div>

  <div class="tiles">
    {#each Object.entries(cards) as [id, c]}
      <button class="card tile" style="display:block;height:auto;text-align:left;white-space:normal;border-color:{kind === id ? 'var(--accent)' : 'var(--border)'}" aria-pressed={kind === id} onclick={() => choose(id)}>
        <h2>{c.title}</h2>
        <div class="sub">{c.text}</div>
      </button>
    {/each}
  </div>

  <form class="card card-pad form" onsubmit={submit} style="max-width:720px">
    <h2>New {cards[kind].title} environment</h2>
    <div class="form-grid">
      <label class="field">Name <input bind:value={form.name} placeholder="my-{kind}" required /></label>
      <label class="field">Version
        <input bind:value={form.version} list="versions" required />
        <datalist id="versions">{#each catalog?.[kind]?.versions ?? [] as v}<option value={v}></option>{/each}</datalist>
      </label>
      <label class="field">Port on this server <input type="number" bind:value={form.port} min="1024" max="65535" required /></label>
    </div>

    {#if usesCode}
      <div class="form-grid">
        <label class="field">Source folder <input class="mono" bind:value={form.code} required /></label>
        <label class="field">App listens on port <input type="number" bind:value={form.appPort} required /></label>
      </div>
      <label class="field">Start command <input class="mono" bind:value={form.command} required /></label>
    {/if}

    {#if kind === 'php'}
      <label class="field">Extra extensions (optional)
        <input class="mono" bind:value={form.extensions} placeholder="imagick soap" />
        <span class="hint">mysqli, pdo_mysql, gd, intl, zip, exif, opcache, bcmath, apcu, memcached and redis are included.</span>
      </label>
    {/if}

    <div class="hint">The port binds to 127.0.0.1, so it's only reachable through nginx. Add a <a href="#/websites">website</a> to expose it.</div>
    {#if error}<div class="error">{error}</div>{/if}
    {#if created}<div class="badge good" style="align-self:flex-start">✓ {created} is running</div>{/if}
    <div><button class="primary">Create</button></div>
  </form>
</div>

{#if guide}
  <Modal title="Guide for coding agents" wide onclose={() => (guide = null)}>
    <div class="spread">
      <span class="hint">Agents working in /opt/stacks on this server pick this up by themselves, as AGENTS.md and CLAUDE.md.</span>
      <button onclick={copyGuide}>Copy</button>
    </div>
    <pre class="log" style="flex:1;font-size:12.5px">{guide}</pre>
  </Modal>
{/if}
