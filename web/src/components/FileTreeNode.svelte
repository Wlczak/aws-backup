<script lang="ts">
  import type { FileRow, TreeFolderInfo } from '../lib/api';
  import { bytes, formatDate, restoreLabel, expiresIn } from '../lib/format';
  import StatusBadge from './StatusBadge.svelte';
  import Self from './FileTreeNode.svelte';

  // Lazy-loaded children for one folder. Each folder only fetches when
  // the user expands it; the parent owns the cache and passes the
  // matching slot in via `children`.
  export type FolderChildren =
    | { state: 'idle' }
    | { state: 'loading' }
    | { state: 'loaded'; folders: TreeFolderInfo[]; files: FileRow[] }
    | { state: 'error'; error: string };

  type Props = {
    // Either folder OR file is set per node — never both.
    folder?: TreeFolderInfo;
    file?: FileRow;
    depth: number;
    expanded: Set<string>;
    selectedIDs: Set<number>;
    cache: Record<string, FolderChildren>;
    folderSelectionState: (path: string) => 0 | 1 | 2;
    onToggleExpand: (path: string) => void;
    onToggleFile: (f: FileRow) => void;
    onToggleFolder: (folder: TreeFolderInfo, select: boolean) => void;
    onOpenDetail: (f: FileRow) => void;
    onRetry: (f: FileRow) => void;
    onDelete: (f: FileRow) => void;
    busy: boolean;
  };

  let {
    folder, file, depth, expanded, selectedIDs, cache,
    folderSelectionState,
    onToggleExpand, onToggleFile, onToggleFolder,
    onOpenDetail, onRetry, onDelete, busy,
  }: Props = $props();

  let isFolder = $derived(folder !== undefined);
  let isOpen = $derived(isFolder && folder !== undefined && expanded.has(folder.path));
  let folderState = $derived(folder ? folderSelectionState(folder.path) : 0);
  let kids = $derived<FolderChildren>(folder ? (cache[folder.path] ?? { state: 'idle' }) : { state: 'idle' });

  function folderCheckbox(el: HTMLInputElement | null) {
    if (!el) return;
    el.indeterminate = folderState === 1;
  }

  function onFolderChange(e: Event) {
    if (!folder) return;
    const check = (e.currentTarget as HTMLInputElement).checked;
    onToggleFolder(folder, check);
  }
</script>

