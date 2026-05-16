<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import {
    api,
    subscribeEvents,
    type RestoreDownloadEstimate,
    type RestoreDownloadSummary,
  } from '../lib/api';
  import { bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import { paths as selectionPaths, clear as clearSelection } from '../lib/selection';
  import ProgressBar from '../components/ProgressBar.svelte';

  let raw = $state('');
  let downloadTargetDir = $state('');
  let downloadEstimate = $state<RestoreDownloadEstimate | null>(null);
  type DownloadResult = {
    files_written: number;
    bytes_written: number;
    total_bytes: number;
    errors: number;
  };
  let downloadResult = $state<DownloadResult | null>(null);
  let downloadBusy = $state(false);
  let estimating = $state(false);
  let verifyChecksum = $state(true);
  let lastVerifyChecksum = $state(true);
  type DownloadTone = 'idle' | 'running' | 'ok' | 'warn' | 'err';
  type DownloadStatus = {
    tone: DownloadTone;
    label: string;
    detail: string;
  };
  const idleStatus = (): DownloadStatus => ({
    tone: 'idle',
    label: 'Idle',
    detail: 'Select paths and a target directory to start a download.',
  });
  const runningStatus = (detail: string): DownloadStatus => ({
    tone: 'running',
    label: 'Downloading',
    detail,
  });
  const okStatus = (detail: string): DownloadStatus => ({
    tone: 'ok',
    label: 'Complete',
    detail,
  });
  const warnStatus = (detail: string): DownloadStatus => ({
    tone: 'warn',
    label: 'Complete with warnings',
    detail,
  });
  const errStatus = (detail: string): DownloadStatus => ({
    tone: 'err',
    label: 'Failed',
    detail,
  });
  let downloadStatus = $state<DownloadStatus>(idleStatus());
  let downloadTotalBytes = $state(0);
  type DownloadProgress = {
    processed: number;
    total: number;
    total_bytes: number;
    files_written: number;
    bytes_written: number;
    phase: 'download';
    status: 'active' | 'done' | 'failed';
    path?: string;
    error?: string;
    errors: number;
  };
  let downloadProgress = $state<DownloadProgress | null>(null);
  let pollTimer: number | undefined;
  let pollDestroyed = false;

  $effect(() => {
    void raw;
    void downloadTargetDir;
    downloadEstimate = null;
    downloadResult = null;
    if (!downloadBusy && !downloadProgress) {
      downloadStatus = idleStatus();
    }
  });

  function applyDownloadProgress(next: DownloadProgress | null) {
    downloadProgress = next;
    if (!next) {
      if (!downloadBusy) downloadStatus = idleStatus();
      return;
    }
    downloadTotalBytes = next.total_bytes ?? downloadTotalBytes;
    if (next.status === 'active') {
      const parts = [
        `${next.files_written.toLocaleString()} written`,
        `${next.processed.toLocaleString()} / ${next.total.toLocaleString()} checked`,
      ];
      if (next.path) parts.push(next.path);
      if (next.error) parts.push(next.error);
      downloadStatus = runningStatus(parts.join(' · '));
      return;
    }

    const parts = [
      `${next.files_written.toLocaleString()} written`,
      bytes(next.bytes_written),
    ];
    parts.push(`${next.processed.toLocaleString()} checked`);
    if (next.errors > 0) parts.push(`${next.errors.toLocaleString()} error(s)`);
    if (next.error) parts.push(next.error);
    downloadStatus = next.status === 'failed'
      ? errStatus(parts.join(' · '))
      : next.errors > 0
        ? warnStatus(parts.join(' · '))
        : okStatus(parts.join(' · '));
  }

  function progressFromSummary(summary: RestoreDownloadSummary): DownloadProgress {
    return {
      processed: summary.processed,
      total: summary.total,
      total_bytes: summary.total_bytes,
      files_written: summary.files_written,
      bytes_written: summary.bytes_written,
      phase: 'download',
      status: summary.status === 'running'
        ? 'active'
        : summary.status === 'failed'
          ? 'failed'
          : 'done',
      error: summary.error_message,
      errors: summary.errors,
    };
  }

  function resultFromProgress(next: DownloadProgress): DownloadResult {
    return {
      files_written: next.files_written,
      bytes_written: next.bytes_written,
      total_bytes: next.total_bytes,
      errors: next.errors,
    };
  }

  let aborted = false;
  onDestroy(() => { aborted = true; });
  onDestroy(() => {
    pollDestroyed = true;
    if (pollTimer) clearTimeout(pollTimer);
  });

  function scheduleNextPoll() {
    if (pollDestroyed) return;
    const delay = downloadProgress?.status === 'active' ? 1000 : 30000;
    pollTimer = window.setTimeout(async () => {
      await refresh();
      scheduleNextPoll();
    }, delay);
  }

  onMount(() => {
    void (async () => {
      try {
        const settings = await api.settings();
        if (downloadTargetDir.trim() === '') {
          downloadTargetDir = settings.backup.download_dir ?? '';
        }
      } catch {
        // If settings aren't available yet, leave the field empty and let
        // the operator pick a target directory manually.
      }
    })();

    const pre = selectionPaths();
    if (pre.length > 0) {
      raw = pre.join('\n');
      toast.info(`Pre-filled ${pre.length} path(s) from Files selection.`);
      clearSelection();
    }
    void refresh().then(() => scheduleNextPoll());
    const sub = subscribeEvents((type, data) => {
      if (type === 'restore_download_start') {
        const d = data as { total: number; total_bytes: number };
        downloadTotalBytes = d.total_bytes ?? 0;
        applyDownloadProgress({
          processed: 0,
          total: d.total,
          total_bytes: downloadTotalBytes,
          files_written: 0,
          bytes_written: 0,
          phase: 'download',
          status: 'active',
          errors: 0,
        });
      } else if (type === 'restore_download_progress') {
        const d = data as {
          processed: number;
          total: number;
          total_bytes: number;
          files_written: number;
          bytes_written: number;
          path?: string;
          error?: string;
          errors: number;
        };
        applyDownloadProgress({
          processed: d.processed,
          total: d.total,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          phase: 'download',
          status: 'active',
          path: d.path,
          error: d.error,
          errors: d.errors,
        });
      } else if (type === 'restore_download_complete') {
        const d = data as { files_written: number; bytes_written: number; total_bytes: number; errors: number };
        downloadTotalBytes = d.total_bytes ?? downloadTotalBytes;
        applyDownloadProgress({
          processed: downloadProgress?.processed ?? downloadProgress?.total ?? 0,
          total: downloadProgress?.total ?? 0,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          phase: 'download',
          status: 'done',
          errors: d.errors,
        });
        downloadResult = resultFromProgress({
          processed: downloadProgress?.processed ?? d.files_written,
          total: downloadProgress?.total ?? d.files_written,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          phase: 'download',
          status: 'done',
          errors: d.errors,
        });
      } else if (type === 'restore_download_failed') {
        const d = data as { files_written: number; bytes_written: number; total_bytes: number; errors: number; error?: string };
        downloadTotalBytes = d.total_bytes ?? downloadTotalBytes;
        applyDownloadProgress({
          processed: downloadProgress?.processed ?? downloadProgress?.total ?? 0,
          total: downloadProgress?.total ?? 0,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          phase: 'download',
          status: 'failed',
          errors: d.errors,
          error: d.error,
        });
        downloadResult = resultFromProgress({
          processed: downloadProgress?.processed ?? d.files_written,
          total: downloadProgress?.total ?? d.files_written,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          phase: 'download',
          status: 'failed',
          errors: d.errors,
          error: d.error,
        });
      }
    });
    return () => sub.close();
  });

  function paths(): string[] {
    const parsed = raw
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
    if (parsed.includes('/')) return ['/'];
    return parsed;
  }

  async function doDownload() {
    const p = paths();
    if (p.length === 0) {
      toast.error('enter at least one path, or / for all files');
      return;
    }
    if (downloadTargetDir.trim() === '') {
      toast.error('enter an absolute target directory');
      return;
    }
    downloadBusy = true;
    downloadProgress = null;
    downloadResult = null;
    downloadTotalBytes = 0;
    downloadStatus = runningStatus('Submitting download request…');
    try {
      lastVerifyChecksum = verifyChecksum;
      const r = await api.restoreDownload(p, downloadTargetDir.trim(), verifyChecksum);
      if (aborted) return;
      downloadStatus = runningStatus(`Download ${r.restore_download_id.toLocaleString()} accepted. Waiting for progress…`);
      if (!downloadProgress) {
        applyDownloadProgress({
          processed: 0,
          total: 0,
          total_bytes: 0,
          files_written: 0,
          bytes_written: 0,
          phase: 'download',
          status: 'active',
          errors: 0,
        });
      }
      toast.success(`Download queued for ${downloadTargetDir.trim()}.`);
    } catch (e) {
      downloadStatus = errStatus(String(e));
      toast.error(String(e));
    } finally {
      downloadBusy = false;
    }
  }

  async function refresh() {
    try {
      const s = await api.status();
      const cur = s.restore_download_current;
      const last = s.restore_download_last;
      const nextSummary = cur ?? last ?? null;
      if (nextSummary) {
        applyDownloadProgress(progressFromSummary(nextSummary));
        if (nextSummary.status === 'completed' || nextSummary.status === 'failed') {
          downloadResult = resultFromProgress(progressFromSummary(nextSummary));
        }
      } else if (!downloadBusy) {
        applyDownloadProgress(null);
      }
    } catch (e) {
      toast.error(String(e));
    }
  }

  async function doEstimate() {
    const p = paths();
    if (p.length === 0) {
      toast.error('enter at least one path, or / for all files');
      return;
    }
    estimating = true;
    downloadEstimate = null;
    try {
      downloadEstimate = await api.restoreDownloadEstimate(p);
    } catch (e) {
      toast.error(String(e));
    } finally {
      estimating = false;
    }
  }
</script>

<h1>Download restored files</h1>

<div class="card">
  <div class="label">Paths (one per line)</div>
  <p class="muted">
    Enter top-level directories or full file paths. Prefix match is applied — e.g.
    <code class="mono">photos</code> selects every file under <code class="mono">photos/</code>.
    Enter <code class="mono">/</code> to select <strong>all files</strong>.
  </p>
  <textarea bind:value={raw} placeholder={"photos\ndocs/2024\nfamily-archive.zip"}></textarea>

  <div class="label" style="margin-top: 0.75rem">Target directory</div>
  <p class="muted">
    Download matching S3 objects or zip members into an absolute local directory
    and verify each restored file against the MD5 stored in the index. Glacier
    objects must already be available; otherwise the download reports a still
    thawing error. The field is prefilled from Settings when a download
    directory is configured.
  </p>
  <div class="download-form">
    <input
      type="text"
      bind:value={downloadTargetDir}
      placeholder="/absolute/path/to/restore"
      class="target-input mono"
      aria-label="Target directory"
    />
    <label class="verify">
      <input type="checkbox" bind:checked={verifyChecksum} />
      Verify hashes against stored MD5
    </label>
    <button
      class="primary"
      onclick={doDownload}
      disabled={(downloadBusy || downloadProgress?.status === 'active') || downloadTargetDir.trim() === ''}
      type="button"
    >
      {#if downloadBusy || downloadProgress?.status === 'active'}
        Downloading…
      {:else}
        Download and verify
      {/if}
    </button>
    <button type="button" onclick={doEstimate} disabled={estimating || downloadBusy || downloadProgress?.status === 'active'}>
      {estimating ? 'Estimating…' : 'Estimate cost'}
    </button>
  </div>
  <div class="statusline" aria-live="polite">
    <span class={`status-pill ${downloadStatus.tone}`}>{downloadStatus.label}</span>
    <span class="status-detail">{downloadStatus.detail}</span>
  </div>
  {#if downloadEstimate}
    <div class="stats" style="margin-top: 0.75rem">
      <div>
        <div class="muted">Restored files</div>
        <div class="big">{downloadEstimate.restored_count.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">In progress</div>
        <div class="big">{downloadEstimate.in_progress_count.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">Not restoring</div>
        <div class="big">{downloadEstimate.not_restoring_count.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">S3 objects</div>
        <div class="big">{downloadEstimate.object_count.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">Downloadable data</div>
        <div class="big">{bytes(downloadEstimate.total_bytes)}</div>
      </div>
      <div>
        <div class="muted">GET fee</div>
        <div class="big">${downloadEstimate.request_fee_usd.toFixed(2)}</div>
      </div>
      <div>
        <div class="muted">Egress</div>
        <div class="big">${downloadEstimate.egress_fee_usd.toFixed(2)}</div>
      </div>
      <div>
        <div class="muted">Total</div>
        <div class="big">${downloadEstimate.total_fee_usd.toFixed(2)}</div>
      </div>
    </div>
    <p class="muted small" style="margin-top: 0.5rem">
      Only restored files are counted as downloadable. Zipped groups still count as one S3 object for request fees.
    </p>
    {#if downloadEstimate.unknown_paths?.length}
      <details style="margin-top: 0.75rem">
        <summary>{downloadEstimate.unknown_paths.length} unknown path(s)</summary>
        <ul class="mono small">
          {#each downloadEstimate.unknown_paths as p}<li>{p}</li>{/each}
        </ul>
      </details>
    {/if}
  {/if}
  {#if downloadProgress}
    <div class="label" style="margin-top: 0.75rem">Live download progress</div>
    {@const pct = downloadProgress.total > 0
      ? Math.min(100, Math.round((downloadProgress.processed / downloadProgress.total) * 100))
      : 0}
    <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={pct} style="margin-top: 0.75rem">
      <div class="fill" style="width: {pct}%"></div>
    </div>
    <p class="muted small" style="margin-top: 0.25rem">
      {downloadProgress.files_written.toLocaleString()} written · {bytes(downloadProgress.bytes_written)} ·
      {downloadProgress.processed.toLocaleString()} / {downloadProgress.total.toLocaleString()} checked
      {#if downloadProgress.path} · {downloadProgress.path}{/if}
      {#if downloadProgress.error} · {downloadProgress.error}{/if}
    </p>
    {#if downloadProgress.total_bytes > 0}
      <div style="margin-top: 0.6rem">
        <ProgressBar
          label="Size downloaded"
          value={downloadProgress.bytes_written}
          max={downloadProgress.total_bytes}
        />
      </div>
    {/if}
  {/if}
</div>

{#if downloadResult}
  <div class="card">
    <div class="label">Download result</div>
    <div class="stats">
      <div>
        <div class="muted">Files written</div>
        <div class="big">{downloadResult.files_written.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">Bytes written</div>
        <div class="big">{bytes(downloadResult.bytes_written)}</div>
      </div>
      <div>
        <div class="muted">Errors</div>
        <div class="big">{downloadResult.errors}</div>
      </div>
      <div>
        <div class="muted">Verification</div>
        <div class="big">{lastVerifyChecksum ? 'On' : 'Off'}</div>
      </div>
    </div>
    {#if downloadTotalBytes > 0}
      <div style="margin-top: 0.75rem">
        <ProgressBar
          label="Size downloaded"
          value={downloadResult.bytes_written}
          max={downloadTotalBytes}
        />
      </div>
    {/if}
  </div>
{/if}

<style>
  .err { color: var(--err); border-color: var(--err); }
  .label { font-size: 0.8rem; color: var(--muted); margin-bottom: 0.25rem; }
  textarea {
    width: 100%;
    min-height: 160px;
    font-family: ui-monospace, monospace;
    font-size: 0.9rem;
    margin-top: 0.5rem;
  }
  .download-form {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    align-items: center;
    margin-top: 0.75rem;
  }
  .verify {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    white-space: nowrap;
  }
  .statusline {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    align-items: center;
    margin-top: 0.85rem;
    padding: 0.65rem 0.8rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--card-bg, transparent);
  }
  .status-pill {
    display: inline-flex;
    align-items: center;
    padding: 0.2rem 0.55rem;
    border-radius: 999px;
    border: 1px solid var(--border);
    font-size: 0.8rem;
    font-weight: 600;
    letter-spacing: 0.01em;
    text-transform: uppercase;
  }
  .status-pill.idle { color: var(--muted); }
  .status-pill.running { color: var(--accent); }
  .status-pill.ok { color: var(--ok, #1f7a3f); }
  .status-pill.warn { color: var(--warn); }
  .status-pill.err { color: var(--err); }
  .status-detail {
    min-width: 0;
    color: var(--muted);
    font-size: 0.9rem;
    overflow-wrap: anywhere;
  }
  .target-input {
    flex: 1 1 320px;
    min-width: 0;
    padding: 0.45rem 0.6rem;
    font-size: 0.9rem;
    font-family: var(--mono, monospace);
    border: 1px solid var(--border);
    background: var(--bg);
    color: var(--fg);
    border-radius: 4px;
  }
  .small { font-size: 0.85rem; }
  details ul { margin: 0.5rem 0 0; padding-left: 1.25rem; max-height: 220px; overflow: auto; }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 1rem;
  }
  .big { font-size: 1.3rem; font-weight: 500; }
  .bar { height: 6px; background: var(--bg); border: 1px solid var(--border); border-radius: 3px; overflow: hidden; }
  .fill { height: 100%; background: var(--accent); transition: width 0.2s ease; }
</style>
