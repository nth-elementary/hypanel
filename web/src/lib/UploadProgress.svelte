<script>
  import { formatBytes } from './api.svelte.js';

  let { label, done, total, speed = null, left = null } = $props();

  const percent = $derived(total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 100);

  function timeLeft(seconds) {
    if (seconds < 60) return `${Math.max(1, Math.round(seconds))} s left`;
    if (seconds < 3600) return `${Math.round(seconds / 60)} min left`;
    return `${(seconds / 3600).toFixed(1)} h left`;
  }
</script>

<div class="upload-progress">
  <div class="break">{label}</div>
  <div class="progress" role="progressbar" aria-label="Upload progress" aria-valuemin="0" aria-valuemax="100" aria-valuenow={percent}>
    <div style="width:{percent}%"></div>
  </div>
  <div class="upload-stats small">
    <span>{percent}% · {formatBytes(done)} of {formatBytes(total)}</span>
    {#if speed === null}
      <span class="muted">Measuring speed...</span>
    {:else if speed > 0}
      <span><b>{formatBytes(speed)}/s</b>{#if left !== null}{' · '}{timeLeft(left)}{/if}</span>
    {:else}
      <span class="muted">Waiting for the server...</span>
    {/if}
  </div>
</div>
