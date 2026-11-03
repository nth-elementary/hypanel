<script>
  // CodeMirror is loaded on first use, so pages without an editor stay small.
  import { onMount } from 'svelte';

  let { value = $bindable(''), filename = '', onready = null } = $props();
  let host;

  onMount(() => {
    let view;
    let cancelled = false;
    import('./codemirror.js').then(({ createEditor }) => {
      if (cancelled) return;
      view = createEditor(host, value, filename, (next) => (value = next));
      onready?.(view);
    });
    return () => {
      cancelled = true;
      view?.destroy();
    };
  });
</script>

<div class="editor" bind:this={host}></div>
