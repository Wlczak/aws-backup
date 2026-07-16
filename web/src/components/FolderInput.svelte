<script lang="ts">
  import { api, type FolderEntry } from '../lib/api';
  import { toast } from '../lib/toast';

  type Props = {
    id: string;
    value?: string;
    placeholder?: string;
    ariaLabel?: string;
  };

  let {
    id,
    value = $bindable(''),
    placeholder = '',
    ariaLabel = 'Folder path',
  }: Props = $props();

  let dialog: HTMLDialogElement;
  let currentPath = $state('');
  let parentPath = $state('');
  let roots = $state<FolderEntry[]>([]);
  let folders = $state<FolderEntry[]>([]);
  let loading = $state(false);
  let creating = $state(false);
  let newFolderName = $state('');

  async function load(path?: string): Promise<boolean> {
    loading = true;
    try {
      const result = await api.folders(path);
      currentPath = result.path;
      parentPath = result.parent ?? '';
      roots = result.roots;
      folders = result.folders;
      return true;
    } catch (e) {
      toast.error(`Could not open folder: ${e}`);
      return false;
    } finally {
      loading = false;
    }
  }

  async function openPicker() {
    newFolderName = '';
    dialog.showModal();
    const requested = value.trim();
    if (requested && await load(requested)) return;
    await load();
  }

  function selectCurrent() {
    if (!currentPath) return;
    value = currentPath;
    dialog.close();
  }

  async function createFolder() {
    const name = newFolderName.trim();
    if (!currentPath || !name || creating) return;
    creating = true;
    try {
      const created = await api.createFolder(currentPath, name);
      newFolderName = '';
      toast.success(`Created ${created.path}`);
      await load(created.path);
    } catch (e) {
      toast.error(`Could not create folder: ${e}`);
    } finally {
      creating = false;
    }
  }
</script>

<div class="folder-input">
  <input {id} type="text" bind:value {placeholder} aria-label={ariaLabel} />
  <button type="button" onclick={() => void openPicker()}>Browse</button>
</div>

<dialog bind:this={dialog} aria-labelledby={`${id}-picker-title`}>
  <div class="picker">
    <div class="picker-head">
      <div>
        <div class="eyebrow">Local filesystem</div>
        <h2 id={`${id}-picker-title`}>Choose a folder</h2>
      </div>
      <button class="close" type="button" onclick={() => dialog.close()} aria-label="Close folder picker">×</button>
    </div>

    <div class="location">
      <button type="button" onclick={() => void load(parentPath)} disabled={!parentPath || loading} aria-label="Open parent folder">↑</button>
      <div class="current mono" title={currentPath}>{currentPath || 'Loading…'}</div>
    </div>

    {#if roots.length > 0}
      <div class="roots" aria-label="Filesystem roots">
        {#each roots as root (root.path)}
          <button
            type="button"
            class:active={root.path === currentPath}
            onclick={() => void load(root.path)}
            disabled={loading}
          >{root.name}</button>
        {/each}
      </div>
    {/if}

    <div class="folder-list" aria-live="polite" aria-busy={loading}>
      {#if loading}
        <div class="empty">Loading folders…</div>
      {:else if folders.length === 0}
        <div class="empty">This folder has no subdirectories.</div>
      {:else}
        {#each folders as folder (folder.path)}
          <button class="folder" type="button" onclick={() => void load(folder.path)}>
            <span aria-hidden="true">▸</span>
            <span>{folder.name}</span>
          </button>
        {/each}
      {/if}
    </div>

    <div class="create-row">
      <input
        type="text"
        bind:value={newFolderName}
        placeholder="New folder name"
        aria-label="New folder name"
        disabled={!currentPath || creating}
        onkeydown={(e) => { if (e.key === 'Enter') void createFolder(); }}
      />
      <button type="button" onclick={() => void createFolder()} disabled={!currentPath || !newFolderName.trim() || creating}>
        {creating ? 'Creating…' : 'Create folder'}
      </button>
    </div>

    <div class="picker-actions">
      <button type="button" onclick={() => dialog.close()}>Cancel</button>
      <button class="primary" type="button" onclick={selectCurrent} disabled={!currentPath || loading}>Select this folder</button>
    </div>
  </div>
</dialog>

<style>
  .folder-input {
    display: flex;
    flex: 1 1 320px;
    min-width: 0;
    gap: 0.45rem;
  }
  .folder-input input,
  .create-row input {
    min-width: 0;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--bg);
    color: var(--text);
    font: inherit;
    padding: 0.4rem 0.55rem;
  }
  .folder-input input { flex: 1; }
  dialog {
    width: min(680px, calc(100vw - 2rem));
    max-height: min(760px, calc(100vh - 2rem));
    padding: 0;
    color: var(--text);
    background: #151820;
    border: 1px solid var(--border);
    border-radius: 10px;
    box-shadow: 0 24px 70px rgba(0, 0, 0, 0.55);
  }
  dialog::backdrop {
    background: rgba(4, 6, 10, 0.76);
    backdrop-filter: blur(3px);
  }
  .picker { padding: 1rem; }
  .picker-head,
  .location,
  .create-row,
  .picker-actions {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }
  .picker-head { justify-content: space-between; }
  .picker-head h2 { margin: 0.1rem 0 0; }
  .eyebrow {
    color: var(--accent);
    font-size: 0.7rem;
    font-weight: 700;
    letter-spacing: 0.12em;
    text-transform: uppercase;
  }
  .close { border: 0; font-size: 1.4rem; }
  .location {
    margin-top: 0.9rem;
    padding: 0.55rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--bg);
  }
  .current {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .roots { display: flex; flex-wrap: wrap; gap: 0.4rem; margin-top: 0.65rem; }
  .roots button.active { border-color: var(--accent); color: var(--accent); }
  .folder-list {
    min-height: 260px;
    max-height: min(45vh, 420px);
    overflow: auto;
    display: grid;
    align-content: start;
    gap: 0.25rem;
    margin-top: 0.75rem;
    padding: 0.4rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--bg);
  }
  .folder {
    display: flex;
    gap: 0.55rem;
    width: 100%;
    text-align: left;
    border-color: transparent;
    background: transparent;
  }
  .folder:hover { background: rgba(76, 139, 245, 0.09); }
  .folder span:first-child { color: var(--accent); }
  .empty { margin: auto; color: var(--muted); font-size: 0.9rem; }
  .create-row { margin-top: 0.75rem; }
  .create-row input { flex: 1; }
  .picker-actions { justify-content: flex-end; margin-top: 0.9rem; }
  @media (max-width: 560px) {
    .folder-input { flex-basis: 100%; }
    .folder-list { min-height: 210px; }
    .create-row { align-items: stretch; flex-direction: column; }
    .picker-actions button { flex: 1; }
  }
</style>
