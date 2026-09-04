<script>
  import { api } from '../lib/api.js';
  import { route, replaceQuery, pushQuery } from '../lib/router.svelte.js';
  import { parseSearchState, serializeSearchState, offsetFor, statesEqual } from '../lib/searchstate.js';

  let categories = $state([]);
  let releases = $state([]);
  let total = $state(0);
  let approximate = $state(false);
  let hasMore = $state(false);
  let loading = $state(false);
  let error = $state('');
  let searched = $state(false);
  let copied = $state(''); // guid of the row whose NZB URL was just copied

  // Editable form fields, seeded from the URL. The URL is the source of truth
  // for a *committed* search; these bind the inputs before commit.
  let q = $state('');
  let cat = $state('');
  let includeObfuscated = $state(false);
  let limit = $state(50);
  let page = $state(1);

  $effect(() => {
    api.categories().then((c) => { categories = c || []; }).catch(() => {});
  });

  // The last committed state we ran a search for, so we don't re-run redundantly.
  let lastRun = null;
  let debounceTimer = null;

  // React to URL query changes (deep link, back/forward, or our own updates):
  // parse the state, sync the form fields, and run the search if it changed.
  $effect(() => {
    const state = parseSearchState(route.query);
    q = state.q;
    cat = state.cat;
    includeObfuscated = state.obf;
    limit = state.limit;
    page = state.page;
    if (!lastRun || !statesEqual(lastRun, state)) {
      runSearch(state);
    }
  });

  async function runSearch(state) {
    lastRun = state;
    loading = true;
    error = '';
    try {
      const res = await api.search({
        q: state.q, cat: state.cat, limit: state.limit,
        offset: offsetFor(state), includeObfuscated: state.obf,
      });
      // Discard if a newer search superseded this one while it was in flight.
      if (!statesEqual(lastRun, state)) return;
      releases = res.releases || [];
      total = res.total || 0;
      approximate = !!res.approximate;
      hasMore = !!res.has_more;
      searched = true;
    } catch (err) {
      if (statesEqual(lastRun, state)) error = err.message || 'Search failed';
    } finally {
      if (statesEqual(lastRun, state)) loading = false;
    }
  }

  // currentState builds a SearchState from the editable fields.
  function currentState(overrides = {}) {
    return {
      q, cat, page, limit, obf: includeObfuscated, ...overrides,
    };
  }

  // commit writes state to the URL (a history entry), which triggers the effect
  // to run the search. push=true adds a back/forward entry.
  function commit(state, push = true) {
    const query = serializeSearchState(state);
    if (push) pushQuery(query); else replaceQuery(query);
  }

  function submit(e) {
    e.preventDefault();
    if (debounceTimer) { clearTimeout(debounceTimer); debounceTimer = null; }
    commit(currentState({ page: 1 }));
  }

  // Debounced live search as the user types, without flooding history or the
  // API: replaceQuery (no new history entry) after a short pause.
  function onQueryInput() {
    if (debounceTimer) clearTimeout(debounceTimer);
    debounceTimer = setTimeout(() => {
      debounceTimer = null;
      commit(currentState({ page: 1 }), false);
    }, 400);
  }

  function goToPage(p) {
    if (p < 1) return;
    commit(currentState({ page: p }));
  }

  async function copyNzb(guid) {
    const url = new URL(api.nzbUrl(guid), window.location.origin).href;
    try {
      await navigator.clipboard.writeText(url);
      copied = guid;
      setTimeout(() => { if (copied === guid) copied = ''; }, 1500);
    } catch { /* clipboard unavailable; the visible NZB link still works */ }
  }

  const catNames = $derived(new Map(categories.map((c) => [c.id, c.name])));
  function catName(id) {
    if (id == null) return '—';
    return catNames.get(id) || `#${id}`;
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

  const hasNext = $derived(hasMore || page * limit < total);
  const hasPrev = $derived(page > 1);
  const totalLabel = $derived(approximate ? `${total}+` : `${total}`);
</script>

<form class="panel row" onsubmit={submit}>
  <input placeholder="Search releases…" bind:value={q} oninput={onQueryInput}
         style="flex:1; min-width:200px" aria-label="Search releases" />
  <select bind:value={cat} onchange={() => commit(currentState({ page: 1 }))}>
    <option value="">All categories</option>
    {#each categories.filter((c) => !c.parent_id) as c}
      <option value={c.id}>{c.name}</option>
    {/each}
  </select>
  <button type="submit" disabled={loading}>{loading ? 'Searching…' : 'Search'}</button>
  <label class="muted" style="display:flex; align-items:center; gap:0.3rem">
    <input type="checkbox" bind:checked={includeObfuscated}
           onchange={() => commit(currentState({ page: 1 }))} /> Include obfuscated
  </label>
</form>

{#if error}<p class="error" role="alert">{error}</p>{/if}

{#if loading && !searched}
  <div class="panel"><p class="muted">Searching…</p></div>
{:else if searched}
  <div class="panel" aria-busy={loading}>
    {#if releases.length === 0}
      <p class="muted">No results{q ? ` for “${q}”` : ''}.</p>
    {:else}
      <table>
        <thead>
          <tr><th>Name</th><th>Category</th><th>Size</th><th>Posted</th><th></th></tr>
        </thead>
        <tbody>
          {#each releases as r}
            <tr>
              <td><a href={`#/release/${encodeURIComponent(r.guid)}`}>{r.name}</a></td>
              <td class="muted">{catName(r.category_id)}</td>
              <td>{fmtSize(r.size_bytes)}</td>
              <td>{fmtDate(r.posted_at)}</td>
              <td class="row" style="gap:0.4rem">
                <a href={api.nzbUrl(r.guid)}>NZB</a>
                <button type="button" class="secondary" style="padding:0.1rem 0.4rem; font-size:0.75rem"
                        onclick={() => copyNzb(r.guid)} title="Copy NZB URL">
                  {copied === r.guid ? 'Copied' : 'Copy'}
                </button>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>

      <div class="row" style="justify-content: space-between; margin-top: 0.8rem;">
        <span class="muted">{totalLabel} result{total === 1 && !approximate ? '' : 's'} · page {page}</span>
        <div class="row">
          <button class="secondary" disabled={!hasPrev || loading} onclick={() => goToPage(page - 1)}>Prev</button>
          <button class="secondary" disabled={!hasNext || loading} onclick={() => goToPage(page + 1)}>Next</button>
        </div>
      </div>
    {/if}
  </div>
{/if}
