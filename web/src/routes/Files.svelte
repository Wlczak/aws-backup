<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type FilesPage, type FileRow } from '../lib/api';
  import { bytes, formatDate } from '../lib/format';
  import StatusBadge from '../components/StatusBadge.svelte';

  let page = $state(1);
  let limit = $state(50);
  let status = $state('');
  let search = $state('');
  let data = $state<FilesPage | null>(null);
  let err = $state('');
  let selected = $state<FileRow | null>(null);

  let totalPages = $derived(data ? Math.max(1, Math.ceil(data.total / limit)) : 1);

  let searchTimer: number | undefined;

  async function load() {
    try {
      data = await api.files({ page, limit, status: status || undefined, search: search || undefined });
      err = '';
    } catch (e) {
      err = String(e);
    }
  }

  onMount(load);

  function onFilter() {
    page = 1;
    load();
  }
  function onSearch() {
    clearTimeout(searchTimer);
    searchTimer = window.setTimeout(() => { page = 1; load(); }, 250);
  }
  function gotoPage(p: number) {
    if (p < 1 || p > totalPages) return;
    page = p;
    load();
  }
</script>

<h1>Files</h1>
{#if err}<div class="card err">{err}</div>{/if}

<div class="toolbar card">
  <label>
    Status
    <select bind:value={status} on:change={onFilter}>
      <option value="">all</option>
      <option value="pending">pending</option>
      <option value="zipped">zipped</option>
      <option value="uploaded">uploaded</option>
      <option value="failed">failed</option>
      <option value="missing">missing</option>
    </select>
  </label>

  <label class="grow">
    Search
    <input type="text" placeholder="path contains…" bind:value={search} on:input={onSearch} />
  </label>

  <label>
    Per page
    <select bind:value={limit} on:change={onFilter}>
      <option value={25}>25</option>
      <option value={50}>50</option>
      <option value={100}>100</option>
      <option value={250}>250</option>
    </select>
  </label>
</div>

<div class="card nopad">
  <table>
    <thead>
      <tr>
        <th>Path</th>
        <th>Size</th>
        <th>Modified</th>
        <th>Status</th>
        <th>Zip</th>
        <th>Uploaded</th>
      </tr>
    </thead>
    <tbody>
      {#if data}
        {#each data.files as f (f.id)}
          <tr on:click={() => (selected = f)} class:selected={selected?.id === f.id}>
            <td class="mono path">{f.path}</td>
            <td class="mono">{bytes(f.size)}</td>
            <td>{formatDate(f.mtime)}</td>
            <td><StatusBadge status={f.status} /></td>
            <td class="mono muted">{f.zip_name ?? ''}</td>
            <td>{f.uploaded_at ? formatDate(f.uploaded_at) : '—'}</td>
          </tr>
        {/each}
        {#if data.files.length === 0}
          <tr><td colspan="6" class="muted" style="text-align: center; padding: 1.5rem">No files match</td></tr>
        {/if}
      {/if}
    </tbody>
  </table>
</div>

<div class="pager">
  <button on:click={() => gotoPage(page - 1)} disabled={page <= 1} type="button">← Prev</button>
  <span class="muted">Page {page} of {totalPages} · {data?.total.toLocaleString() ?? 0} files</span>
  <button on:click={() => gotoPage(page + 1)} disabled={page >= totalPages} type="button">Next →</button>
</div>

{#if selected}
  <div class="card">
    <div class="row"><span class="label">Path</span><span class="mono">{selected.path}</span></div>
    <div class="row"><span class="label">Status</span><StatusBadge status={selected.status} /></div>
    <div class="row"><span class="label">Size</span><span>{bytes(selected.size)} ({selected.size.toLocaleString()} B)</span></div>
    <div class="row"><span class="label">Modified</span><span>{formatDate(selected.mtime)}</span></div>
    <div class="row"><span class="label">Last seen</span><span>{formatDate(selected.last_seen_at)}</span></div>
    {#if selected.md5}<div class="row"><span class="label">MD5</span><span class="mono">{selected.md5}</span></div>{/if}
    {#if selected.s3_key}<div class="row"><span class="label">S3 key</span><span class="mono">{selected.s3_key}</span></div>{/if}
    {#if selected.zip_name}<div class="row"><span class="label">Zip</span><span class="mono">{selected.zip_name}</span></div>{/if}
    {#if selected.uploaded_at}<div class="row"><span class="label">Uploaded at</span><span>{formatDate(selected.uploaded_at)}</span></div>{/if}
    <button on:click={() => (selected = null)} type="button" style="margin-top: 0.5rem">Close</button>
  </div>
{/if}

<style>
  .toolbar { display: flex; gap: 1rem; align-items: flex-end; flex-wrap: wrap; }
  .toolbar label { display: grid; gap: 0.25rem; font-size: 0.85rem; color: var(--muted); }
  .toolbar .grow { flex: 1; min-width: 200px; }
  .nopad { padding: 0; overflow: hidden; }
  .path { max-width: 520px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  tbody tr { cursor: pointer; }
  tbody tr:hover { background: rgba(255, 255, 255, 0.03); }
  tbody tr.selected { background: rgba(76, 139, 245, 0.08); }
  .pager { display: flex; gap: 1rem; align-items: center; justify-content: center; }
  .row { display: grid; grid-template-columns: 120px 1fr; gap: 0.5rem; padding: 0.25rem 0; }
  .label { color: var(--muted); font-size: 0.85rem; }
  .err { color: var(--err); border-color: var(--err); }
</style>
