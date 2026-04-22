<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type FilesPage, type FileRow } from '../lib/api';
  import { bytes, formatDate } from '../lib/format';
  import { selection, toggle, clear, ids, paths } from '../lib/selection';
  import { go } from '../lib/router';
  import StatusBadge from '../components/StatusBadge.svelte';
  import FileTreeNode from '../components/FileTreeNode.svelte';
  import { buildTree, collectFiles, type TreeNode } from '../lib/tree';

  type ViewMode = 'tree' | 'flat';
  const VIEW_KEY = 'aws-backup:files-view';

  let viewMode = $state<ViewMode>(
    (typeof localStorage !== 'undefined' && (localStorage.getItem(VIEW_KEY) as ViewMode)) || 'tree',
  );
  let page = $state(1);
  let limit = $state(50);
  let status = $state('');
  let search = $state('');
  let data = $state<FilesPage | null>(null);
  let err = $state('');
  let detail = $state<FileRow | null>(null);
  let busy = $state(false);
  let expanded = $state<Set<string>>(new Set(['']));

  let totalPages = $derived(
    data && viewMode === 'flat' ? Math.max(1, Math.ceil(data.total / limit)) : 1,
  );
  let selectedIDs = $derived(new Set($selection.map((f) => f.id)));
  let tree = $derived<TreeNode | null>(
    viewMode === 'tree' && data ? buildTree(data.files) : null,
  );

  let searchTimer: number | undefined;

  async function load() {
    try {
      if (viewMode === 'tree') {
        data = await api.files({
          all: true,
          status: status || undefined,
          search: search || undefined,
        });
      } else {
        data = await api.files({
          page,
          limit,
          status: status || undefined,
          search: search || undefined,
        });
      }
      err = '';
    } catch (e) {
      err = String(e);
    }
  }

  onMount(load);

  function setView(m: ViewMode) {
    if (m === viewMode) return;
    viewMode = m;
    page = 1;
    try { localStorage.setItem(VIEW_KEY, m); } catch { /* private mode */ }
    load();
  }

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

  function toggleAllVisible(e: Event) {
    const check = (e.currentTarget as HTMLInputElement).checked;
    if (!data) return;
    if (check) {
      for (const f of data.files) {
        if (!selectedIDs.has(f.id)) toggle({ id: f.id, path: f.path });
      }
    } else {
      for (const f of data.files) {
        if (selectedIDs.has(f.id)) toggle({ id: f.id, path: f.path });
      }
    }
  }

  function toggleFolder(node: TreeNode, select: boolean) {
    const files = collectFiles(node);
    for (const f of files) {
      const has = selectedIDs.has(f.id);
      if (select && !has) toggle({ id: f.id, path: f.path });
      else if (!select && has) toggle({ id: f.id, path: f.path });
    }
  }

  function toggleExpand(path: string) {
    const next = new Set(expanded);
    if (next.has(path)) next.delete(path);
    else next.add(path);
    expanded = next;
  }

  function expandAll() {
    if (!tree) return;
    const next = new Set<string>();
    const stack: TreeNode[] = [tree];
    while (stack.length) {
      const n = stack.pop()!;
      if (n.isFolder) {
        next.add(n.path);
        for (const c of n.children) stack.push(c);
      }
    }
    expanded = next;
  }

  function collapseAll() {
    expanded = new Set(['']);
  }

  async function retryRow(f: FileRow) {
    busy = true;
    try {
      await api.retryFile(f.id);
      await load();
    } catch (e) { err = String(e); }
    finally { busy = false; }
  }

  async function retrySelected() {
    const sel = ids();
    if (sel.length === 0) return;
    busy = true;
    try {
      await api.retryFiles(sel);
      clear();
      await load();
    } catch (e) { err = String(e); }
    finally { busy = false; }
  }

  async function retryAllFailed() {
    busy = true;
    try {
      const res = await api.retryAllFailed();
      await load();
      err = '';
      if (res.affected === 0) err = 'No failed files to retry.';
    } catch (e) { err = String(e); }
    finally { busy = false; }
  }

  async function deleteRow(f: FileRow) {
    if (!confirm(`Delete ${f.path} from the index?\n(The S3 object, if any, stays.)`)) return;
    busy = true;
    try {
      await api.deleteFile(f.id);
      if (detail?.id === f.id) detail = null;
      if (selectedIDs.has(f.id)) toggle({ id: f.id, path: f.path });
      await load();
    } catch (e) { err = String(e); }
    finally { busy = false; }
  }

  async function deleteSelected() {
    const sel = ids();
    if (sel.length === 0) return;
    if (!confirm(`Delete ${sel.length} file(s) from the index?\n(S3 objects stay.)`)) return;
    busy = true;
    try {
      await api.deleteFiles(sel);
      clear();
      await load();
    } catch (e) { err = String(e); }
    finally { busy = false; }
  }

  function restoreSelected() {
    if (paths().length === 0) return;
    go('restore');
  }

  let allVisibleSelected = $derived(
    data !== null && data.files.length > 0 && data.files.every((f) => selectedIDs.has(f.id))
  );
