<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api, subscribeEvents, type Status, type FileStats } from '../lib/api';
  import { bytes, formatDate, relativeTime } from '../lib/format';
  import StatusBadge from '../components/StatusBadge.svelte';
  import ProgressBar from '../components/ProgressBar.svelte';

  let status = $state<Status | null>(null);
  let stats = $state<FileStats | null>(null);
  let logLines = $state<string[]>([]);
  let scanSeen = $state(0);
  let uploads = $state({ completed: 0, failed: 0, started: 0 });
  let triggering = $state(false);
  let err = $state('');

  let es: { close: () => void } | null = null;
  let pollTimer: number | undefined;

  async function refresh() {
    try {
      status = await api.status();
      stats = await api.fileStats();
      err = '';
    } catch (e) {
      err = String(e);
    }
  }

  onMount(() => {
    refresh();
    pollTimer = window.setInterval(refresh, 3000);
    es = subscribeEvents((type, data) => {
      const d = data as any;
      if (type === 'scan_complete') scanSeen = d?.data?.seen ?? 0;
      if (type === 'upload_start') uploads.started++;
      if (type === 'upload_complete') uploads.completed++;
      if (type === 'upload_failed') uploads.failed++;
      const line = `[${type}] ${JSON.stringify(d.data ?? d)}`;
      logLines = [...logLines.slice(-49), line];
      if (type === 'run_complete' || type === 'run_start') refresh();
    });
  });

  onDestroy(() => {
    es?.close();
    if (pollTimer) clearInterval(pollTimer);
  });

  async function triggerRun(mode: 'full' | 'scan' | 'upload' = 'full') {
    triggering = true;
    err = '';
    try {
      await api.triggerRun({ mode });
      uploads = { completed: 0, failed: 0, started: 0 };
      scanSeen = 0;
      logLines = [];
    } catch (e) {
      err = String(e);
    } finally {
      triggering = false;
    }
  }

  async function cancel() {
    if (!status?.current) return;
    try {
      await api.cancelRun(status.current.id);
    } catch (e) {
      err = String(e);
    }
  }

  let syncing = $state(false);
  let syncInfo = $state('');
  let fullSyncResult = $state<{
    local_missing_count: number;
    local_missing_from_cloud?: string[];
    cloud_missing_count: number;
    cloud_missing_from_local?: string[];
    cloud_file_count: number;
    local_file_count: number;
    zip_indexes_consumed: number;
  } | null>(null);

  let purging = $state(false);
  async function purgeMissing() {
    purging = true;
    err = '';
    try {
      const r = await api.purgeMissing();
      await refresh();
      if (r.affected === 0) err = 'No missing-status entries found.';
    } catch (e) {
      err = String(e);
    } finally {
      purging = false;
    }
  }

  // --- fix-action state ---
  let backingUp = $state(false);
  let backupStarted = $state(false);
  let backupErr = $state('');

  let showRestoreForm = $state(false);
  let restoreTargetDir = $state('');
  let restoring = $state(false);
  let restoreResult = $state<{ files_written: number; bytes_written: number; skipped?: string[]; errors?: string[] } | null>(null);
  let restoreErr = $state('');

  let showDeleteConfirm = $state(false);
  let deleting = $state(false);
  let deleteResult = $state<{ deleted_standalone: number; deleted_zips: number; skipped_partial_zip: number; errors?: string[] } | null>(null);
  let deleteErr = $state('');

  function resetFixState() {
    backupStarted = false; backupErr = '';
    showRestoreForm = false; restoreResult = null; restoreErr = '';
    showDeleteConfirm = false; deleteResult = null; deleteErr = '';
  }

  async function backUpMissing() {
    if (!fullSyncResult?.local_missing_from_cloud?.length) return;
    backingUp = true; backupStarted = false; backupErr = '';
    try {
      // Force-reset these paths to pending regardless of their current DB
      // status — files may be marked uploaded/zipped even though they're
      // absent from the cloud index (e.g. zip exists but has no .index.txt).
      await api.retryByPaths(fullSyncResult.local_missing_from_cloud);
      await api.triggerRun({ mode: 'upload' });
      backupStarted = true;
      await refresh();
    } catch (e) {
      backupErr = String(e);
    } finally {
      backingUp = false;
    }
  }

  async function restoreMissing() {
    if (!restoreTargetDir || !fullSyncResult?.cloud_missing_from_local?.length) return;
    restoring = true; restoreResult = null; restoreErr = '';
    try {
      restoreResult = await api.restoreTrigger(fullSyncResult.cloud_missing_from_local, restoreTargetDir);
    } catch (e) {
      restoreErr = String(e);
    } finally {
      restoring = false;
    }
  }

  async function deleteCloudMissing() {
    if (!fullSyncResult?.cloud_missing_from_local?.length) return;
    deleting = true; deleteResult = null; deleteErr = '';
    try {
      deleteResult = await api.deleteCloudPaths(fullSyncResult.cloud_missing_from_local);
    } catch (e) {
      deleteErr = String(e);
    } finally {
      deleting = false;
      showDeleteConfirm = false;
    }
  }

  async function syncWithS3() {
    syncing = true;
    syncInfo = '';
    fullSyncResult = null;
    err = '';
    try {
      const r = await api.sync();
      const total = r.missing_zips + r.missing_individual;
      if (total === 0) {
        const checked = r.zip_names_in_db + r.individual_keys_in_db;
        syncInfo = `Index in sync — ${checked} object(s) checked.`;
      } else {
        syncInfo = `Sync complete: ${r.missing_zips} zip(s) + ${r.missing_individual} individual file(s) missing from S3, ${r.files_reset} file(s) reset to pending.`;
      }
      await refresh();
    } catch (e) {
      err = String(e);
    } finally {
      syncing = false;
    }
  }

  async function fullSync() {
    syncing = true;
    syncInfo = '';
    fullSyncResult = null;
    err = '';
    resetFixState();
    try {
      const r = await api.syncFull();
      fullSyncResult = r;
      const existence = r.missing_zips + r.missing_individual;
      const parts = [
        `${r.zip_indexes_consumed} zip index(es) consumed`,
        `${r.cloud_file_count} cloud file(s) / ${r.local_file_count} local file(s)`,
      ];
      if (existence > 0) parts.push(`${existence} S3 object(s) missing`);
      syncInfo = `Full sync: ${parts.join(' · ')}.`;
      await refresh();
    } catch (e) {
      err = String(e);
    } finally {
      syncing = false;
    }
  }
