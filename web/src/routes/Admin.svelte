<script>
  import { api } from '../lib/api.js';

  let groups = $state([]);
  let users = $state([]);
  let servers = $state([]);
  let status = $state(null);
  let stats = $state(null);
  let logs = $state([]);
  let logLevel = $state('');
  let logTimer = null;
  let newGroup = $state('');
  let bulkNames = $state('');
  let bulkBackfillDays = $state(7);
  let bulkBusy = $state(false);
  let newUser = $state({ username: '', password: '', admin: false });
  let newServer = $state({ name: '', host: '', port: 563, tls: true, username: '', password: '', max_conns: 10, priority: 0, enabled: true });
  let discoverQuery = $state('');
  let discoverResults = $state([]);
  let discoverTotal = $state(0);
  let discoverOffset = $state(0);
  let discoverLoading = $state(false);
  let discoverCachedAt = $state('');
  let schedule = $state({ scan_interval: '', downstream_interval: '', build_interval: '', postprocess_interval: '' });
  let savingSchedule = $state(false);
  let health = $state(null);
  let error = $state('');
  let notice = $state('');

  function loadAll() {
    api.groups().then((g) => { groups = g || []; }).catch((e) => { error = e.message; });
    api.users().then((u) => { users = u || []; }).catch((e) => { error = e.message; });
    api.servers().then((s) => { servers = s || []; }).catch((e) => { error = e.message; });
    api.status().then((s) => { status = s; }).catch(() => {});
    api.stats().then((s) => { stats = s; }).catch(() => {});
    loadSchedule();
    api.health().then((h) => { health = h; }).catch(() => {});
    loadLogs();
  }

  function fmtBytes(bytes) {
    if (!bytes) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let n = bytes, i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(1)} ${units[i]}`;
  }

  function loadSchedule() {
    api.schedule().then((s) => {
      if (s) schedule = {
        scan_interval: s.scan_interval || '',
        downstream_interval: s.downstream_interval || '',
        build_interval: s.build_interval || '',
        postprocess_interval: s.postprocess_interval || '',
      };
    }).catch(() => {});
  }

  async function saveSchedule(e) {
    e.preventDefault();
    savingSchedule = true;
    error = '';
    notice = '';
    try {
      await api.updateSchedule(schedule);
      notice = 'Schedule updated and applied live';
      loadSchedule();
    } catch (err) {
      error = err.message || 'Failed to update schedule';
    } finally {
      savingSchedule = false;
    }
  }

  function loadLogs() {
    api.logs(logLevel, 200).then((l) => { logs = l || []; }).catch(() => {});
  }

  // Auto-refresh logs every 5s while the admin page is mounted.
  $effect(() => {
    logTimer = setInterval(loadLogs, 5000);
    return () => clearInterval(logTimer);
  });

  function fmtTime(s) {
    if (!s) return '';
    return new Date(s).toLocaleTimeString();
  }
  function levelClass(lvl) {
    if (lvl && lvl.startsWith('ERROR')) return 'error';
    if (lvl && lvl.startsWith('WARN')) return 'muted';
    return '';
  }

  async function addServer() {
    error = '';
    try {
      const payload = { ...newServer, password: newServer.password || null };
      await api.createServer(payload);
      newServer = { name: '', host: '', port: 563, tls: true, username: '', password: '', max_conns: 10, priority: 0, enabled: true };
      loadAll();
    } catch (e) { error = e.message; }
  }
  async function toggleServer(s) {
    try {
      // password null = leave unchanged
      await api.updateServer(s.id, { name: s.name, host: s.host, port: s.port, tls: s.tls, username: s.username, password: null, max_conns: s.max_conns, priority: s.priority, enabled: !s.enabled });
      loadAll();
    } catch (e) { error = e.message; }
  }
  async function removeServer(id) {
    try { await api.deleteServer(id); loadAll(); }
    catch (e) { error = e.message; }
  }

  $effect(loadAll);

  async function addGroup() {
    error = '';
    try { await api.createGroup(newGroup); newGroup = ''; loadAll(); }
    catch (e) { error = e.message; }
  }

  async function addBulk() {
    error = '';
    notice = '';
    const names = bulkNames.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean);
    if (names.length === 0) { error = 'Enter one or more newsgroup names'; return; }
    bulkBusy = true;
    try {
      const res = await api.bulkGroups(names, Number(bulkBackfillDays) || 0);
      notice = `Bulk add: ${res.added} added, ${res.existing} existing, ${res.errors} error(s)`;
      bulkNames = '';
      loadAll();
    } catch (e) { error = e.message; }
    finally { bulkBusy = false; }
  }

  async function runDiscover(offset = 0, refresh = false) {
    error = '';
    discoverLoading = true;
    discoverOffset = offset;
    try {
      const res = await api.discover(discoverQuery, 25, offset, refresh);
      discoverResults = res.groups || [];
      discoverTotal = res.total || 0;
      discoverCachedAt = res.cached_at || '';
    } catch (e) { error = e.message; }
    finally { discoverLoading = false; }
  }
  async function addDiscovered(name) {
    error = '';
    try { await api.createGroup(name); notice = `Added ${name}`; loadAll(); }
    catch (e) { error = e.message; }
  }
  function fmtCount(n) {
    if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return String(n);
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
  async function postProcess() {
    error = '';
    notice = '';
    try { await api.triggerPostProcess(); notice = 'Post-processing triggered'; }
    catch (e) { error = e.message; }
  }
  async function retryFailedPP() {
    error = '';
    notice = '';
    try {
      const res = await api.retryFailedPP();
      notice = `Requeued ${res.requeued} failed release(s) for post-processing`;
      api.stats().then((s) => { stats = s; }).catch(() => {});
    } catch (e) { error = e.message; }
  }
  async function setBackfillTarget(g) {
    error = '';
    const daysStr = prompt(`Backfill target for ${g.name}\n\nDays back to index (blank = use global default, 0 = no day limit):`, g.backfill_target_days ?? '');
    if (daysStr === null) return;
    const artStr = prompt(`Max articles per backfill pass (blank = default, 0 = unlimited):`, g.backfill_target_articles ?? '');
    if (artStr === null) return;
    const days = daysStr.trim() === '' ? null : parseInt(daysStr, 10);
    const articles = artStr.trim() === '' ? null : parseInt(artStr, 10);
    try {
      await api.setGroupBackfill(g.id, days, articles);
      notice = `Backfill target set for ${g.name}`;
      loadAll();
    } catch (e) { error = e.message; }
  }
  function backfillTargetLabel(g) {
    const parts = [];
    if (g.backfill_target_days != null) parts.push(`${g.backfill_target_days}d`);
    if (g.backfill_target_articles != null) parts.push(`${fmtCount(g.backfill_target_articles)} art`);
    return parts.length ? parts.join(' / ') : 'default';
  }
</script>

<h2>Admin</h2>
{#if error}<p class="error">{error}</p>{/if}
{#if notice}<p class="muted">{notice}</p>{/if}

{#if health}
  <div class="panel">
    <h3 style="margin-top:0">
      System health
      <span class="badge" style="background:{health.status === 'ok' ? '#1a7f37' : health.status === 'warn' ? '#9a6700' : '#b42318'}">{health.status}</span>
    </h3>
    <div class="row" style="gap:2rem; flex-wrap:wrap">
      <div>
        <div class="muted" style="font-size:0.8rem">Process</div>
        <div>Goroutines: {health.process.goroutines}</div>
        <div>Heap: {health.process.heap_alloc_mb} MB</div>
        <div>Uptime: {Math.floor(health.process.uptime_secs / 60)} min</div>
        <div class="muted" style="font-size:0.8rem">{health.process.go_version}</div>
      </div>
      {#if health.database}
        <div>
          <div class="muted" style="font-size:0.8rem">Database</div>
          <div>Size: {fmtBytes(health.database.size_bytes)}</div>
          <div>Cache hit: {health.database.cache_hit_ratio < 0 ? '—' : (health.database.cache_hit_ratio * 100).toFixed(1) + '%'}</div>
          <div>Pool: {health.database.pool_total}/{health.database.pool_max} ({health.database.pool_idle} idle)</div>
        </div>
      {/if}
      {#if health.usenet}
        <div>
          <div class="muted" style="font-size:0.8rem">Usenet</div>
          <div>Connections: {health.usenet.pool_open} open, {health.usenet.pool_idle} idle</div>
          <div>Server: {health.usenet.server_configured ? 'configured' : 'not configured'}</div>
        </div>
      {/if}
    </div>
    {#if health.checks && health.checks.length > 0}
      <table style="margin-top:0.8rem">
        <thead><tr><th>Check</th><th>Status</th><th>Detail</th></tr></thead>
        <tbody>
          {#each health.checks as c}
            <tr>
              <td>{c.name}</td>
              <td style="color:{c.status === 'ok' ? '#1a7f37' : c.status === 'warn' ? '#9a6700' : '#b42318'}">{c.status}</td>
              <td class="muted">{c.message || ''}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    {/if}
  </div>
{/if}

<div class="panel">
  <h3 style="margin-top:0">News servers</h3>
  <div class="row" style="flex-wrap:wrap">
    <input placeholder="name" bind:value={newServer.name} style="width:120px" />
    <input placeholder="host" bind:value={newServer.host} style="flex:1; min-width:160px" />
    <input type="number" placeholder="port" bind:value={newServer.port} style="width:80px" />
    <label class="row" style="gap:0.3rem"><input type="checkbox" bind:checked={newServer.tls} /> TLS</label>
    <input placeholder="username" bind:value={newServer.username} style="width:130px" />
    <input type="password" placeholder="password" bind:value={newServer.password} style="width:130px" />
    <input type="number" placeholder="conns" bind:value={newServer.max_conns} style="width:80px" title="max connections" />
    <button onclick={addServer}>Add server</button>
  </div>
  {#if servers.length > 0}
    <table style="margin-top:0.8rem">
      <thead><tr><th>Name</th><th>Host</th><th>Port</th><th>TLS</th><th>User</th><th>Pass</th><th>Conns</th><th>Enabled</th><th></th></tr></thead>
      <tbody>
        {#each servers as s}
          <tr>
            <td>{s.name}</td>
            <td>{s.host}</td>
            <td>{s.port}</td>
            <td>{s.tls ? 'yes' : 'no'}</td>
            <td>{s.username || '—'}</td>
            <td>{s.has_password ? '••••' : '—'}</td>
            <td>{s.max_conns}</td>
            <td>{s.enabled ? 'yes' : 'no'}</td>
            <td class="row">
              <button class="secondary" onclick={() => toggleServer(s)}>{s.enabled ? 'Disable' : 'Enable'}</button>
              <button class="danger" onclick={() => removeServer(s.id)}>Delete</button>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  {:else}
    <p class="muted">No news servers configured.</p>
  {/if}
</div>

<div class="panel">
  <h3 style="margin-top:0">Discover newsgroups</h3>
  <p class="muted" style="margin-top:0">Search the groups your provider carries and add ones to index. The first search fetches the full list from the provider (can take a few seconds).</p>
  <div class="row">
    <input placeholder="filter e.g. alt.binaries" bind:value={discoverQuery} style="flex:1; min-width:200px"
           onkeydown={(e) => { if (e.key === 'Enter') runDiscover(0); }} />
    <button onclick={() => runDiscover(0)} disabled={discoverLoading}>{discoverLoading ? 'Searching…' : 'Search'}</button>
    <button class="secondary" onclick={() => runDiscover(0, true)} disabled={discoverLoading} title="Refresh cached list from provider">Refresh</button>
  </div>
  {#if discoverResults.length > 0}
    <table style="margin-top:0.8rem">
      <thead><tr><th>Group</th><th>~Articles</th><th>Status</th><th></th></tr></thead>
      <tbody>
        {#each discoverResults as g}
          <tr>
            <td>{g.name}</td>
            <td class="muted">{fmtCount(g.estimated_count)}</td>
            <td class="muted">{g.status}</td>
            <td><button class="secondary" onclick={() => addDiscovered(g.name)}>Add</button></td>
          </tr>
        {/each}
      </tbody>
    </table>
    <div class="row" style="justify-content:space-between; margin-top:0.6rem">
      <span class="muted">{discoverTotal} match{discoverTotal === 1 ? '' : 'es'}{#if discoverCachedAt} · list cached {new Date(discoverCachedAt).toLocaleTimeString()}{/if}</span>
      <div class="row">
        <button class="secondary" disabled={discoverOffset <= 0} onclick={() => runDiscover(discoverOffset - 25)}>Prev</button>
        <button class="secondary" disabled={discoverOffset + 25 >= discoverTotal} onclick={() => runDiscover(discoverOffset + 25)}>Next</button>
      </div>
    </div>
  {/if}
</div>

<div class="panel">
  <h3 style="margin-top:0">Newsgroups</h3>
  <div class="row">
    <input placeholder="alt.binaries.example" bind:value={newGroup} style="flex:1" />
    <button onclick={addGroup}>Add group</button>
  </div>
  <details style="margin-top:0.6rem">
    <summary>Bulk add</summary>
    <p class="muted">Paste multiple newsgroup names (one per line, or comma/space separated). A backfill window of N days is applied to each.</p>
    <textarea placeholder="alt.binaries.movies&#10;alt.binaries.tv&#10;alt.binaries.sounds.flac" bind:value={bulkNames} rows="5" style="width:100%"></textarea>
    <div class="row" style="margin-top:0.5rem; align-items:center; gap:0.6rem">
      <label>Backfill days <input type="number" min="0" bind:value={bulkBackfillDays} style="width:5rem" /></label>
      <button onclick={addBulk} disabled={bulkBusy}>{bulkBusy ? 'Adding…' : 'Add all'}</button>
    </div>
  </details>
  {#if groups.length > 0}
    <table style="margin-top:0.8rem">
      <thead><tr><th>Group</th><th>Active</th><th>Fwd pos</th><th>Backfill</th><th>Target</th><th></th></tr></thead>
      <tbody>
        {#each groups as g}
          <tr>
            <td>{g.name}</td>
            <td>{g.active ? 'yes' : 'no'}</td>
            <td>{g.last_scanned_high}</td>
            <td>{g.backfill_complete ? 'done' : g.backfill_low}</td>
            <td class="muted">{backfillTargetLabel(g)}</td>
            <td class="row">
              <button class="secondary" onclick={() => toggleGroup(g)}>{g.active ? 'Disable' : 'Enable'}</button>
              <button class="secondary" onclick={() => scan(g.name)}>Scan</button>
              <button class="secondary" onclick={() => backfill(g.name)}>Backfill</button>
              <button class="secondary" onclick={() => setBackfillTarget(g)}>Target</button>
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
    <button class="secondary" onclick={() => postProcess()}>Run post-processing now</button>
    <button class="secondary" onclick={() => retryFailedPP()}>Retry failed post-processing</button>
  </div>
</div>

<div class="panel">
  <h3 style="margin-top:0">Schedule</h3>
  <p class="muted">How often each pipeline stage runs. Use durations like <code>30s</code>, <code>5m</code>, <code>1h</code>. Changes apply live and persist across restarts.</p>
  <form onsubmit={saveSchedule}>
    <div class="row" style="flex-wrap:wrap; gap:0.8rem">
      <label>Scan<br /><input placeholder="15m" bind:value={schedule.scan_interval} /></label>
      <label>Assemble<br /><input placeholder="5m" bind:value={schedule.downstream_interval} /></label>
      <label>Build<br /><input placeholder="2m" bind:value={schedule.build_interval} /></label>
      <label>Post-process<br /><input placeholder="5m" bind:value={schedule.postprocess_interval} /></label>
    </div>
    <div class="row" style="margin-top:0.8rem">
      <button type="submit" disabled={savingSchedule}>{savingSchedule ? 'Saving…' : 'Save schedule'}</button>
    </div>
  </form>
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
  <h3 style="margin-top:0">Pipeline depth</h3>
  {#if stats}
    <table>
      <tbody>
        <tr><td>Parts (total, est.)</td><td>{fmtCount(stats.parts_total)}</td></tr>
        <tr><td>Parts awaiting assembly (est.)</td><td>{fmtCount(stats.parts_unassigned)}</td></tr>
        <tr><td>Binaries (total / complete)</td><td>{fmtCount(stats.binaries_total)} / {fmtCount(stats.binaries_complete)}</td></tr>
        <tr><td>Complete binaries awaiting release</td><td>{fmtCount(stats.binaries_unreleased)}</td></tr>
        <tr><td>Releases (total)</td><td>{fmtCount(stats.releases_total)}</td></tr>
        <tr><td>Releases pending post-processing</td><td>{fmtCount((stats.releases_by_pp_status || {}).pending || 0)}</td></tr>
        <tr><td>Releases post-processed (done)</td><td>{fmtCount((stats.releases_by_pp_status || {}).done || 0)}</td></tr>
        <tr><td>Releases failed (awaiting retry)</td><td>{fmtCount(((stats.releases_by_pp_status || {}).failed || 0) - stats.releases_failed_exhausted)}</td></tr>
        <tr><td>Releases failed (retries exhausted)</td><td>{fmtCount(stats.releases_failed_exhausted)}</td></tr>
      </tbody>
    </table>
    <p class="muted" style="font-size:0.8rem">Parts totals are estimates; binary/release counts are exact.</p>

    {#if stats.groups && stats.groups.length > 0}
      <h4 style="margin:0.8rem 0 0.3rem">Releases by group</h4>
      <table>
        <thead><tr><th>Group</th><th>Releases</th><th>Pending pp</th></tr></thead>
        <tbody>
          {#each stats.groups as gr}
            <tr><td>{gr.name}</td><td>{fmtCount(gr.releases_total)}</td><td>{fmtCount(gr.releases_pending)}</td></tr>
          {/each}
        </tbody>
      </table>
    {/if}
  {:else}
    <p class="muted">No stats available.</p>
  {/if}

  <h3>Pipeline status</h3>
  {#if status}
    <pre style="overflow:auto; font-size:0.8rem">{JSON.stringify(status, null, 2)}</pre>
  {:else}
    <p class="muted">No status available.</p>
  {/if}
</div>

<div class="panel">
  <div class="row" style="justify-content:space-between">
    <h3 style="margin:0">Logs</h3>
    <div class="row">
      <select bind:value={logLevel} onchange={loadLogs}>
        <option value="">All levels</option>
        <option value="info">Info+</option>
        <option value="warn">Warn+</option>
        <option value="error">Error only</option>
      </select>
      <button class="secondary" onclick={loadLogs}>Refresh</button>
    </div>
  </div>
  {#if logs.length === 0}
    <p class="muted" style="margin-bottom:0">No log entries.</p>
  {:else}
    <div style="max-height:360px; overflow:auto; font-family:monospace; font-size:0.78rem; margin-top:0.6rem">
      {#each logs as e}
        <div class={levelClass(e.level)} style="padding:0.15rem 0; border-bottom:1px solid var(--border)">
          <span class="muted">{fmtTime(e.time)}</span>
          <span style="display:inline-block; width:52px">{e.level}</span>
          {e.message}
          {#if e.attrs}
            {#each Object.entries(e.attrs) as [k, v]}
              <span class="muted"> {k}={v}</span>
            {/each}
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>
