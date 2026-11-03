<script>
  import { onMount } from 'svelte';
  import { get, post, toast } from '../lib/api.svelte.js';
  import Editor from '../lib/Editor.svelte';

  let doc = $state(null);
  let html = $state('');
  let editing = $state(false);
  let draft = $state('');
  let error = $state('');
  let saving = $state(false);

  async function render(text) {
    const { renderMarkdown } = await import('../lib/markdown.js');
    html = renderMarkdown(text);
  }

  async function load() {
    try {
      doc = await get('/guide');
      await render(doc.text);
      error = '';
    } catch (e) {
      error = e.message;
    }
  }
  onMount(load);

  function startEdit() {
    draft = doc.text;
    error = '';
    editing = true;
  }

  function cancelEdit() {
    if (draft !== doc.text && !window.confirm('Discard your changes?')) return;
    editing = false;
  }

  async function save() {
    saving = true;
    error = '';
    try {
      const res = await post('/guide', { text: draft, modTime: doc.modTime });
      doc = { ...doc, text: draft, modTime: res.modTime };
      await render(draft);
      editing = false;
      toast('Saved');
    } catch (e) {
      error = e.message;
    }
    saving = false;
  }

  function key(e) {
    if (editing && (e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      save();
    }
  }
</script>

<svelte:window onkeydown={key} />

<div class="page" style={editing ? 'height:100%' : ''}>
  <div class="page-head">
    <div>
      <h1>Guide</h1>
      <div class="muted small">Your notes for this server, in Markdown. Show or hide this page in <a href="#/settings">Settings</a>.</div>
    </div>
    <div class="row">
      {#if editing}
        <span class="hint">⌘/Ctrl+S saves</span>
        <button onclick={cancelEdit}>Cancel</button>
        <button class="primary" disabled={saving} onclick={save}>{saving ? 'Saving...' : 'Save'}</button>
      {:else if doc}
        <button class="primary" onclick={startEdit}>Edit</button>
      {/if}
    </div>
  </div>

  {#if error}<div class="notice bad">{error}</div>{/if}

  {#if editing}
    <div style="flex:1;min-height:400px;display:flex"><Editor bind:value={draft} filename="guide.md" /></div>
  {:else if doc}
    {#if doc.text.trim()}
      <div class="card card-pad markdown">{@html html}</div>
    {:else}
      <div class="card empty">The guide is empty. Press Edit to write something.</div>
    {/if}
  {/if}
</div>
