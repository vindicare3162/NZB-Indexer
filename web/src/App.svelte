<script>
  import { session, logout, isAdmin } from './lib/session.svelte.js';
  import { route, navigate } from './lib/router.svelte.js';
  import { api } from './lib/api.js';
  import Login from './routes/Login.svelte';
  import Setup from './routes/Setup.svelte';
  import Search from './routes/Search.svelte';
  import ReleaseDetail from './routes/ReleaseDetail.svelte';
  import ApiKeys from './routes/ApiKeys.svelte';
  import Admin from './routes/Admin.svelte';

  // Determine whether first-run setup is required (no users yet). Only checked
  // while unauthenticated. null = still loading.
  let setupRequired = $state(null);

  $effect(() => {
    if (!session.authenticated) {
      api.setupStatus()
        .then((s) => { setupRequired = !!(s && s.setup_required); })
        .catch(() => { setupRequired = false; });
    }
  });

  function onLogout() {
    logout();
    navigate('/');
  }
  function onSetupDone() {
    setupRequired = false;
  }
</script>

{#if !session.authenticated}
  {#if setupRequired === null}
    <div class="login-wrap"><p class="muted" style="text-align:center">Loading…</p></div>
  {:else if setupRequired}
    <Setup onDone={onSetupDone} />
  {:else}
    <Login />
  {/if}
{:else}
  <nav class="topbar">
    <span class="brand">goindex</span>
    <a href="#/" class:active={route.path === '/'}>Search</a>
    <a href="#/apikeys" class:active={route.path === '/apikeys'}>API Keys</a>
    {#if isAdmin()}
      <a href="#/admin" class:active={route.path.startsWith('/admin')}>Admin</a>
    {/if}
    <span class="spacer"></span>
    <span class="muted">{session.username}{#if isAdmin()} <span class="badge">admin</span>{/if}</span>
    <button class="secondary" onclick={onLogout}>Log out</button>
  </nav>

  <div class="container">
    {#if route.path === '/'}
      <Search />
    {:else if route.path.startsWith('/release/')}
      <ReleaseDetail guid={decodeURIComponent(route.path.slice('/release/'.length))} />
    {:else if route.path === '/apikeys'}
      <ApiKeys />
    {:else if route.path.startsWith('/admin')}
      <Admin />
    {:else}
      <p class="muted">Not found.</p>
    {/if}
  </div>
{/if}
