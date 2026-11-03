<script>
  import { onMount } from 'svelte';
  import { Terminal } from '@xterm/xterm';
  import { FitAddon } from '@xterm/addon-fit';
  import '@xterm/xterm/css/xterm.css';
  import { get } from '../lib/api.svelte.js';

  let { params } = $props();

  let host;
  let target = $state(params.get('container') ?? '');
  let containers = $state([]);
  let connected = $state(false);
  let term;
  let fit;
  let socket;

  function connect() {
    socket?.close();
    term.reset();
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    const query = target ? `?container=${encodeURIComponent(target)}` : '';
    socket = new WebSocket(`${proto}://${location.host}/api/terminal/ws${query}`);
    socket.binaryType = 'arraybuffer';
    const current = socket;
    current.onopen = () => {
      connected = true;
      sendSize();
      term.focus();
    };
    current.onmessage = (e) => term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data));
    current.onclose = () => {
      if (current !== socket) return;
      connected = false;
      term.write('\r\n\x1b[90m[session ended]\x1b[0m\r\n');
    };
  }

  function sendSize() {
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ cols: term.cols, rows: term.rows }));
    }
  }

  onMount(() => {
    term = new Terminal({
      fontSize: 13,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      cursorBlink: true,
      scrollback: 5000,
      theme: { background: '#111110', foreground: '#e6e5e0' },
    });
    fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    const encoder = new TextEncoder();
    term.onData((d) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(encoder.encode(d));
    });
    term.onResize(sendSize);

    const observer = new ResizeObserver(() => fit.fit());
    observer.observe(host);

    get('/docker/containers')
      .then((list) => {
        containers = list.filter((c) => c.State === 'running').map((c) => c.Names[0].replace(/^\//, '')).sort();
      })
      .catch(() => {});
    connect();

    return () => {
      observer.disconnect();
      const s = socket;
      socket = null;
      s?.close();
      term.dispose();
    };
  });
</script>

<div class="page" style="height:100%;max-width:none">
  <div class="page-head">
    <h1>Terminal</h1>
    <div class="row">
      <label class="row small muted">Connect to
        <select bind:value={target} onchange={connect} aria-label="Terminal target">
          <option value="">This server (root)</option>
          {#if target && !containers.includes(target)}<option value={target}>{target}</option>{/if}
          {#each containers as c}<option value={c}>{c}</option>{/each}
        </select>
      </label>
      <button onclick={connect}>{connected ? 'Restart session' : 'Reconnect'}</button>
    </div>
  </div>
  <div style="flex:1;min-height:300px;background:#111110;border-radius:var(--radius);padding:10px;overflow:hidden">
    <div bind:this={host} style="height:100%"></div>
  </div>
</div>
