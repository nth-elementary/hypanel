<script>
  import { ui, post } from '../lib/api.svelte.js';

  let user = $state('');
  let password = $state('');
  let code = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      const res = await post('/auth/login', { user, password, code });
      ui.user = res.user;
      ui.authed = true;
    } catch (err) {
      error = err.message;
      code = '';
    }
    busy = false;
  }
</script>

<div class="login">
  <form class="card form" onsubmit={submit}>
    <h1>HyPanel</h1>
    <label class="field">Username <input bind:value={user} autocomplete="username" required /></label>
    <label class="field">Password <input type="password" bind:value={password} autocomplete="current-password" required /></label>
    <label class="field">Authenticator code
      <input bind:value={code} inputmode="numeric" autocomplete="one-time-code" maxlength="6" pattern="[0-9]{'{'}6{'}'}" required />
    </label>
    {#if error}<div class="error">{error}</div>{/if}
    <button class="primary" disabled={busy} style="justify-content:center">{busy ? 'Signing in...' : 'Sign in'}</button>
  </form>
</div>
