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

  async function syncWithS3() {
    syncing = true;
    syncInfo = '';
    err = '';
    try {
      const r = await api.sync();
      if (r.missing_in_s3 === 0) {
        syncInfo = `Index in sync — ${r.keys_in_db} keys checked.`;
      } else {
        syncInfo = `Sync complete: ${r.missing_in_s3} key(s) missing from S3, ${r.files_reset} file(s) reset to pending.`;
      }
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
    {/if}
  </div>

  <div class="card">
    <div class="label">S3 sync</div>
    <div class="muted" style="margin-bottom: 0.5rem; font-size: 0.85rem">
      Check that every recorded S3 key still exists. Missing objects are reset to pending for re-upload.
    </div>
    {#if syncInfo}<div class="sync-info">{syncInfo}</div>{/if}
    <button onclick={syncWithS3} type="button" disabled={syncing || !!status?.current}>
      {syncing ? 'Syncing…' : 'Sync with S3'}
    </button>
  </div>
</div>

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
  }
</style>
