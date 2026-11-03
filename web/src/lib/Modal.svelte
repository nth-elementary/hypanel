<script>
  let { title, onclose, wide = false, children, foot } = $props();

  function key(e) {
    if (e.key === 'Escape') onclose?.();
  }
</script>

<svelte:window onkeydown={key} />

<div class="overlay" role="presentation">
  <div class="modal" class:wide role="dialog" aria-modal="true" aria-label={title}>
    <div class="modal-head">
      <h2 class="break">{title}</h2>
      <button onclick={onclose} aria-label="Close">✕</button>
    </div>
    <div class="modal-body">{@render children()}</div>
    {#if foot}<div class="modal-foot">{@render foot()}</div>{/if}
  </div>
</div>
