<script>
  import { api } from '../lib/api.js';
  import { login } from '../lib/session.svelte.js';

  let { onDone } = $props();

  let username = $state('');
  let password = $state('');
  let confirm = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    error = '';
    if (password.length < 8) {
      error = 'Password must be at least 8 characters.';
      return;
    }
    if (password !== confirm) {
      error = 'Passwords do not match.';
      return;
    }
    busy = true;
    try {
      const res = await api.setup(username, password);
      if (res && res.token) {
        login(res.token, res.username, res.role);
      }
      onDone?.();
    } catch (err) {
      error = err.message || 'Setup failed';
    } finally {
      busy = false;
    }
  }
</script>

<div class="login-wrap">
  <form class="panel" onsubmit={submit}>
    <h1>Welcome to goindex</h1>
    <p class="muted" style="text-align:center; margin-top:0">Create the first administrator account.</p>
    {#if error}<p class="error">{error}</p>{/if}
    <input placeholder="Admin username" bind:value={username} autocomplete="username" />
    <input type="password" placeholder="Password (min 8 chars)" bind:value={password} autocomplete="new-password" />
    <input type="password" placeholder="Confirm password" bind:value={confirm} autocomplete="new-password" />
    <button type="submit" disabled={busy}>{busy ? 'Creating…' : 'Create admin account'}</button>
  </form>
</div>
