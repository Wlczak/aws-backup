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
    current_path?: string;
    current_bytes?: number;
    current_total_bytes?: number;
    current_percent?: number;
    file_status?: 'active' | 'done' | 'failed';
  };
  let downloadProgress = $state<DownloadProgress | null>(null);
  type DownloadItem = {
    path: string;
    bytes: number;
    total: number;
    percent: number;
    status: 'active' | 'done' | 'failed';
    error?: string;
  };
  let downloadItems = $state<Record<string, DownloadItem>>({});
  let downloadItemOrder = $state<string[]>([]);
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
    if (next.current_path) {
      upsertDownloadItem(next.current_path, {
        bytes: next.current_bytes ?? 0,
        total: next.current_total_bytes ?? 0,
        percent: next.current_percent ?? 0,
        status: next.file_status ?? (next.status === 'failed' ? 'failed' : next.status === 'done' ? 'done' : 'active'),
        error: next.error,
      });
    }
    if (next.status === 'active') {
      const parts = [
        `${next.files_written.toLocaleString()} written`,
        `${next.processed.toLocaleString()} / ${next.total.toLocaleString()} checked`,
      ];
      if (next.current_path) {
        const total = next.current_total_bytes ?? 0;
        const current = next.current_bytes ?? 0;
        const pct = next.current_percent ?? 0;
        parts.push(`${next.current_path} (${bytes(current)} / ${bytes(total)}${total > 0 ? `, ${pct}%` : ''})`);
      } else if (next.path) {
        parts.push(next.path);
      }
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
      current_path: summary.current_path,
      current_bytes: summary.current_bytes,
      current_total_bytes: summary.current_total_bytes,
      current_percent: summary.current_percent,
      error: summary.error_message,
      errors: summary.errors,
    };
  }

  function upsertDownloadItem(path: string, patch: Partial<DownloadItem>) {
    const prev = downloadItems[path];
    if (!prev) {
      downloadItemOrder = [...downloadItemOrder, path];
    }
    downloadItems[path] = {
      path,
      bytes: prev?.bytes ?? 0,
      total: prev?.total ?? 0,
      percent: prev?.percent ?? 0,
      status: prev?.status ?? 'active',
      error: prev?.error,
      ...patch,
    };
  }

  let downloadItemList = $derived(
    downloadItemOrder.map((path) => downloadItems[path]).filter((item): item is DownloadItem => !!item),
  );

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
        downloadItems = {};
        downloadItemOrder = [];
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
          current_path?: string;
          current_bytes?: number;
          current_total_bytes?: number;
          current_percent?: number;
          file_status?: 'active' | 'done' | 'failed';
        };
        if (d.path) {
          upsertDownloadItem(d.path, {
            bytes: d.current_bytes ?? 0,
            total: d.current_total_bytes ?? 0,
            percent: d.current_percent ?? 0,
            status: d.file_status ?? 'active',
            error: d.error,
          });
        }
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
          current_path: d.current_path ?? d.path,
          current_bytes: d.current_bytes,
          current_total_bytes: d.current_total_bytes,
          current_percent: d.current_percent,
          file_status: d.file_status,
        });
      } else if (type === 'restore_download_complete') {
        const d = data as { files_written: number; bytes_written: number; total_bytes: number; errors: number };
        downloadTotalBytes = d.total_bytes ?? downloadTotalBytes;
        const currentPath = downloadProgress?.current_path;
        const currentBytes = downloadProgress?.current_bytes;
        const currentTotalBytes = downloadProgress?.current_total_bytes;
        const currentPercent = downloadProgress?.current_percent;
        applyDownloadProgress({
          processed: downloadProgress?.processed ?? downloadProgress?.total ?? 0,
          total: downloadProgress?.total ?? 0,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          phase: 'download',
          status: 'done',
          errors: d.errors,
          current_path: currentPath,
          current_bytes: currentBytes,
          current_total_bytes: currentTotalBytes,
          current_percent: currentPercent,
          file_status: 'done',
        });
        if (currentPath) {
          upsertDownloadItem(currentPath, {
            bytes: currentTotalBytes ?? currentBytes ?? 0,
            total: currentTotalBytes ?? currentBytes ?? 0,
            percent: 100,
            status: 'done',
            error: undefined,
          });
        }
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
        const currentPath = downloadProgress?.current_path;
        const currentBytes = downloadProgress?.current_bytes;
        const currentTotalBytes = downloadProgress?.current_total_bytes;
        const currentPercent = downloadProgress?.current_percent;
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
          current_path: currentPath,
          current_bytes: currentBytes,
          current_total_bytes: currentTotalBytes,
          current_percent: currentPercent,
          file_status: 'failed',
        });
        if (currentPath) {
          upsertDownloadItem(currentPath, {
            bytes: currentBytes ?? 0,
            total: currentTotalBytes ?? 0,
            percent: currentPercent ?? 0,
            status: 'failed',
            error: d.error,
          });
        }
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
    downloadItems = {};
    downloadItemOrder = [];
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
        if (nextSummary.current_path) {
          upsertDownloadItem(nextSummary.current_path, {
            bytes: nextSummary.current_bytes ?? 0,
            total: nextSummary.current_total_bytes ?? 0,
            percent: nextSummary.current_percent ?? 0,
            status: nextSummary.status === 'running'
              ? 'active'
              : nextSummary.status === 'failed'
                ? 'failed'
                : 'done',
            error: nextSummary.error_message,
          });
        }
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
      {#if downloadProgress.error} · {downloadProgress.error}{/if}
    </p>
    <div class="current-file" class:empty={!downloadProgress.current_path}>
      {#if downloadProgress.current_path}
        <div class="current-head">
          <div>
            <div class="muted small">Current file</div>
            <div class="mono current-path">{downloadProgress.current_path}</div>
          </div>
          <div class="mono current-pct">{downloadProgress.current_percent ?? 0}%</div>
        </div>
        {#if (downloadProgress.current_total_bytes ?? 0) > 0}
          <div style="margin-top: 0.5rem">
            <ProgressBar
              label="File bytes"
              value={downloadProgress.current_bytes ?? 0}
              max={downloadProgress.current_total_bytes ?? 0}
            />
          </div>
        {/if}
      {:else}
        <div class="muted small">Current file</div>
        <div class="current-placeholder">Waiting for the next file…</div>
      {/if}
    </div>
    {#if downloadProgress.total_bytes > 0}
      <div style="margin-top: 0.6rem">
        <ProgressBar
          label="Job bytes"
          value={downloadProgress.bytes_written}
          max={downloadProgress.total_bytes}
        />
      </div>
    {/if}
  {/if}
</div>

{#if downloadItemList.length}
  <div class="card">
    <div class="label">Download activity</div>
    <div class="activity-list">
      {#each downloadItemList as item (item.path)}
        <div class="activity-item">
          <div class="activity-head">
            <span class="mono activity-path">{item.path}</span>
            <span class={`activity-pill ${item.status}`}>{item.status}</span>
          </div>
          <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={item.percent} style="margin-top: 0.45rem">
            <div class="fill" style="width: {item.percent}%"></div>
          </div>
          <div class="activity-meta mono small">
            {bytes(item.bytes)} / {bytes(item.total)} ({item.percent}%)
            {#if item.error} · {item.error}{/if}
          </div>
        </div>
      {/each}
    </div>
  </div>
{/if}

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
  .current-file {
    margin-top: 0.75rem;
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: rgba(255, 255, 255, 0.02);
    min-height: 5.5rem;
  }
  .current-file.empty {
    display: flex;
    flex-direction: column;
    justify-content: center;
  }
  .current-head,
  .activity-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 0.75rem;
  }
  .current-path,
  .activity-path {
    overflow-wrap: anywhere;
  }
  .current-pct {
    white-space: nowrap;
    color: var(--muted);
  }
  .current-placeholder {
    margin-top: 0.35rem;
    color: var(--muted);
    min-height: 1.25rem;
  }
  .activity-list {
    display: grid;
    gap: 0.75rem;
  }
  .activity-item {
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: rgba(255, 255, 255, 0.02);
  }
  .activity-pill {
    display: inline-flex;
    align-items: center;
    padding: 0.15rem 0.5rem;
    border-radius: 999px;
    border: 1px solid var(--border);
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.02em;
  }
  .activity-pill.active { color: var(--accent); }
  .activity-pill.done { color: var(--ok, #1f7a3f); }
  .activity-pill.failed { color: var(--err); }
  .activity-meta {
    margin-top: 0.35rem;
    color: var(--muted);
    overflow-wrap: anywhere;
  }
</style>
