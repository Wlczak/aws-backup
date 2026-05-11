<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api, type FilesPage, type FileRow, type TreeFolderInfo } from '../lib/api';
  import { bytes, formatDate, restoreLabel, expiresIn } from '../lib/format';
  import { selection, toggle, clear, ids, paths } from '../lib/selection';
  import { go } from '../lib/router';
  import { toast } from '../lib/toast';
  import StatusBadge from '../components/StatusBadge.svelte';
  import FileTreeNode, { type FolderChildren } from '../components/FileTreeNode.svelte';

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
  let detail = $state<FileRow | null>(null);
  let busy = $state(false);
  let expanded = $state<Set<string>>(new Set());

  // Lazy tree state. The cache is keyed by folder path; the root level
  // lives under '' and is fetched on mount + on filter change. Each
  // folder's children are fetched only when the user expands it. (#tree-lazy)
  let treeCache = $state<Record<string, FolderChildren>>({});
  // Selected folder paths whose descendant ids haven't been resolved yet
  // (e.g. user toggled the folder before its children loaded). Tracked
  // separately so the folder checkbox shows "all" without needing the ids.
  let selectedFolderPaths = $state<Set<string>>(new Set());

  let totalPages = $derived(
    data && viewMode === 'flat' ? Math.max(1, Math.ceil(data.total / limit)) : 1,
  );
  let selectedIDs = $derived(new Set($selection.map((f) => f.id)));

  let searchTimer: number | undefined;
  let aborted = false;
  let inflight: AbortController | null = null;

  async function loadFlat() {
    if (inflight) inflight.abort();
    const ctl = new AbortController();
    inflight = ctl;
    try {
      const next = await api.files(
        { page, limit, status: status || undefined, search: search || undefined },
        ctl.signal,
      );
      if (aborted || ctl.signal.aborted) return;
      data = next;
    } catch (e) {
      if (aborted || ctl.signal.aborted) return;
      toast.error(String(e));
    }
  }

  // Load the children at one prefix into the tree cache. Subsequent
  // identical requests are no-ops while the first is in flight.
  async function loadTreeNode(prefix: string) {
    const cur = treeCache[prefix];
    if (cur && (cur.state === 'loading' || cur.state === 'loaded')) return;
    treeCache = { ...treeCache, [prefix]: { state: 'loading' } };
    try {
      const res = await api.filesTree({ prefix: prefix || undefined, status: status || undefined });
      if (aborted) return;
      treeCache = {
        ...treeCache,
        [prefix]: { state: 'loaded', folders: res.folders, files: res.files },
      };
      // If the user pre-selected this folder before it loaded, fan the
      // selection out to the now-known descendants.
      if (selectedFolderPaths.has(prefix)) {
        await fanOutFolderSelection(prefix, true, /*alreadyTracked*/ true);
      }
    } catch (e) {
      if (aborted) return;
      treeCache = { ...treeCache, [prefix]: { state: 'error', error: String(e) } };
    }
  }

  // Reset the lazy-tree state and reload root. Called on filter change
  // and on initial mount.
  async function reloadTreeRoot() {
    treeCache = {};
    expanded = new Set();
    await loadTreeNode('');
  }

  async function load() {
    if (viewMode === 'tree') {
      await reloadTreeRoot();
    } else {
      await loadFlat();
    }
  }

  onMount(load);
  onDestroy(() => {
    aborted = true;
    if (inflight) inflight.abort();
    if (searchTimer) clearTimeout(searchTimer);
  });

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
    searchTimer = window.setTimeout(() => {
      page = 1;
      // Search is only wired through the flat list; flip mode if the
      // user types into the search box while in tree view.
      if (search && viewMode === 'tree') setView('flat');
      else load();
    }, 250);
  }
  function gotoPage(p: number) {
    if (p < 1 || p > totalPages) return;
    page = p;
    loadFlat();
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

  // Resolve a folder selection toggle into per-file selection updates.
  // The descendants of a folder may not be in the tree cache yet, so we
  // ask the server for the full ID list. Used by the folder checkbox.
  async function fanOutFolderSelection(prefix: string, select: boolean, alreadyTracked = false) {
    if (!alreadyTracked) {
      const next = new Set(selectedFolderPaths);
      if (select) next.add(prefix);
      else next.delete(prefix);
      selectedFolderPaths = next;
    }
    try {
      const res = await api.filesSubtreeIDs({ prefix, status: status || undefined });
      if (res.truncated) {
        toast.info(`Folder has ${res.total.toLocaleString()} files; selected the first ${res.ids.length.toLocaleString()}.`);
      }
      for (let i = 0; i < res.ids.length; i++) {
        const id = res.ids[i];
        const path = res.paths[i];
        const has = selectedIDs.has(id);
        if (select && !has) toggle({ id, path });
        else if (!select && has) toggle({ id, path });
      }
      // Selection store is the source of truth from here on; clear our
      // pending marker now that it's hydrated.
      if (!select) {
        const next = new Set(selectedFolderPaths);
        next.delete(prefix);
        selectedFolderPaths = next;
      }
    } catch (e) {
      toast.error(String(e));
    }
  }

  function onToggleFolder(folder: TreeFolderInfo, select: boolean) {
    fanOutFolderSelection(folder.path, select);
  }

  // 0 = none selected, 1 = some, 2 = all. Computed from cached
  // descendants; if the cache doesn't yet contain a folder we trust the
  // pending-selection marker so the checkbox renders consistently.
  function folderSelectionState(folderPath: string): 0 | 1 | 2 {
    if (selectedFolderPaths.has(folderPath)) return 2;
    const stack = [folderPath];
    let total = 0, sel = 0;
    while (stack.length) {
      const p = stack.pop()!;
      const entry = treeCache[p];
      if (!entry || entry.state !== 'loaded') continue;
      for (const fd of entry.folders) stack.push(fd.path);
      for (const f of entry.files) {
        total++;
        if (selectedIDs.has(f.id)) sel++;
      }
    }
    if (total === 0 || sel === 0) return 0;
    if (sel === total) return 2;
    return 1;
  }

  async function toggleExpand(path: string) {
    const next = new Set(expanded);
    if (next.has(path)) {
      next.delete(path);
    } else {
      next.add(path);
      // Lazy fetch: only fire the request the first time this folder
      // opens. Subsequent expand toggles read from the cache.
      if (!treeCache[path] || treeCache[path].state === 'error') {
        loadTreeNode(path);
      }
    }
    expanded = next;
  }

  async function retryRow(f: FileRow) {
    busy = true;
    try {
      await api.retryFile(f.id);
      await invalidateRowParent(f.path);
    } catch (e) { toast.error(String(e)); }
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
    } catch (e) { toast.error(String(e)); }
    finally { busy = false; }
  }

  async function retryAllFailed() {
    busy = true;
    try {
      const res = await api.retryAllFailed();
      await load();
      if (res.affected === 0) toast.info('No failed files to retry.');
    } catch (e) { toast.error(String(e)); }
    finally { busy = false; }
  }

  async function deleteRow(f: FileRow) {
    if (!confirm(`Delete ${f.path} from the index?\n(The S3 object, if any, stays.)`)) return;
    busy = true;
    try {
      await api.deleteFile(f.id);
      if (detail?.id === f.id) detail = null;
      if (selectedIDs.has(f.id)) toggle({ id: f.id, path: f.path });
      await invalidateRowParent(f.path);
    } catch (e) { toast.error(String(e)); }
    finally { busy = false; }
  }

  // After a single-row mutation, re-fetch only the parent folder so the
  // tree reflects the change without a full reload. Falls back to a
  // global reload in flat mode.
  async function invalidateRowParent(path: string) {
    if (viewMode !== 'tree') {
      await loadFlat();
      return;
    }
    const parts = path.split(/[\\/]+/).filter((s: string) => s.length > 0);
    parts.pop();
    const parent = parts.join('/');
    delete treeCache[parent];
    treeCache = { ...treeCache };
    await loadTreeNode(parent);
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
    } catch (e) { toast.error(String(e)); }
    finally { busy = false; }
  }

  function restoreSelected() {
    if (paths().length === 0) return;
    go('restore');
  }

  async function rescanSelected() {
    const sel = paths();
    if (sel.length === 0) return;
    busy = true;
    try {
      await api.triggerRun({ mode: 'scan', paths: sel });
    } catch (e) { toast.error(String(e)); }
    finally { busy = false; }
  }

  let allVisibleSelected = $derived(
    data !== null && data.files.length > 0 && data.files.every((f) => selectedIDs.has(f.id))
  );

  let rootKids = $derived<FolderChildren>(treeCache[''] ?? { state: 'idle' });
