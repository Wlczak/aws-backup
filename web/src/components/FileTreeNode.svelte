<script lang="ts">
  import type { TreeNode } from '../lib/tree';
  import { collectFiles } from '../lib/tree';
  import type { FileRow } from '../lib/api';
  import { bytes, formatDate, restoreLabel, expiresIn } from '../lib/format';
  import StatusBadge from './StatusBadge.svelte';
  import Self from './FileTreeNode.svelte';

  type Props = {
    node: TreeNode;
    depth: number;
    expanded: Set<string>;
    selectedIDs: Set<number>;
    onToggleExpand: (path: string) => void;
    onToggleFile: (f: FileRow) => void;
    onToggleFolder: (folder: TreeNode, select: boolean) => void;
    onOpenDetail: (f: FileRow) => void;
    onRetry: (f: FileRow) => void;
    onDelete: (f: FileRow) => void;
    busy: boolean;
  };

  let {
    node, depth, expanded, selectedIDs,
    onToggleExpand, onToggleFile, onToggleFolder,
    onOpenDetail, onRetry, onDelete, busy,
  }: Props = $props();

  // Folder selection state: 0 = none, 1 = partial, 2 = all.
  let folderState = $derived.by(() => {
    if (!node.isFolder) return 0;
    const all = collectFiles(node);
    if (all.length === 0) return 0;
    let sel = 0;
    for (const f of all) if (selectedIDs.has(f.id)) sel++;
    if (sel === 0) return 0;
    if (sel === all.length) return 2;
    return 1;
  });

  let isOpen = $derived(node.isFolder && expanded.has(node.path));

  function folderCheckbox(el: HTMLInputElement | null) {
    if (!el) return;
    el.indeterminate = folderState === 1;
  }

  function onFolderChange(e: Event) {
    const check = (e.currentTarget as HTMLInputElement).checked;
    onToggleFolder(node, check);
  }
</script>

{#if node.isFolder}
  <div class="row folder" style="padding-left: {depth * 18 + 8}px">
    <input
      type="checkbox"
      class="check"
      checked={folderState === 2}
      use:folderCheckbox
      onchange={onFolderChange}
      aria-label={`Select folder ${node.path}`}
    />
    <button
      type="button"
      class="twisty"
      onclick={() => onToggleExpand(node.path)}
      aria-label={isOpen ? 'Collapse' : 'Expand'}
    >{isOpen ? '▾' : '▸'}</button>
    <span class="icon" aria-hidden="true">📁</span>
    <button type="button" class="name linkish folder-name" onclick={() => onToggleExpand(node.path)}>{node.name || '/'}</button>
    <span class="meta muted mono">{node.fileCount} · {bytes(node.totalSize)}</span>
  </div>
  {#if isOpen}
    {#each node.children as child (child.path)}
      <Self
        node={child}
        depth={depth + 1}
        {expanded}
        {selectedIDs}
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
{:else if node.file}
  {@const f = node.file}
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
    <button type="button" class="name mono clickable linkish" onclick={() => onOpenDetail(f)}>{node.name}</button>
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
</style>
