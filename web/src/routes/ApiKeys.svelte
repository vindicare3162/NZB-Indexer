<script>
  import { api } from '../lib/api.js';

  let keys = $state([]);
  let label = $state('');
  let error = $state('');
  let loading = $state(true);

  function load() {
    loading = true;
    api.myKeys()
      .then((k) => { keys = k || []; })
      .catch((err) => { error = err.message; })
      .finally(() => { loading = false; });
  }

  $effect(load);

  async function create() {
    error = '';
    try {
      await api.createKey(label);
      label = '';
      load();
    } catch (err) {
      error = err.message;
    }
  }

  async function remove(id) {
    error = '';
    try {
      await api.deleteKey(id);
      load();
    } catch (err) {
      error = err.message;
    }
  }
</script>

<h2>API Keys</h2>
<p class="muted">Use these keys to connect Sonarr, Radarr, or Prowlarr to the Newznab API at <code>/api</code>.</p>

{#if error}<p class="error">{error}</p>{/if}

<div class="panel row">
  <input placeholder="Label (e.g. sonarr)" bind:value={label} />
  <button onclick={create}>Create key</button>
</div>

<div class="panel">
  {#if loading}
    <p class="muted">Loading…</p>
  {:else if keys.length === 0}
    <p class="muted">No API keys yet.</p>
  {:else}
    <table>
      <thead><tr><th>Label</th><th>Key</th><th>Created</th><th></th></tr></thead>
      <tbody>
        {#each keys as k}
          <tr>
            <td>{k.label}</td>
            <td><code>{k.api_key}</code></td>
            <td>{new Date(k.created_at).toLocaleDateString()}</td>
            <td><button class="danger" onclick={() => remove(k.id)}>Delete</button></td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>
