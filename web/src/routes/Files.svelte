<script>
  import { onMount } from 'svelte';
  import { get, post, attempt, toast, followTask, confirmDialog, promptDialog, formatBytes, formatTime } from '../lib/api.svelte.js';
  import Modal from '../lib/Modal.svelte';
  import Editor from '../lib/Editor.svelte';
  import Icon from '../lib/Icon.svelte';
  import UploadProgress from '../lib/UploadProgress.svelte';
  import { uploadChunks, trackSpeed } from '../lib/upload.js';

  let { params } = $props();

  const FALLBACK = '/www/sites';
  const RECENT_KEY = 'files.recent';
  const RECENT_MAX = 10;
  const DRAG_TYPE = 'application/x-hypanel-paths';

  let path = $state(FALLBACK);
  let pathInput = $state('');
  let entries = $state([]);
  let error = $state('');
  let selected = $state(new Set());
  let filter = $state('');
  let editing = $state(null);
  let perms = $state(null);
  let upload = $state(null);
  let recent = $state(readRecent());
  let sizes = $state({});
  let preview = $state(null);
  let compressing = $state(null);
  let menuFor = $state(null); // the row whose actions are open, on narrow or touch screens
  let dropTarget = $state(null);
  let desktopDrag = $state(false); // files from the computer are being dragged over the page
  let dragDepth = 0; // dragenter and dragleave fire for every child element crossed
  let dragging = []; // paths being dragged within the panel
  let fileInput;

  const crumbs = $derived.by(() => {
    const parts = path.split('/').filter(Boolean);
    return parts.map((name, i) => ({ name, path: '/' + parts.slice(0, i + 1).join('/') }));
  });
  const parent = $derived(path === '/' ? '/' : path.replace(/\/[^/]*$/, '') || '/');
  const visible = $derived(entries.filter((e) => e.name.toLowerCase().includes(filter.toLowerCase())));
  const previewRows = $derived(
    preview ? preview.entries.filter((e) => e.name.toLowerCase().includes(preview.filter.toLowerCase())) : [],
  );

  const joinTo = (dir, name) => (dir === '/' ? '/' + name : dir + '/' + name);
  const join = (name) => joinTo(path, name);
  const count = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;
  const baseName = (p) => p.replace(/\/+$/, '').split('/').pop() || '/';
  const isArchive = (name) => /\.(zip|tar|tgz|tbz2|txz|tar\.gz|tar\.xz|tar\.bz2)$/i.test(name);
  const archiveStem = (name) => name.replace(/\.(zip|tar|tgz|tbz2|txz|tar\.gz|tar\.xz|tar\.bz2)$/i, '');

  function readRecent() {
    try {
      const list = JSON.parse(localStorage.getItem(RECENT_KEY) || '[]');
      return Array.isArray(list) ? list.filter((p) => typeof p === 'string') : [];
    } catch {
      return [];
    }
  }

  function saveRecent(list) {
    recent = list;
    try {
      localStorage.setItem(RECENT_KEY, JSON.stringify(list));
    } catch {
      // Storage can be blocked; the list still works until the page is closed.
    }
  }

  async function load(next = path) {
    try {
      const res = await get('/files/list?path=' + encodeURIComponent(next));
      const changed = res.path !== path;
      entries = res.entries;
      path = pathInput = res.path;
      selected = new Set();
      if (changed) filter = '';
      error = '';
      saveRecent([res.path, ...recent.filter((p) => p !== res.path)].slice(0, RECENT_MAX));
      return true;
    } catch (e) {
      error = e.message;
      return false;
    }
  }

  onMount(async () => {
    const asked = params.get('path');
    if (asked) {
      await load(asked);
      return;
    }
    // Pick up where the last visit left off, unless that folder is gone.
    if (recent[0] && !(await load(recent[0]))) {
      saveRecent(recent.slice(1));
      await load(FALLBACK);
    } else if (!recent[0]) {
      await load(FALLBACK);
    }
  });

  // A row's actions: icons on a wide screen with a pointer, a labelled list otherwise.
  function actions(e) {
    const list = [];
    if (!e.dir && isArchive(e.name)) {
      list.push({ label: 'Preview', icon: 'eye', run: () => openPreview(e) });
      list.push({ label: 'Extract', icon: 'extract', run: () => extract(e) });
    }
    list.push(
      { label: 'Compress', icon: 'compress', run: () => startCompress([e.path]) },
      { label: 'Download', icon: 'download', href: '/api/files/download?path=' + encodeURIComponent(e.path) },
      { label: 'Rename', icon: 'rename', run: () => rename(e) },
      { label: 'Move', icon: 'move', run: () => moveTo([e.path]) },
      {
        label: 'Permissions',
        icon: 'lock',
        run: () => (perms = { path: e.path, mode: e.mode, owner: e.owner, group: e.group, origOwner: e.owner, origGroup: e.group, recursive: false }),
      },
      { label: 'Delete', icon: 'trash', danger: true, run: () => remove([e.path]) },
    );
    return list;
  }

  function toggle(p) {
    const next = new Set(selected);
    next.has(p) ? next.delete(p) : next.add(p);
    selected = next;
  }

  async function open(entry) {
    if (entry.dir) return load(entry.path);
    if (isArchive(entry.name)) return openPreview(entry);
    try {
      const f = await get('/files/read?path=' + encodeURIComponent(entry.path));
      editing = { path: f.path, content: f.content, modTime: f.modTime, name: entry.name };
    } catch (e) {
      toast(e.message, true);
    }
  }

  async function save() {
    const res = await attempt(() => post('/files/write', { path: editing.path, content: editing.content, modTime: editing.modTime }), 'Saved');
    if (res) {
      editing.modTime = res.modTime;
      load();
    }
  }

  async function newFolder() {
    const name = await promptDialog({ title: 'New folder', action: 'Create' });
    if (name && (await attempt(() => post('/files/mkdir', { path: join(name) })))) load();
  }

  async function newFile() {
    const name = await promptDialog({ title: 'New file', action: 'Create' });
    if (name) editing = { path: join(name), content: '', modTime: 0, name };
  }

  // The server refuses to move or delete what a website or container uses. Going ahead
  // anyway takes retyping the name, since it breaks them. Resolves false if declined.
  async function guarded(send) {
    try {
      await send(false);
      return true;
    } catch (e) {
      if (!e.data?.inUse) throw e;
      const name = baseName(e.message.replace(/ is in use$/, ''));
      const ok = await confirmDialog({
        title: `${name} is in use`,
        message: `Used by:\n${e.data.inUse.join('\n')}\n\nThese will stop working. A website's missing folder can also stop nginx from reloading for every site.`,
        action: 'Do it anyway',
        danger: true,
        typed: name,
      });
      if (!ok) return false;
      await send(true);
      return true;
    }
  }

  async function rename(entry) {
    const name = await promptDialog({ title: 'Rename', value: entry.name, action: 'Rename' });
    if (!name || name === entry.name) return;
    const done = await attempt(() => guarded((force) => post('/files/rename', { from: entry.path, to: join(name), force })));
    if (done) load();
  }

  async function remove(paths) {
    const list = paths.length === 1 ? paths[0] : `${paths.length} items:\n${paths.slice(0, 10).join('\n')}`;
    const ok = await confirmDialog({ title: 'Delete permanently?', message: list, action: 'Delete', danger: true });
    if (!ok) return;
    const done = await attempt(() => guarded((force) => post('/files/delete', { paths, force })));
    if (done) {
      toast('Deleted');
      load();
    }
  }

  async function savePerms() {
    const p = perms;
    let ok = await attempt(() => post('/files/chmod', { path: p.path, mode: p.mode, recursive: p.recursive }));
    if (ok && (p.owner !== p.origOwner || p.group !== p.origGroup)) {
      ok = await attempt(() => post('/files/chown', { path: p.path, owner: p.owner, group: p.group, recursive: p.recursive }));
    }
    if (ok) {
      toast('Permissions updated');
      perms = null;
      load();
    }
  }

  // Folder sizes

  async function measure(p) {
    sizes[p] = { busy: true };
    try {
      const res = await get('/files/size?path=' + encodeURIComponent(p));
      sizes[p] = { size: res.size };
    } catch (e) {
      sizes[p] = { failed: true };
      toast(e.message, true);
    }
  }

  async function measureAll() {
    // One at a time: several du runs at once would thrash the disk on a live server.
    for (const e of visible.filter((x) => x.dir && !x.link)) {
      await measure(e.path);
    }
  }

  // Archives

  async function openPreview(entry) {
    preview = { entry, entries: [], truncated: false, filter: '', loading: true, error: '' };
    try {
      const res = await get('/files/archive?path=' + encodeURIComponent(entry.path));
      if (preview?.entry === entry) Object.assign(preview, { entries: res.entries, truncated: res.truncated, loading: false });
    } catch (e) {
      if (preview?.entry === entry) Object.assign(preview, { error: e.message, loading: false });
    }
  }

  function startCompress(paths) {
    const name = paths.length === 1 ? baseName(paths[0]) : 'archive';
    compressing = { paths, name, format: '.tar.gz', error: '' };
  }

  async function submitCompress() {
    compressing.error = '';
    const { paths, name, format } = compressing;
    try {
      const res = await post('/files/compress', { paths, dest: join(name + format) });
      compressing = null;
      followTask(res.task, () => load());
    } catch (e) {
      compressing.error = e.message;
    }
  }

  async function extract(entry, dest) {
    const target = dest ?? (await promptDialog({ title: 'Extract to', value: join(archiveStem(entry.name)), action: 'Extract' }));
    if (!target) return;
    const res = await attempt(() => post('/files/extract', { path: entry.path, dest: target }));
    if (res) {
      preview = null;
      followTask(res.task, () => load());
    }
  }

  // Uploads

  // Reads files and folders dropped from the desktop. Entries must be collected before the
  // first await, because the browser empties the drop's data right after the event. Array.from
  // because a DataTransferItemList isn't iterable in every browser.
  async function collectDropped(dataTransfer) {
    const roots = Array.from(dataTransfer.items || [])
      .filter((item) => item.kind === 'file')
      .map((item) => ({ entry: item.webkitGetAsEntry?.(), file: item.getAsFile() }));
    if (!roots.length) return Array.from(dataTransfer.files || []).map((file) => ({ file, rel: file.name }));
    const out = [];
    const walk = async (entry, prefix) => {
      if (entry.isFile) {
        const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
        out.push({ file, rel: prefix + entry.name });
        return;
      }
      out.push({ dir: true, rel: prefix + entry.name });
      const reader = entry.createReader();
      // readEntries returns batches, and an empty batch means the end.
      for (;;) {
        const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
        if (!batch.length) break;
        for (const child of batch) await walk(child, prefix + entry.name + '/');
      }
    };
    for (const { entry, file } of roots) {
      if (entry?.isDirectory) await walk(entry, '');
      else if (file) out.push({ file, rel: file.name });
    }
    return out;
  }

  // Describes what's about to be uploaded and what it would replace, then asks. Uploading over
  // an existing file replaces it with no way back, so that gets called out.
  async function confirmUpload(items, dir) {
    const files = items.filter((i) => !i.dir);
    const bytes = files.reduce((sum, i) => sum + i.file.size, 0);
    const tops = new Map();
    for (const item of items) {
      const [top, ...rest] = item.rel.split('/');
      const entry = tops.get(top) ?? { folder: false, files: 0, bytes: 0 };
      entry.folder ||= item.dir || rest.length > 0;
      if (!item.dir) {
        entry.files++;
        entry.bytes += item.file.size;
      }
      tops.set(top, entry);
    }
    let taken = null;
    try {
      const listed = dir === path ? entries : (await get('/files/list?path=' + encodeURIComponent(dir))).entries;
      taken = new Set(listed.map((e) => e.name));
    } catch {
      // Reported in the message below.
    }

    const names = [...tops.keys()];
    const lines = names.slice(0, 12).map((name) => {
      const t = tops.get(name);
      return t.folder ? `${name}/  (${count(t.files, 'file')}, ${formatBytes(t.bytes)})` : `${name}  (${formatBytes(t.bytes)})`;
    });
    if (names.length > lines.length) lines.push(`...and ${names.length - lines.length} more`);
    const clashes = taken ? names.filter((name) => taken.has(name)) : [];
    const sections = [`To:   ${dir}`, `${count(files.length, 'file')}, ${formatBytes(bytes)}`, lines.join('\n')];
    if (clashes.length) {
      const shown = clashes.slice(0, 12).map((name) => (tops.get(name).folder ? `${name}/ is merged: files with the same name are replaced` : `${name} is replaced`));
      if (clashes.length > shown.length) shown.push(`...and ${clashes.length - shown.length} more`);
      sections.push(`Already there:\n${shown.join('\n')}`);
    }
    if (!taken) sections.push(`Couldn't check what's already in ${dir}. Files with the same name are replaced.`);
    return confirmDialog({
      title: `Upload ${names.length === 1 ? names[0] : count(names.length, 'item')}?`,
      message: sections.join('\n\n'),
      action: clashes.length ? 'Upload and replace' : 'Upload',
      danger: clashes.length > 0,
      mono: true,
      enter: true,
    });
  }

  async function uploadItems(items, dir) {
    if (!items.length) {
      toast("The browser didn't provide any files from that drop. Try the Upload button instead.", true);
      return;
    }
    if (!(await confirmUpload(items, dir))) return;
    const files = items.filter((i) => !i.dir);
    const total = files.reduce((sum, i) => sum + i.file.size, 0);
    let index = 0;
    let uploaded = 0;
    let finished = 0; // bytes in files already sent
    let failure = '';
    upload = { name: '', index: 0, count: files.length, done: 0, total, speed: null, left: null };
    const meter = trackSpeed(total, (speed, left) => {
      upload.speed = speed;
      upload.left = left;
    });
    for (const item of items) {
      const dest = joinTo(dir, item.rel);
      try {
        if (item.dir) {
          await post('/files/mkdir', { path: dest, parents: true });
          continue;
        }
        index++;
        upload.name = item.rel;
        upload.index = index;
        await uploadChunks(item.file, dest, (sent) => {
          upload.done = finished + sent;
          meter.set(upload.done);
        });
        finished += item.file.size;
        uploaded++;
      } catch (e) {
        failure = `${item.rel}: ${e.message}`;
        break;
      }
    }
    const average = meter.stop();
    upload = null;
    if (failure) toast(`${failure} (${uploaded} of ${count(files.length, 'file')} uploaded)`, true);
    else if (files.length) toast(`Uploaded ${count(uploaded, 'file')} to ${dir}${total >= 1 << 20 ? `, ${formatBytes(average)}/s on average` : ''}`);
    else toast(`Created ${count(items.length, 'folder')} in ${dir}`);
    load();
  }

  // Drag and drop: rows can be dragged onto folders, breadcrumbs and recent places to move
  // them, and files from the desktop can be dropped on any of those or on the list itself.

  function dragStart(e, entry) {
    dragging = selected.has(entry.path) ? [...selected] : [entry.path];
    e.dataTransfer.setData(DRAG_TYPE, JSON.stringify(dragging));
    e.dataTransfer.effectAllowed = 'move';
  }

  function dragEnd() {
    dragging = [];
    dropTarget = null;
  }

  // Array.from because older Safari gives types as a DOMStringList, without includes().
  const dragTypes = (e) => Array.from(e.dataTransfer?.types || []);
  const isInternal = (e) => dragTypes(e).includes(DRAG_TYPE);
  const isDesktop = (e) => !isInternal(e) && dragTypes(e).includes('Files');

  // A folder can't move into itself, and a move to where it already is does nothing.
  function canMoveTo(dir) {
    return !dragging.some((p) => p === dir || dir.startsWith(p + '/') || (p.replace(/\/[^/]*$/, '') || '/') === dir);
  }

  function dragOver(e, dir) {
    e.preventDefault();
    e.stopPropagation();
    if (isInternal(e) && !canMoveTo(dir)) {
      e.dataTransfer.dropEffect = 'none';
      dropTarget = null;
      return;
    }
    e.dataTransfer.dropEffect = isInternal(e) ? 'move' : 'copy';
    dropTarget = dir;
  }

  function dragLeave(e, dir) {
    if (dropTarget === dir && !e.currentTarget.contains(e.relatedTarget)) dropTarget = null;
  }

  async function dropOn(e, dir) {
    e.preventDefault();
    e.stopPropagation();
    dropTarget = null;
    desktopDrag = false;
    dragDepth = 0;
    try {
      if (isInternal(e)) {
        const paths = JSON.parse(e.dataTransfer.getData(DRAG_TYPE) || '[]');
        if (canMoveTo(dir)) await move(paths, dir);
        dragging = [];
        return;
      }
      await uploadItems(await collectDropped(e.dataTransfer), dir);
    } catch (err) {
      toast(`Couldn't read what was dropped: ${err.message}`, true);
    }
  }

  // Everything a drop target needs. Safari also wants dragenter cancelled, not just dragover.
  const dropProps = (dir) => ({
    ondragenter: (e) => dragOver(e, dir),
    ondragover: (e) => dragOver(e, dir),
    ondragleave: (e) => dragLeave(e, dir),
    ondrop: (e) => dropOn(e, dir),
  });

  // The whole window accepts files from the computer, not just the list. Without this, a drop
  // on empty space would make the browser navigate away to the file.
  function windowEnter(e) {
    if (!isDesktop(e)) return;
    e.preventDefault();
    dragDepth++;
    desktopDrag = true;
  }

  function windowOver(e) {
    if (!isDesktop(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
  }

  function windowLeave(e) {
    if (!isDesktop(e)) return;
    dragDepth = Math.max(0, dragDepth - 1);
    if (!dragDepth) desktopDrag = false;
  }

  function windowDrop(e) {
    if (isDesktop(e)) dropOn(e, path);
  }

  // Moving without dragging, which is the only way on a phone. move() still shows the full
  // from and to before anything happens.
  async function moveTo(paths) {
    const what = paths.length === 1 ? baseName(paths[0]) : count(paths.length, 'item');
    const typed = await promptDialog({ title: `Move ${what} to`, message: 'Full path of the destination folder.', value: path, action: 'Continue' });
    const dir = typed?.trim().replace(/\/+$/, '') || (typed?.trim() ? '/' : '');
    if (!dir) return;
    const staying = paths.filter((from) => (from.replace(/\/[^/]*$/, '') || '/') === dir);
    if (staying.length === paths.length) {
      toast(`Already in ${dir}`, true);
      return;
    }
    await move(paths.filter((from) => !staying.includes(from)), dir);
  }

  async function move(paths, dir) {
    const what = paths.length === 1 ? baseName(paths[0]) : `${paths.length} items`;
    const shown = paths.slice(0, 12).map((from) => `From: ${from}\nTo:   ${joinTo(dir, baseName(from))}`);
    if (paths.length > shown.length) shown.push(`...and ${paths.length - shown.length} more`);
    const ok = await confirmDialog({ title: `Move ${what}?`, message: shown.join('\n\n'), action: 'Move', mono: true, enter: true });
    if (!ok) return;
    let moved = 0;
    for (const from of paths) {
      try {
        if (!(await guarded((force) => post('/files/rename', { from, to: joinTo(dir, baseName(from)), force })))) break;
        moved++;
      } catch (e) {
        toast(`${baseName(from)}: ${e.message}`, true);
        break;
      }
    }
    if (moved) toast(`Moved ${moved} ${moved === 1 ? 'item' : 'items'} to ${dir}`);
    load();
  }
</script>

<svelte:window ondragenter={windowEnter} ondragover={windowOver} ondragleave={windowLeave} ondrop={windowDrop} />

{#if desktopDrag}
  <div class="drop-banner" role="status">Drop to upload to <code>{dropTarget ?? path}</code></div>
{/if}

<div class="page" role="region" aria-label="Files">
  <div class="page-head">
    <h1>Files</h1>
    <div class="row">
      <button onclick={newFolder}>New folder</button>
      <button onclick={newFile}>New file</button>
      <button class="primary" onclick={() => fileInput.click()}>Upload</button>
      <input bind:this={fileInput} type="file" multiple hidden onchange={(e) => { uploadItems([...e.target.files].map((file) => ({ file, rel: file.name })), path); e.target.value = ''; }} />
    </div>
  </div>

  {#if upload}
    <div class="card uploading">
      <UploadProgress
        label="Uploading {upload.count > 1 ? `${upload.index} of ${upload.count}: ` : ''}{upload.name}"
        done={upload.done}
        total={upload.total}
        speed={upload.speed}
        left={upload.left}
      />
    </div>
  {/if}

  <div class="card">
    <div class="card-head">
      <div class="row grow">
        <button
          class:drop-target={dropTarget === parent}
          onclick={() => load(parent)}
          {...dropProps(parent)}
          aria-label="Parent folder"
        >↑</button>
        <button
          class="link crumb"
          class:drop-target={dropTarget === '/'}
          onclick={() => load('/')}
          {...dropProps('/')}
        >/</button>
        {#each crumbs as c, i}
          <button
            class="link crumb"
            class:drop-target={dropTarget === c.path}
            onclick={() => load(c.path)}
            {...dropProps(c.path)}
          >{c.name}</button>
          {#if i < crumbs.length - 1}<span class="muted">/</span>{/if}
        {/each}
      </div>
      <div class="row tools">
        <input class="filter" placeholder="Filter" bind:value={filter} aria-label="Filter files" />
        <input class="mono go" bind:value={pathInput} onkeydown={(e) => e.key === 'Enter' && load(pathInput)} aria-label="Go to path" />
      </div>
    </div>

    {#if selected.size}
      <div class="row small" style="padding:8px 14px;border-bottom:1px solid var(--border);background:var(--surface-2)">
        <b>{selected.size} selected</b>
        <button onclick={() => startCompress([...selected])}>Compress</button>
        <button onclick={() => moveTo([...selected])}>Move</button>
        <button class="danger" onclick={() => remove([...selected])}>Delete</button>
        <button onclick={() => (selected = new Set())}>Clear</button>
      </div>
    {/if}

    {#if error}
      <div class="card-pad error">{error}</div>
    {:else if !visible.length}
      <div class="empty">{entries.length ? 'No matches.' : 'This folder is empty. Drop files here to upload.'}</div>
    {:else}
      <div class="table-wrap">
        <table class="files">
          <thead>
            <tr>
              <th style="width:30px"></th>
              <th>Name</th>
              <th class="col-size">
                Size
                {#if entries.some((e) => e.dir)}<button class="link small" style="margin-left:6px" onclick={measureAll}>Calculate all</button>{/if}
              </th>
              <th class="col-time">Modified</th>
              <th class="col-extra">Mode</th>
              <th class="col-extra">Owner</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {#each visible as e (e.path)}
              <tr
                draggable="true"
                class:drop-target={e.dir && dropTarget === e.path}
                ondragstart={(ev) => dragStart(ev, e)}
                ondragend={dragEnd}
                {...e.dir ? dropProps(e.path) : {}}
              >
                <td><input type="checkbox" checked={selected.has(e.path)} onchange={() => toggle(e.path)} aria-label="Select {e.name}" /></td>
                <td class="break">
                  <button class="link" style="color:{e.dir ? 'var(--accent)' : 'var(--text)'};text-align:left;white-space:normal" onclick={() => open(e)}>
                    {e.dir ? '📁' : isArchive(e.name) ? '🗜️' : '📄'} {e.name}
                  </button>
                  {#if e.link}<span class="muted small"> → {e.link}</span>{/if}
                  <div class="meta muted small">
                    {#if !e.dir}{formatBytes(e.size)}{' · '}{/if}{#if e.dir && sizes[e.path]?.size !== undefined}{formatBytes(sizes[e.path].size)}{' · '}{/if}{formatTime(e.modTime)}
                  </div>
                </td>
                <td class="col-size nowrap muted small">
                  {#if !e.dir}
                    {formatBytes(e.size)}
                  {:else if sizes[e.path]?.busy}
                    Calculating...
                  {:else if sizes[e.path]?.size !== undefined}
                    <button class="link small" style="color:inherit" title="Calculate again" onclick={() => measure(e.path)}>{formatBytes(sizes[e.path].size)}</button>
                  {:else}
                    <button class="link small" onclick={() => measure(e.path)}>Calculate</button>
                  {/if}
                </td>
                <td class="col-time nowrap muted small">{formatTime(e.modTime)}</td>
                <td class="col-extra mono muted">{e.mode}</td>
                <td class="col-extra nowrap muted small">{e.owner}:{e.group}</td>
                <td class="actions">
                  <div class="icons">
                    {#each actions(e) as a (a.label)}
                      {#if a.href}
                        <a class="btn icon-btn" href={a.href} title={a.label} aria-label="{a.label} {e.name}"><Icon name={a.icon} /></a>
                      {:else}
                        <button class="icon-btn" class:danger={a.danger} title={a.label} aria-label="{a.label} {e.name}" onclick={a.run}><Icon name={a.icon} /></button>
                      {/if}
                    {/each}
                  </div>
                  <button class="icon-btn more" title="Actions" aria-label="Actions for {e.name}" onclick={() => (menuFor = e)}><Icon name="more" /></button>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
  {#if recent.length > 1}
    <div class="row small" aria-label="Recently visited">
      <span class="muted">Recent</span>
      {#each recent as p (p)}
        <button
          class="chip mono"
          class:on={p === path}
          class:drop-target={dropTarget === p}
          onclick={() => load(p)}
          {...dropProps(p)}
        >{p}</button>
      {/each}
      <button class="link small" style="color:var(--text-3)" onclick={() => saveRecent([path])}>Clear</button>
    </div>
  {/if}

  <div class="hint drag-hint">Drag items onto a folder, the path above or a recent place to move them. Drop files or folders from your computer anywhere on this page to upload, or onto a folder to upload into it.</div>
</div>

{#if editing}
  <Modal title={editing.path} wide onclose={() => (editing = null)}>
    <Editor bind:value={editing.content} filename={editing.name} />
    {#snippet foot()}
      <button onclick={() => (editing = null)}>Close</button>
      <button class="primary" onclick={save}>Save</button>
    {/snippet}
  </Modal>
{/if}

{#if menuFor}
  <Modal title={menuFor.name} onclose={() => (menuFor = null)}>
    <div class="details small">
      <span class="muted">Path</span><span class="mono break">{menuFor.path}</span>
      <span class="muted">Size</span>
      <span>
        {#if !menuFor.dir}
          {formatBytes(menuFor.size)}
        {:else if sizes[menuFor.path]?.busy}
          Calculating...
        {:else if sizes[menuFor.path]?.size !== undefined}
          {formatBytes(sizes[menuFor.path].size)}
        {:else}
          <button class="link small" onclick={() => measure(menuFor.path)}>Calculate</button>
        {/if}
      </span>
      <span class="muted">Modified</span><span>{formatTime(menuFor.modTime)}</span>
      <span class="muted">Mode</span><span class="mono">{menuFor.mode}</span>
      <span class="muted">Owner</span><span>{menuFor.owner}:{menuFor.group}</span>
    </div>
    <div class="action-list">
      {#each actions(menuFor) as a (a.label)}
        {#if a.href}
          <a class="btn" href={a.href} onclick={() => (menuFor = null)}><Icon name={a.icon} /> {a.label}</a>
        {:else}
          <button class:danger={a.danger} onclick={() => { menuFor = null; a.run(); }}><Icon name={a.icon} /> {a.label}</button>
        {/if}
      {/each}
    </div>
  </Modal>
{/if}

{#if perms}
  <Modal title="Permissions: {perms.path}" onclose={() => (perms = null)}>
    <div class="form">
      <label class="field">Mode (octal) <input class="mono" bind:value={perms.mode} /></label>
      <div class="form-grid">
        <label class="field">Owner <input bind:value={perms.owner} /></label>
        <label class="field">Group <input bind:value={perms.group} /></label>
      </div>
      <label class="row small"><input type="checkbox" bind:checked={perms.recursive} /> Apply to everything inside</label>
    </div>
    {#snippet foot()}
      <button onclick={() => (perms = null)}>Cancel</button>
      <button class="primary" onclick={savePerms}>Apply</button>
    {/snippet}
  </Modal>
{/if}

{#if compressing}
  <Modal title="Compress {compressing.paths.length === 1 ? baseName(compressing.paths[0]) : compressing.paths.length + ' items'}" onclose={() => (compressing = null)}>
    <form class="form" id="compress" onsubmit={(e) => { e.preventDefault(); submitCompress(); }}>
      <div class="form-grid">
        <label class="field">Archive name <input bind:value={compressing.name} required /></label>
        <label class="field">Format
          <select bind:value={compressing.format}>
            <option value=".tar.gz">.tar.gz</option>
            <option value=".zip">.zip</option>
          </select>
        </label>
      </div>
      <div class="hint">Saved as <code>{join(compressing.name + compressing.format)}</code>. Zip archives leave out symlinks.</div>
      {#if compressing.error}<div class="error">{compressing.error}</div>{/if}
    </form>
    {#snippet foot()}
      <button onclick={() => (compressing = null)}>Cancel</button>
      <button class="primary" type="submit" form="compress">Compress</button>
    {/snippet}
  </Modal>
{/if}

{#if preview}
  <Modal title={preview.entry.name} wide onclose={() => (preview = null)}>
    <div class="spread">
      <div class="row small">
        <span class="muted">{preview.loading ? 'Reading archive...' : `${preview.entries.length} entries · ${formatBytes(preview.entry.size)} on disk`}</span>
        {#if preview.truncated}<span class="badge warn">▲ Showing the first {preview.entries.length}</span>{/if}
      </div>
      <input placeholder="Filter" bind:value={preview.filter} aria-label="Filter archive contents" style="width:180px" />
    </div>
    {#if preview.error}<div class="error">{preview.error}</div>{/if}
    <div class="table-wrap" style="flex:1;min-height:0;overflow:auto;border:1px solid var(--border);border-radius:8px">
      <table>
        <thead><tr><th>Name</th><th>Size</th><th>Modified</th></tr></thead>
        <tbody>
          {#each previewRows as a (a.name)}
            <tr>
              <td class="mono break">{a.dir ? '📁 ' : ''}{a.name}{#if a.link}<span class="muted"> → {a.link}</span>{/if}</td>
              <td class="nowrap small muted">{a.dir ? '' : formatBytes(a.size)}</td>
              <td class="nowrap small muted">{formatTime(a.modTime)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    {#snippet foot()}
      <a class="btn" href={'/api/files/download?path=' + encodeURIComponent(preview.entry.path)}>Download</a>
      <button onclick={() => extract(preview.entry, path)}>Extract here</button>
      <button class="primary" onclick={() => extract(preview.entry)}>Extract to folder</button>
    {/snippet}
  </Modal>
{/if}

<style>
  .tools .filter { width: 130px; }
  .tools .go { width: 220px; }
  .uploading { padding: 12px 16px; position: sticky; top: 8px; z-index: 5; }
  .meta { display: none; margin-top: 2px; }
  .icons { display: inline-flex; justify-content: flex-end; }
  .icons > * { margin-left: 4px; }
  .more { display: none; }
  .details { display: grid; grid-template-columns: auto minmax(0, 1fr); gap: 4px 14px; margin-bottom: 14px; }
  .action-list { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 8px; }
  .action-list > * { justify-content: flex-start; min-height: 44px; }

  /* Tooltips need a pointer, and the icons need room: otherwise, one button opens a labelled list. */
  @media (max-width: 1100px), (hover: none) {
    .icons { display: none; }
    .more { display: inline-flex; }
    .col-extra { display: none; }
    .drag-hint { display: none; }
  }
  @media (max-width: 760px) {
    .col-size, .col-time { display: none; }
    .meta { display: block; }
    .files td, .files th { padding-left: 10px; padding-right: 10px; }
    .tools { width: 100%; }
    .tools .filter { flex: 1 1 110px; width: auto; min-width: 0; }
    .tools .go { flex: 3 1 180px; width: auto; min-width: 0; }
  }
</style>
