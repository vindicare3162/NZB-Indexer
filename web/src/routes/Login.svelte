<script>
  import { api } from '../lib/api.js';
  import { login } from '../lib/session.svelte.js';

  let username = $state('');
  let password = $state('');
  let error = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    error = '';
    busy = true;
    try {
      const res = await api.login(username, password);
      login(res.token, res.username, res.role);
    } catch (err) {
      error = err.message || 'Login failed';
    } finally {
      busy = false;
    }
  }
</script>

<div class="login-wrap">
  <form class="panel" onsubmit={submit}>
    <h1>goindex</h1>
    {#if error}<p class="error">{error}</p>{/if}
    <input placeholder="Username" bind:value={username} autocomplete="username" />
    <input type="password" placeholder="Password" bind:value={password} autocomplete="current-password" />
    <button type="submit" disabled={busy}>{busy ? 'Signing in…' : 'Sign in'}</button>
  </form>
</div>
