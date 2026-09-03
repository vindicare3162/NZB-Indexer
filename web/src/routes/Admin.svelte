<script>
  import { api } from '../lib/api.js';

  let groups = $state([]);
  let users = $state([]);
  let status = $state(null);
  let newGroup = $state('');
  let newUser = $state({ username: '', password: '', admin: false });
  let error = $state('');
  let notice = $state('');

  function loadAll() {
    api.groups().then((g) => { groups = g || []; }).catch((e) => { error = e.message; });
    api.users().then((u) => { users = u || []; }).catch((e) => { error = e.message; });
    api.status().then((s) => { status = s; }).catch(() => {});
  }

  $effect(loadAll);

  async function addGroup() {
    error = '';
    try { await api.createGroup(newGroup); newGroup = ''; loadAll(); }
    catch (e) { error = e.message; }
  }
  async function toggleGroup(g) {
    try { await api.setGroupActive(g.id, !g.active); loadAll(); }
    catch (e) { error = e.message; }
  }
  async function removeGroup(id) {
    try { await api.deleteGroup(id); loadAll(); }
    catch (e) { error = e.message; }
  }
  async function addUser() {
    error = '';
    try {
      await api.createUser(newUser.username, newUser.password, newUser.admin);
      newUser = { username: '', password: '', admin: false };
      loadAll();
    } catch (e) { error = e.message; }
  }
  async function removeUser(id) {
    try { await api.deleteUser(id); loadAll(); }
    catch (e) { error = e.message; }
  }
  async function scan(group) {
    notice = '';
    try { await api.triggerScan(group || ''); notice = 'Scan triggered'; }
    catch (e) { error = e.message; }
  }
  async function backfill(group) {
    notice = '';
    try { await api.triggerBackfill(group || ''); notice = 'Backfill triggered'; }
    catch (e) { error = e.message; }
  }
</script>

<h2>Admin</h2>
{#if error}<p class="error">{error}</p>{/if}
{#if notice}<p class="muted">{notice}</p>{/if}

<div class="panel">
  <h3 style="margin-top:0">Newsgroups</h3>
  <div class="row">
    <input placeholder="alt.binaries.example" bind:value={newGroup} style="flex:1" />
    <button onclick={addGroup}>Add group</button>
  </div>
  {#if groups.length > 0}
    <table style="margin-top:0.8rem">
      <thead><tr><th>Group</th><th>Active</th><th>Fwd pos</th><th>Backfill</th><th></th></tr></thead>
      <tbody>
        {#each groups as g}
          <tr>
            <td>{g.name}</td>
            <td>{g.active ? 'yes' : 'no'}</td>
            <td>{g.last_scanned_high}</td>
            <td>{g.backfill_complete ? 'done' : g.backfill_low}</td>
            <td class="row">
              <button class="secondary" onclick={() => toggleGroup(g)}>{g.active ? 'Disable' : 'Enable'}</button>
              <button class="secondary" onclick={() => scan(g.name)}>Scan</button>
              <button class="secondary" onclick={() => backfill(g.name)}>Backfill</button>
              <button class="danger" onclick={() => removeGroup(g.id)}>Delete</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
  <div class="row" style="margin-top:0.8rem">
    <button onclick={() => scan('')}>Scan all</button>
    <button class="secondary" onclick={() => backfill('')}>Backfill all</button>
  </div>
</div>

<div class="panel">
  <h3 style="margin-top:0">Users</h3>
  <div class="row">
    <input placeholder="username" bind:value={newUser.username} />
    <input type="password" placeholder="password" bind:value={newUser.password} />
    <label class="row" style="gap:0.3rem"><input type="checkbox" bind:checked={newUser.admin} /> admin</label>
    <button onclick={addUser}>Add user</button>
  </div>
  {#if users.length > 0}
    <table style="margin-top:0.8rem">
      <thead><tr><th>Username</th><th>Role</th><th>Active</th><th></th></tr></thead>
      <tbody>
        {#each users as u}
          <tr>
            <td>{u.username}</td>
            <td>{u.role}</td>
            <td>{u.active ? 'yes' : 'no'}</td>
            <td><button class="danger" onclick={() => removeUser(u.id)}>Delete</button></td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<div class="panel">
  <h3 style="margin-top:0">Pipeline status</h3>
  {#if status}
    <pre style="overflow:auto; font-size:0.8rem">{JSON.stringify(status, null, 2)}</pre>
  {:else}
    <p class="muted">No status available.</p>
  {/if}
</div>