{#if folder}
  <div class="row folder" style="padding-left: {depth * 18 + 8}px">
    <input
      type="checkbox"
      class="check"
      checked={folderState === 2}
      use:folderCheckbox
      onchange={onFolderChange}
      aria-label={`Select folder ${folder.path}`}
    />
    <button
      type="button"
      class="twisty"
      onclick={() => onToggleExpand(folder!.path)}
      aria-label={isOpen ? 'Collapse' : 'Expand'}
    >{isOpen ? '▾' : '▸'}</button>
    <span class="icon" aria-hidden="true">📁</span>
    <button type="button" class="name linkish folder-name" onclick={() => onToggleExpand(folder!.path)}>{folder.name || '/'}</button>
    <span class="meta muted mono">{folder.file_count} · {bytes(folder.total_size)}</span>
  </div>
  {#if isOpen}
    {#if kids.state === 'loading'}
      <div class="row child-status muted" style="padding-left: {(depth + 1) * 18 + 8}px">Loading…</div>
    {:else if kids.state === 'error'}
      <div class="row child-status err" style="padding-left: {(depth + 1) * 18 + 8}px">{kids.error}</div>
    {:else if kids.state === 'loaded'}
      {#if kids.folders.length === 0 && kids.files.length === 0}
        <div class="row child-status muted" style="padding-left: {(depth + 1) * 18 + 8}px">empty</div>
      {/if}
      {#each kids.folders as sub (sub.path)}
        <Self
          folder={sub}
          depth={depth + 1}
          {expanded}
          {selectedIDs}
          {cache}
          {folderSelectionState}
          {onToggleExpand}
          {onToggleFile}
          {onToggleFolder}
          {onOpenDetail}
          {onRetry}
          {onDelete}
          {busy}
        />
      {/each}
      {#each kids.files as f (f.id)}
        <Self
          file={f}
          depth={depth + 1}
          {expanded}
          {selectedIDs}
          {cache}
          {folderSelectionState}
          {onToggleExpand}
          {onToggleFile}
          {onToggleFolder}
          {onOpenDetail}
          {onRetry}
          {onDelete}
          {busy}
        />
      {/each}
    {/if}
  {/if}
{:else if file}
  {@const f = file}
  {@const fname = f.path.split(/[\\/]+/).filter((s: string) => s.length > 0).pop() ?? f.path}
  <div class="row file" class:picked={selectedIDs.has(f.id)} style="padding-left: {depth * 18 + 8}px">
    <input
      type="checkbox"
      class="check"
      checked={selectedIDs.has(f.id)}
      onchange={() => onToggleFile(f)}
      aria-label={`Select ${f.path}`}
    />
    <span class="twisty-spacer"></span>
    <span class="icon" aria-hidden="true">📄</span>
    <button type="button" class="name mono clickable linkish" onclick={() => onOpenDetail(f)}>{fname}</button>
    <span class="meta mono muted">{bytes(f.size)}</span>
    <span class="meta muted">{formatDate(f.mtime)}</span>
    <StatusBadge status={f.status} />
    {#if f.restore_status}
      <StatusBadge status={restoreLabel(f.restore_status)} />
      {#if f.restore_status === 'restored' && f.restore_expires_at}
        <span class="meta muted small" title={f.restore_expires_at}>{expiresIn(f.restore_expires_at)}</span>
      {/if}
    {/if}
    <span class="actions">
      {#if f.status === 'failed' || f.status === 'missing'}
        <button
          class="row-action"
          type="button"
          onclick={(e) => { e.stopPropagation(); onRetry(f); }}
          disabled={busy}
          title="Retry"
        >↻</button>
      {/if}
      <button
        class="row-action danger"
        type="button"
        onclick={(e) => { e.stopPropagation(); onDelete(f); }}
        disabled={busy}
        title="Delete"
      >×</button>
    </span>
  </div>
{/if}

<style>
  .row {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.3rem 0.5rem;
    border-bottom: 1px solid rgba(255, 255, 255, 0.04);
    min-height: 32px;
  }
  .row:hover { background: rgba(255, 255, 255, 0.03); }
  .row.picked { background: rgba(76, 139, 245, 0.08); }
  .check { width: 16px; height: 16px; flex-shrink: 0; }
  .twisty {
    width: 18px; height: 18px; padding: 0;
    background: transparent; border: none;
    color: var(--muted); cursor: pointer;
    font-size: 0.8rem; line-height: 1;
    flex-shrink: 0;
  }
  .twisty-spacer { width: 18px; flex-shrink: 0; }
  .twisty:hover { color: var(--fg); }
  .icon { flex-shrink: 0; font-size: 0.95rem; }
  .name { flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .folder .name { cursor: pointer; font-weight: 500; }
  .clickable { cursor: pointer; }
  .linkish {
    background: transparent;
    border: none;
    padding: 0;
    margin: 0;
    color: inherit;
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .linkish:hover { color: var(--accent); }
  .folder-name { font-weight: 500; }
  .meta { font-size: 0.8rem; flex-shrink: 0; }
  .actions { display: flex; gap: 0.25rem; flex-shrink: 0; }
  .row-action { padding: 0.15rem 0.4rem; font-size: 0.85rem; line-height: 1; }
  .row-action.danger { border-color: var(--err); color: var(--err); }
  .row-action.danger:hover:not(:disabled) { background: rgba(239, 80, 80, 0.1); }
  .small { font-size: 0.7rem; }
  .child-status { font-style: italic; }
  .child-status.err { color: var(--err); }
</style>