</script>

<h1>Files</h1>

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
    <input
      type="text"
      placeholder={viewMode === 'tree' ? 'path contains… (switches to flat view)' : 'path contains…'}
      bind:value={search}
      oninput={onSearch}
    />
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

  {#if status === 'failed'}
    <button onclick={retryAllFailed} disabled={busy} type="button">Retry all failed</button>
  {/if}
</div>

{#if $selection.length > 0}
  <div class="card selectionbar">
    <span><strong>{$selection.length}</strong> selected</span>
    <button onclick={restoreSelected} disabled={busy} type="button">Restore selected</button>
    <button onclick={rescanSelected} disabled={busy} type="button" title="Re-scan selected paths for changes">Rescan</button>
    <button onclick={retrySelected} disabled={busy} type="button">Retry</button>
    <button onclick={deleteSelected} disabled={busy} type="button" class="danger">Delete</button>
    <button onclick={clear} disabled={busy} type="button">Clear</button>
  </div>
{/if}

{#if viewMode === 'tree'}
  <div class="card nopad">
    {#if rootKids.state === 'loading'}
      <div class="muted empty">Loading…</div>
    {:else if rootKids.state === 'error'}
      <div class="empty err">{rootKids.error}</div>
    {:else if rootKids.state === 'loaded'}
      {#if rootKids.folders.length === 0 && rootKids.files.length === 0}
        <div class="muted empty">No files</div>
      {/if}
      {#each rootKids.folders as fd (fd.path)}
        <FileTreeNode
          folder={fd}
          depth={0}
          {expanded}
          {selectedIDs}
          cache={treeCache}
          {folderSelectionState}
          onToggleExpand={toggleExpand}
          onToggleFile={(f) => toggle({ id: f.id, path: f.path })}
          onToggleFolder={onToggleFolder}
          onOpenDetail={(f) => (detail = f)}
          onRetry={retryRow}
          onDelete={deleteRow}
          {busy}
        />
      {/each}
      {#each rootKids.files as f (f.id)}
        <FileTreeNode
          file={f}
          depth={0}
          {expanded}
          {selectedIDs}
          cache={treeCache}
          {folderSelectionState}
          onToggleExpand={toggleExpand}
          onToggleFile={(ff) => toggle({ id: ff.id, path: ff.path })}
          onToggleFolder={onToggleFolder}
          onOpenDetail={(ff) => (detail = ff)}
          onRetry={retryRow}
          onDelete={deleteRow}
          {busy}
        />
      {/each}
    {/if}
  </div>
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
          <th>Restore</th>
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
              <td
                class="mono path"
                role="button"
                tabindex="0"
                aria-label={`Open details for ${f.path}`}
                onclick={() => (detail = f)}
                onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); detail = f; } }}
              >{f.path}</td>
              <td class="mono" onclick={() => (detail = f)}>{bytes(f.size)}</td>
              <td onclick={() => (detail = f)}>{formatDate(f.mtime)}</td>
              <td onclick={() => (detail = f)}><StatusBadge status={f.status} s3Key={f.s3_key} /></td>
              <td onclick={() => (detail = f)}>
                {#if f.restore_status}
                  <StatusBadge status={restoreLabel(f.restore_status)} />
                  {#if f.restore_status === 'restored' && f.restore_expires_at}
                    <span class="muted small">· expires {expiresIn(f.restore_expires_at)}</span>
                  {/if}
                {/if}
              </td>
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
            <tr><td colspan="9" class="muted" style="text-align: center; padding: 1.5rem">No files match</td></tr>
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
    <div class="row"><span class="label">Status</span><StatusBadge status={detail.status} s3Key={detail.s3_key} /></div>
    <div class="row"><span class="label">Size</span><span>{bytes(detail.size)} ({detail.size.toLocaleString()} B)</span></div>
    <div class="row"><span class="label">Modified</span><span>{formatDate(detail.mtime)}</span></div>
    <div class="row"><span class="label">Last seen</span><span>{formatDate(detail.last_seen_at)}</span></div>
    {#if detail.md5}<div class="row"><span class="label">MD5</span><span class="mono">{detail.md5}</span></div>{/if}
    {#if detail.s3_key}<div class="row"><span class="label">S3 key</span><span class="mono">{detail.s3_key}</span></div>{/if}
    {#if detail.zip_name}<div class="row"><span class="label">Zip</span><span class="mono">{detail.zip_name}</span></div>{/if}
    {#if detail.uploaded_at}<div class="row"><span class="label">Uploaded at</span><span>{formatDate(detail.uploaded_at)}</span></div>{/if}
    {#if detail.restore_status}
      <div class="row">
        <span class="label">Restore</span>
        <span>
          <StatusBadge status={restoreLabel(detail.restore_status)} />
          {#if detail.restore_status === 'restored' && detail.restore_expires_at}
            <span class="muted">expires {formatDate(detail.restore_expires_at)} ({expiresIn(detail.restore_expires_at)})</span>
          {/if}
        </span>
      </div>
    {/if}
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
  .empty.err { color: var(--err); }
  .small { font-size: 0.78rem; margin-left: 0.3rem; }
</style>
