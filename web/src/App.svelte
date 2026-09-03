<script>
  import { session, logout, isAdmin } from './lib/session.svelte.js';
  import { route, navigate } from './lib/router.svelte.js';
  import Login from './routes/Login.svelte';
  import Search from './routes/Search.svelte';
  import ReleaseDetail from './routes/ReleaseDetail.svelte';
  import ApiKeys from './routes/ApiKeys.svelte';
  import Admin from './routes/Admin.svelte';

  function onLogout() {
    logout();
    navigate('/');
  }
</script>

{#if !session.authenticated}
  <Login />
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
