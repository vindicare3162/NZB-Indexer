<script>
  import { api } from '../lib/api.js';

  let { guid } = $props();

  let release = $state(null);
  let files = $state([]);
  let error = $state('');
  let loading = $state(true);

  $effect(() => {
    loading = true;
    error = '';
    api.release(guid)
      .then((res) => {
        release = res.release;
        files = res.files || [];
      })
      .catch((err) => { error = err.message || 'Failed to load release'; })
      .finally(() => { loading = false; });
  });

  function fmtSize(bytes) {
    if (!bytes) return '—';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let n = bytes, i = 0;
    while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(1)} ${units[i]}`;
  }
</script>

<p><a href="#/">← Back to search</a></p>

{#if loading}
  <p class="muted">Loading…</p>
{:else if error}
  <p class="error">{error}</p>
{:else if release}
  <div class="panel">
    <h2 style="margin-top:0">{release.name}</h2>
    <div class="row" style="gap:1.2rem">
      <span class="muted">Size: {fmtSize(release.size_bytes)}</span>
      <span class="muted">Parts: {release.total_parts}</span>
      <span class="muted">Grabs: {release.grabs}</span>
      <span class="badge">{release.pp_status}</span>
    </div>
    <p style="margin-top:1rem">
      <a href={api.nzbUrl(release.guid)}><button>Download NZB</button></a>
    </p>
  </div>

  {#if release.nfo}
    <div class="panel">
      <h3 style="margin-top:0">NFO</h3>
      <pre style="overflow:auto; white-space:pre-wrap; font-size:0.8rem">{release.nfo}</pre>
    </div>
  {/if}

  {#if files.length > 0}
    <div class="panel">
      <h3 style="margin-top:0">Files</h3>
      <table>
        <thead><tr><th>File</th><th>Size</th><th>Segments</th></tr></thead>
        <tbody>
          {#each files as f}
            <tr>
              <td>{f.file_name}</td>
              <td>{fmtSize(f.size_bytes)}</td>
              <td>{(f.segments || []).length}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
{/if}