</script>

<h1>Files</h1>
{#if err}<div class="card err">{err}</div>{/if}

<div class="toolbar card">
  <div class="viewswitch" role="tablist" aria-label="View mode">
    <button
      type="button"
      role="tab"
      class:active={viewMode === 'tree'}
      aria-selected={viewMode === 'tree'}
      onclick={() => setView('tree')}
    >Tree</button>
    <button
      type="button"
      role="tab"
      class:active={viewMode === 'flat'}
      aria-selected={viewMode === 'flat'}
      onclick={() => setView('flat')}
    >Flat</button>
  </div>

  <label>
    Status
    <select bind:value={status} onchange={onFilter}>
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
    <input type="text" placeholder="path contains…" bind:value={search} oninput={onSearch} />
  </label>

  {#if viewMode === 'flat'}
    <label>
      Per page
      <select bind:value={limit} onchange={onFilter}>
        <option value={25}>25</option>
        <option value={50}>50</option>
        <option value={100}>100</option>
        <option value={250}>250</option>
      </select>
    </label>
  {/if}

  {#if viewMode === 'tree'}
    <button type="button" onclick={expandAll}>Expand all</button>
    <button type="button" onclick={collapseAll}>Collapse all</button>
  {/if}

  {#if status === 'failed'}
    <button onclick={retryAllFailed} disabled={busy} type="button">Retry all failed</button>
  {/if}
</div>

{#if $selection.length > 0}
  <div class="card selectionbar">
    <span><strong>{$selection.length}</strong> selected</span>
    <button onclick={restoreSelected} disabled={busy} type="button">Restore selected</button>
    <button onclick={retrySelected} disabled={busy} type="button">Retry</button>
    <button onclick={deleteSelected} disabled={busy} type="button" class="danger">Delete</button>
    <button onclick={clear} disabled={busy} type="button">Clear</button>
  </div>
{/if}

{#if viewMode === 'tree'}
  <div class="card nopad">
    {#if tree && tree.children.length > 0}
      {#each tree.children as child (child.path)}
        <FileTreeNode
          node={child}
          depth={0}
          {expanded}
          {selectedIDs}
          onToggleExpand={toggleExpand}
          onToggleFile={(f) => toggle({ id: f.id, path: f.path })}
          onToggleFolder={toggleFolder}
          onOpenDetail={(f) => (detail = f)}
          onRetry={retryRow}
          onDelete={deleteRow}
          {busy}
        />
      {/each}
    {:else if data}
      <div class="muted empty">No files match</div>
    {/if}
  </div>
  {#if data}
    <div class="pager">
      <span class="muted">{data.total.toLocaleString()} files</span>
    </div>
  {/if}
{:else}
  <div class="card nopad">
    <table>
      <thead>
        <tr>
          <th class="check">
            <input type="checkbox" checked={allVisibleSelected} onchange={toggleAllVisible} aria-label="Select all visible" />
          </th>
          <th>Path</th>
          <th>Size</th>
          <th>Modified</th>
          <th>Status</th>
          <th>Zip</th>
          <th>Uploaded</th>
          <th class="actions-col"></th>
        </tr>
      </thead>
      <tbody>
        {#if data}
          {#each data.files as f (f.id)}
            <tr class:picked={selectedIDs.has(f.id)}>
              <td class="check">
                <input
                  type="checkbox"
                  checked={selectedIDs.has(f.id)}
                  onchange={() => toggle({ id: f.id, path: f.path })}
                  aria-label={`Select ${f.path}`}
                />
              </td>
              <td class="mono path" onclick={() => (detail = f)}>{f.path}</td>
              <td class="mono" onclick={() => (detail = f)}>{bytes(f.size)}</td>
              <td onclick={() => (detail = f)}>{formatDate(f.mtime)}</td>
              <td onclick={() => (detail = f)}><StatusBadge status={f.status} /></td>
              <td class="mono muted" onclick={() => (detail = f)}>{f.zip_name ?? ''}</td>
              <td onclick={() => (detail = f)}>{f.uploaded_at ? formatDate(f.uploaded_at) : '—'}</td>
              <td class="actions-col">
                {#if f.status === 'failed' || f.status === 'missing'}
                  <button class="row-action" onclick={(e) => { e.stopPropagation(); retryRow(f); }} disabled={busy} title="Retry">↻</button>
                {/if}
                <button class="row-action danger" onclick={(e) => { e.stopPropagation(); deleteRow(f); }} disabled={busy} title="Delete">×</button>
              </td>
            </tr>
          {/each}
          {#if data.files.length === 0}
            <tr><td colspan="8" class="muted" style="text-align: center; padding: 1.5rem">No files match</td></tr>
          {/if}
        {/if}
      </tbody>
    </table>
  </div>

  <div class="pager">
    <button onclick={() => gotoPage(page - 1)} disabled={page <= 1} type="button">← Prev</button>
    <span class="muted">Page {page} of {totalPages} · {data?.total.toLocaleString() ?? 0} files</span>
    <button onclick={() => gotoPage(page + 1)} disabled={page >= totalPages} type="button">Next →</button>
  </div>
{/if}

{#if detail}
  <div class="card">
    <div class="row"><span class="label">Path</span><span class="mono">{detail.path}</span></div>
    <div class="row"><span class="label">Status</span><StatusBadge status={detail.status} /></div>
    <div class="row"><span class="label">Size</span><span>{bytes(detail.size)} ({detail.size.toLocaleString()} B)</span></div>
    <div class="row"><span class="label">Modified</span><span>{formatDate(detail.mtime)}</span></div>
    <div class="row"><span class="label">Last seen</span><span>{formatDate(detail.last_seen_at)}</span></div>
    {#if detail.md5}<div class="row"><span class="label">MD5</span><span class="mono">{detail.md5}</span></div>{/if}
    {#if detail.s3_key}<div class="row"><span class="label">S3 key</span><span class="mono">{detail.s3_key}</span></div>{/if}
    {#if detail.zip_name}<div class="row"><span class="label">Zip</span><span class="mono">{detail.zip_name}</span></div>{/if}
    {#if detail.uploaded_at}<div class="row"><span class="label">Uploaded at</span><span>{formatDate(detail.uploaded_at)}</span></div>{/if}
    <button onclick={() => (detail = null)} type="button" style="margin-top: 0.5rem">Close</button>
  </div>
{/if}

<style>
  .toolbar { display: flex; gap: 1rem; align-items: flex-end; flex-wrap: wrap; }
  .toolbar label { display: grid; gap: 0.25rem; font-size: 0.85rem; color: var(--muted); }
  .toolbar .grow { flex: 1; min-width: 200px; }
  .nopad { padding: 0; overflow: hidden; }
  .path { max-width: 480px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; cursor: pointer; }
  td { cursor: default; }
  .check { width: 36px; text-align: center; }
  .actions-col { width: 80px; text-align: right; }
  tbody tr:hover { background: rgba(255, 255, 255, 0.03); }
  tbody tr.picked { background: rgba(76, 139, 245, 0.08); }
  .pager { display: flex; gap: 1rem; align-items: center; justify-content: center; }
  .row { display: grid; grid-template-columns: 120px 1fr; gap: 0.5rem; padding: 0.25rem 0; }
  .label { color: var(--muted); font-size: 0.85rem; }
  .err { color: var(--err); border-color: var(--err); }
  .selectionbar { display: flex; gap: 0.5rem; align-items: center; flex-wrap: wrap; }
  .row-action {
    padding: 0.2rem 0.5rem;
    font-size: 0.9rem;
    line-height: 1;
    margin-left: 0.25rem;
  }
  .row-action.danger, button.danger { border-color: var(--err); color: var(--err); }
  .row-action.danger:hover:not(:disabled), button.danger:hover:not(:disabled) {
    background: rgba(239, 80, 80, 0.1);
  }
  .viewswitch {
    display: inline-flex;
    border: 1px solid var(--border);
    border-radius: 4px;
    overflow: hidden;
  }
  .viewswitch button {
    border: none;
    border-radius: 0;
    background: transparent;
    padding: 0.35rem 0.9rem;
    font-size: 0.85rem;
    color: var(--muted);
  }
  .viewswitch button.active {
    background: var(--accent);
    color: var(--bg);
  }
  .viewswitch button + button { border-left: 1px solid var(--border); }
  .empty { padding: 1.5rem; text-align: center; }
</style>
