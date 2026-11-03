<script>
  import { icons } from './icons.js';

  let { name } = $props();
</script>

<svg class="icon" class:icon-dots={name === 'more'} viewBox="0 0 24 24" aria-hidden="true"><path d={icons[name]} /></svg>
