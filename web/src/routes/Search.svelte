<script>
  import { api } from '../lib/api.js';

  let q = $state('');
  let cat = $state('');
  let includeObfuscated = $state(false);
  let categories = $state([]);
  let releases = $state([]);
  let total = $state(0);
  let limit = $state(50);
  let offset = $state(0);
  let loading = $state(false);
  let error = $state('');
  let searched = $state(false);

  $effect(() => {
    api.categories().then((c) => { categories = c || []; }).catch(() => {});
  });

  async function runSearch(newOffset = 0) {
    loading = true;
    error = '';
    offset = newOffset;
    try {
      const res = await api.search({ q, cat, limit, offset, includeObfuscated });
      releases = res.releases || [];
      total = res.total || 0;
      searched = true;
    } catch (err) {
      error = err.message || 'Search failed';
    } finally {
      loading = false;
    }
  }

  function submit(e) {
    e.preventDefault();
    runSearch(0);
  }

  function fmtSize(bytes) {
    if (!bytes) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let n = bytes, i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(1)} ${units[i]}`;
  }

  function fmtDate(s) {
    if (!s) return '—';
    return new Date(s).toLocaleDateString();
  }

  const hasNext = $derived(offset + limit < total);
  const hasPrev = $derived(offset > 0);
</script>

<form class="panel row" onsubmit={submit}>
  <input placeholder="Search releases…" bind:value={q} style="flex:1; min-width:200px" />
  <select bind:value={cat}>
    <option value="">All categories</option>
    {#each categories.filter((c) => !c.parent_id) as c}
      <option value={c.id}>{c.name}</option>
    {/each}
  </select>
  <button type="submit" disabled={loading}>{loading ? 'Searching…' : 'Search'}</button>
  <label class="muted" style="display:flex; align-items:center; gap:0.3rem">
    <input type="checkbox" bind:checked={includeObfuscated} /> Include obfuscated
  </label>
</form>

{#if error}<p class="error">{error}</p>{/if}

{#if searched}
  <div class="panel">
    {#if releases.length === 0}
      <p class="muted">No results.</p>
    {:else}
      <table>
        <thead>
          <tr><th>Name</th><th>Size</th><th>Posted</th><th></th></tr>
        </thead>
        <tbody>
          {#each releases as r}
            <tr>
              <td><a href={`#/release/${encodeURIComponent(r.guid)}`}>{r.name}</a></td>
              <td>{fmtSize(r.size_bytes)}</td>
              <td>{fmtDate(r.posted_at)}</td>
              <td><a href={api.nzbUrl(r.guid)}>NZB</a></td>
            </tr>
          {/each}
        </tbody>
      </table>

      <div class="row" style="justify-content: space-between; margin-top: 0.8rem;">
        <span class="muted">{total} result{total === 1 ? '' : 's'}</span>
        <div class="row">
          <button class="secondary" disabled={!hasPrev} onclick={() => runSearch(offset - limit)}>Prev</button>
          <button class="secondary" disabled={!hasNext} onclick={() => runSearch(offset + limit)}>Next</button>
        </div>
      </div>
    {/if}
  </div>
{/if}