</script>

<h1>Dashboard</h1>

{#if err}<div class="card err">{err}</div>{/if}

<div class="grid">
  <div class="card">
    <div class="label">Current run</div>
    {#if status?.current}
      <div class="big">
        run #{status.current.id} <StatusBadge status={status.current.status} />
      </div>
      <div class="muted">started {relativeTime(status.current.started_at)}</div>
      <button onclick={cancel} type="button" style="margin-top: 0.5rem">Cancel</button>
    {:else}
      <div class="big muted">Idle</div>
      <div class="run-actions">
        <button class="primary" onclick={() => triggerRun('full')} type="button" disabled={triggering}>
          {triggering ? 'Starting…' : 'Run now'}
        </button>
        <button onclick={() => triggerRun('scan')} type="button" disabled={triggering} title="Scan source for changes without uploading">
          Scan only
        </button>
        <button onclick={() => triggerRun('upload')} type="button" disabled={triggering} title="Upload all pending files without scanning">
          Upload only
        </button>
      </div>
    {/if}
  </div>

  <div class="card">
    <div class="label">Last run</div>
    {#if status?.last}
      <div class="big">
        #{status.last.id} <StatusBadge status={status.last.status} />
      </div>
      <div class="muted">
        {formatDate(status.last.started_at)} · {status.last.files_uploaded.toLocaleString()} files · {bytes(status.last.bytes_uploaded)}
      </div>
      {#if status.last.error_message}
        <div class="err mono" style="margin-top: 0.5rem">{status.last.error_message}</div>
      {/if}
    {:else}
      <div class="big muted">No runs yet</div>
    {/if}
  </div>

  <div class="card">
    <div class="label">Index</div>
    {#if stats}
      <div class="big">{stats.total_count.toLocaleString()} files</div>
      <div class="muted">{bytes(stats.total_size)} total</div>
      <div class="pills">
        {#each Object.entries(stats.by_status) as [k, v]}
          <span class="pill"><StatusBadge status={k} /> {v.toLocaleString()}</span>
        {/each}
      </div>
      {#if (stats.by_status['missing'] ?? 0) > 0}
        <button
          class="danger fix-btn"
          style="margin-top: 0.5rem"
          onclick={purgeMissing}
          type="button"
          disabled={purging}
          title="Remove DB entries for files deleted from local disk. Does not affect S3."
        >
          {purging ? 'Removing…' : `Remove ${stats.by_status['missing'].toLocaleString()} missing entr${stats.by_status['missing'] === 1 ? 'y' : 'ies'}`}
        </button>
      {/if}
    {/if}
  </div>

  <div class="card">
    <div class="label">S3 sync</div>
    <div class="muted" style="margin-bottom: 0.5rem; font-size: 0.85rem">
      Quick: existence check by S3 key — missing objects reset to pending.<br />
      Full: also downloads every zip index and diffs local vs cloud file sets.
    </div>
    {#if syncInfo}<div class="sync-info">{syncInfo}</div>{/if}
    <div class="run-actions">
      <button onclick={syncWithS3} type="button" disabled={syncing || !!status?.current}>
        {syncing ? 'Syncing…' : 'Sync with S3'}
      </button>
      <button onclick={fullSync} type="button" disabled={syncing || !!status?.current}
              title="Heavier: downloads every .index.txt sidecar and compares file contents">
        Full sync
      </button>
    </div>
  </div>
</div>

{#if fullSyncResult}
  <div class="card">
    <div class="label">Full sync result</div>
    <div class="sync-grid">

      <!-- Local files not in cloud -->
      <details open={fullSyncResult.local_missing_count > 0}>
        <summary>
          <strong>{fullSyncResult.local_missing_count}</strong> local file(s) missing from cloud
          {#if fullSyncResult.local_missing_count > (fullSyncResult.local_missing_from_cloud?.length ?? 0)}
            (showing first {fullSyncResult.local_missing_from_cloud?.length})
          {/if}
        </summary>
        {#if fullSyncResult.local_missing_from_cloud?.length}
          <ul class="mono small">
            {#each fullSyncResult.local_missing_from_cloud as p}<li>{p}</li>{/each}
          </ul>
        {/if}
        {#if fullSyncResult.local_missing_count > 0}
          <div class="fix-row">
            {#if backupStarted}
              <span class="ok-text">Upload run started — watch Live progress below.</span>
            {:else}
              <button
                class="primary fix-btn"
                onclick={backUpMissing}
                type="button"
                disabled={backingUp || !!status?.current}
                title={status?.current ? 'A run is already in progress' : ''}
              >
                {backingUp ? 'Starting…' : `Back up ${fullSyncResult.local_missing_count} missing file(s)`}
              </button>
              {#if status?.current}<span class="muted small">run in progress</span>{/if}
            {/if}
            {#if backupErr}<span class="err small">{backupErr}</span>{/if}
          </div>
        {/if}
      </details>

      <!-- Cloud files not in local -->
      <details open={fullSyncResult.cloud_missing_count > 0}>
        <summary>
          <strong>{fullSyncResult.cloud_missing_count}</strong> cloud file(s) missing locally
          {#if fullSyncResult.cloud_missing_count > (fullSyncResult.cloud_missing_from_local?.length ?? 0)}
            (showing first {fullSyncResult.cloud_missing_from_local?.length})
          {/if}
        </summary>
        {#if fullSyncResult.cloud_missing_from_local?.length}
          <ul class="mono small">
            {#each fullSyncResult.cloud_missing_from_local as p}<li>{p}</li>{/each}
          </ul>
        {/if}
        {#if fullSyncResult.cloud_missing_count > 0}
          <div class="fix-row">
            {#if restoreResult}
              <span class="ok-text">
                Restored {restoreResult.files_written} file(s) · {bytes(restoreResult.bytes_written)}
                {#if restoreResult.skipped?.length} · {restoreResult.skipped.length} skipped{/if}
                {#if restoreResult.errors?.length} · {restoreResult.errors.length} error(s){/if}
              </span>
            {:else if deleteResult}
              <span class="ok-text">
                Deleted {deleteResult.deleted_standalone} standalone + {deleteResult.deleted_zips} zip(s)
                {#if deleteResult.skipped_partial_zip > 0} · {deleteResult.skipped_partial_zip} partial zip(s) skipped{/if}
                {#if deleteResult.errors?.length} · {deleteResult.errors.length} error(s){/if}
              </span>
            {:else if showRestoreForm}
              <div class="fix-form">
                <input
                  type="text"
                  bind:value={restoreTargetDir}
                  placeholder="/absolute/path/to/restore/into"
                  class="path-input"
                />
                <button
                  class="primary fix-btn"
                  onclick={restoreMissing}
                  type="button"
                  disabled={restoring || !restoreTargetDir}
                >
                  {restoring ? 'Restoring…' : `Restore ${fullSyncResult.cloud_missing_from_local?.length ?? fullSyncResult.cloud_missing_count} file(s)`}
                </button>
                <button onclick={() => showRestoreForm = false} type="button">Cancel</button>
              </div>
              {#if restoreErr}<span class="err small">{restoreErr}</span>{/if}
            {:else if showDeleteConfirm}
              <div class="fix-form">
                <span class="warn-text small">
                  Delete {fullSyncResult.cloud_missing_from_local?.length ?? fullSyncResult.cloud_missing_count} cloud object(s)?
                  Standalone files are removed immediately; zips are only deleted when all their contents are targeted.
                </span>
                <button
                  class="danger fix-btn"
                  onclick={deleteCloudMissing}
                  type="button"
                  disabled={deleting}
                >
                  {deleting ? 'Deleting…' : 'Confirm delete'}
                </button>
                <button onclick={() => showDeleteConfirm = false} type="button">Cancel</button>
              </div>
              {#if deleteErr}<span class="err small">{deleteErr}</span>{/if}
            {:else}
              <button class="fix-btn" onclick={() => showRestoreForm = true} type="button">
                Restore to directory…
              </button>
              <button class="fix-btn danger" onclick={() => showDeleteConfirm = true} type="button">
                Delete from cloud…
              </button>
            {/if}
          </div>
        {/if}
      </details>

    </div>
  </div>
{/if}

{#if status?.current || logLines.length}
  <div class="card">
    <div class="label">Live progress</div>
    <div class="live-stats">
      <span>scanned: <strong>{scanSeen.toLocaleString()}</strong></span>
      <span>started: <strong>{uploads.started}</strong></span>
      <span>completed: <strong>{uploads.completed}</strong></span>
      <span>failed: <strong>{uploads.failed}</strong></span>
    </div>
    {#if uploads.started > 0}
      <ProgressBar value={uploads.completed} max={uploads.started} label="Uploads" />
    {/if}
    {#if logLines.length}
      <pre class="log">{logLines.join('\n')}</pre>
    {/if}
  </div>
{/if}

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
    gap: 1rem;
  }
  .label { font-size: 0.8rem; color: var(--muted); margin-bottom: 0.25rem; }
  .run-actions { display: flex; gap: 0.5rem; flex-wrap: wrap; margin-top: 0.5rem; }
  .sync-info { font-size: 0.85rem; margin-bottom: 0.5rem; color: var(--fg); }
  .big { font-size: 1.4rem; font-weight: 500; margin-bottom: 0.3rem; }
  .pills { display: flex; flex-wrap: wrap; gap: 0.4rem; margin-top: 0.5rem; }
  .pill { font-size: 0.85rem; display: inline-flex; gap: 0.4rem; align-items: center; }
  .err { color: var(--err); border-color: var(--err); }
  .live-stats { display: flex; gap: 1rem; flex-wrap: wrap; margin-bottom: 0.75rem; }
  .sync-grid { display: flex; flex-direction: column; gap: 0.75rem; }
  .small { font-size: 0.85rem; }
  details { border: 1px solid var(--border); border-radius: 4px; padding: 0.5rem 0.75rem; }
  details[open] { background: var(--bg); }
  details summary { cursor: pointer; }
  details ul { margin: 0.5rem 0 0; padding-left: 1.25rem; max-height: 220px; overflow: auto; }
  .fix-row { margin-top: 0.75rem; display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; }
  .fix-form { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; width: 100%; }
  .fix-btn { font-size: 0.85rem; }
  .danger { border-color: var(--err); color: var(--err); }
  .danger:hover:not(:disabled) { background: var(--err); color: #fff; }
  .path-input { flex: 1 1 240px; min-width: 0; padding: 0.3rem 0.6rem; font-size: 0.85rem; font-family: var(--mono, monospace); border: 1px solid var(--border); background: var(--bg); color: var(--fg); border-radius: 4px; }
  .ok-text { color: var(--ok, #22c55e); font-size: 0.85rem; }
  .warn-text { color: var(--warn, #f59e0b); }
  .log {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.75rem;
    margin: 0.5rem 0 0;
    max-height: 280px;
    overflow: auto;
    font-size: 0.82rem;
    color: var(--muted);
    /* Long JSON event lines were pushing the page wider than the
       1200px shell; wrap them inside the log pane instead. */
    min-width: 0;
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
