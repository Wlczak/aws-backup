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
  import Skeleton from '../components/Skeleton.svelte';

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
  type DownloadTone = 'idle' | 'running' | 'ok' | 'warn' | 'err' | 'cancelled';
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
  const cancelledStatus = (detail: string): DownloadStatus => ({
    tone: 'cancelled',
    label: 'Cancelled',
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
    status: 'active' | 'done' | 'failed' | 'cancelled';
    path?: string;
    error?: string;
    errors: number;
    current_path?: string;
    current_bytes?: number;
    current_total_bytes?: number;
    current_percent?: number;
    file_status?: 'active' | 'done' | 'failed' | 'cancelled';
  };
  let downloadProgress = $state<DownloadProgress | null>(null);
  type DownloadItem = {
    path: string;
    bytes: number;
    total: number;
    percent: number;
    status: 'active' | 'done' | 'failed' | 'cancelled';
    error?: string;
  };
  let downloadItems = $state<Record<string, DownloadItem>>({});
  let downloadItemOrder = $state<string[]>([]);
  let currentFile = $state<DownloadItem | null>(null);
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
      const item = {
        path: next.current_path,
        bytes: next.current_bytes ?? 0,
        total: next.current_total_bytes ?? 0,
        percent: next.current_percent ?? 0,
        status: next.file_status ?? (next.status === 'failed' || next.status === 'cancelled'
          ? next.status
          : next.status === 'done'
            ? 'done'
            : 'active'),
        error: next.error,
      };
      currentFile = item;
      upsertDownloadItem(next.current_path, item);
    } else if (currentFile && next.status === 'active') {
      // Keep showing the last active file while the job advances between
      // progress ticks that don't repeat current_path.
      upsertDownloadItem(currentFile.path, currentFile);
    } else if (currentFile && next.status !== 'active') {
      currentFile = {
        ...currentFile,
        status: terminalItemStatus(next.status),
        error: next.error,
      };
      upsertDownloadItem(currentFile.path, currentFile);
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
    downloadStatus = next.status === 'failed' || next.status === 'cancelled'
      ? terminalDownloadStatus(next.status, parts.join(' · '))
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
          : summary.status === 'cancelled'
            ? 'cancelled'
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

  function terminalDownloadStatus(status: DownloadProgress['status'], detail: string): DownloadStatus {
    if (status === 'failed') return errStatus(detail);
    if (status === 'cancelled') return cancelledStatus(detail);
    return warnStatus(detail);
  }

  function terminalItemStatus(status: DownloadProgress['status']): DownloadItem['status'] {
    if (status === 'failed') return 'failed';
    if (status === 'cancelled') return 'cancelled';
    return 'done';
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
    const sub = subscribeEvents((event) => {
      if (event.type === 'restore_download_start') {
        downloadTotalBytes = event.data.total_bytes;
        downloadItems = {};
        downloadItemOrder = [];
        currentFile = null;
        applyDownloadProgress({
          processed: 0,
          total: event.data.total,
          total_bytes: downloadTotalBytes,
          files_written: 0,
          bytes_written: 0,
          phase: 'download',
          status: 'active',
          errors: 0,
        });
      } else if (event.type === 'restore_download_progress') {
        if (event.data.path) {
          upsertDownloadItem(event.data.path, {
            bytes: event.data.current_bytes ?? 0,
            total: event.data.current_total_bytes ?? 0,
            percent: event.data.current_percent ?? 0,
            status: event.data.file_status ?? 'active',
            error: event.data.error,
          });
        }
        applyDownloadProgress({
          processed: event.data.processed,
          total: event.data.total,
          total_bytes: event.data.total_bytes ?? downloadTotalBytes,
          files_written: event.data.files_written,
          bytes_written: event.data.bytes_written,
          phase: 'download',
          status: 'active',
          path: event.data.path,
          error: event.data.error,
          errors: event.data.errors,
          current_path: event.data.current_path ?? event.data.path,
          current_bytes: event.data.current_bytes,
          current_total_bytes: event.data.current_total_bytes,
          current_percent: event.data.current_percent,
          file_status: event.data.file_status,
        });
      } else if (event.type === 'restore_download_complete') {
        downloadTotalBytes = event.data.total_bytes ?? downloadTotalBytes;
        const currentPath = downloadProgress?.current_path;
        const currentBytes = downloadProgress?.current_bytes;
        const currentTotalBytes = downloadProgress?.current_total_bytes;
        const currentPercent = downloadProgress?.current_percent;
        applyDownloadProgress({
          processed: downloadProgress?.processed ?? downloadProgress?.total ?? 0,
          total: downloadProgress?.total ?? 0,
          total_bytes: event.data.total_bytes ?? downloadTotalBytes,
          files_written: event.data.files_written,
          bytes_written: event.data.bytes_written,
          phase: 'download',
          status: 'done',
          errors: event.data.errors,
          current_path: currentPath,
          current_bytes: currentBytes,
          current_total_bytes: currentTotalBytes,
          current_percent: currentPercent,
          file_status: 'done',
        });
        if (currentPath) {
          currentFile = {
            path: currentPath,
            bytes: currentTotalBytes ?? currentBytes ?? 0,
            total: currentTotalBytes ?? currentBytes ?? 0,
            percent: 100,
            status: 'done',
            error: undefined,
          };
          upsertDownloadItem(currentPath, currentFile);
        }
        downloadResult = resultFromProgress({
          processed: downloadProgress?.processed ?? event.data.files_written,
          total: downloadProgress?.total ?? event.data.files_written,
          total_bytes: event.data.total_bytes ?? downloadTotalBytes,
          files_written: event.data.files_written,
          bytes_written: event.data.bytes_written,
          phase: 'download',
          status: 'done',
          errors: event.data.errors,
        });
      } else if (event.type === 'restore_download_failed') {
        downloadTotalBytes = event.data.total_bytes ?? downloadTotalBytes;
        const currentPath = downloadProgress?.current_path;
        const currentBytes = downloadProgress?.current_bytes;
        const currentTotalBytes = downloadProgress?.current_total_bytes;
        const currentPercent = downloadProgress?.current_percent;
        applyDownloadProgress({
          processed: downloadProgress?.processed ?? downloadProgress?.total ?? 0,
          total: downloadProgress?.total ?? 0,
          total_bytes: event.data.total_bytes ?? downloadTotalBytes,
          files_written: event.data.files_written,
          bytes_written: event.data.bytes_written,
          phase: 'download',
          status: 'failed',
          errors: event.data.errors,
          error: event.data.error,
          current_path: currentPath,
          current_bytes: currentBytes,
          current_total_bytes: currentTotalBytes,
          current_percent: currentPercent,
          file_status: 'failed',
        });
        if (currentPath) {
          currentFile = {
            path: currentPath,
            bytes: currentBytes ?? 0,
            total: currentTotalBytes ?? 0,
            percent: currentPercent ?? 0,
            status: 'failed',
            error: event.data.error,
          };
          upsertDownloadItem(currentPath, currentFile);
        }
        downloadResult = resultFromProgress({
          processed: downloadProgress?.processed ?? event.data.files_written,
          total: downloadProgress?.total ?? event.data.files_written,
          total_bytes: event.data.total_bytes ?? downloadTotalBytes,
          files_written: event.data.files_written,
          bytes_written: event.data.bytes_written,
          phase: 'download',
          status: 'failed',
          errors: event.data.errors,
          error: event.data.error,
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
    currentFile = null;
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
                : nextSummary.status === 'cancelled'
                  ? 'cancelled'
                  : 'done',
            error: nextSummary.error_message,
          });
        }
        if (nextSummary.status === 'completed' || nextSummary.status === 'failed' || nextSummary.status === 'cancelled') {
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
  {#if estimating && !downloadEstimate}
    <div class="card loading-card">
      <div class="label">Estimate</div>
      <div class="skeleton-card">
        <Skeleton lines={1} widths={['64%']} height="0.95rem" />
        <div class="stats">
          {#each Array(4) as _}
            <div>
              <Skeleton lines={1} widths={['5rem']} height="0.85rem" />
              <Skeleton lines={1} widths={['4.5rem']} height="1.3rem" />
            </div>
          {/each}
        </div>
        <Skeleton lines={1} widths={['88%']} height="0.95rem" />
      </div>
    </div>
  {/if}
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
    <div class="current-file">
      {#if currentFile}
        <div class="current-head">
          <div>
            <div class="muted small">Current file</div>
            <div class="mono current-path">{currentFile.path}</div>
          </div>
          <div class="mono current-pct">{currentFile.percent}%</div>
        </div>
        {#if currentFile.total > 0}
          <div style="margin-top: 0.5rem">
            <ProgressBar
              label="File bytes"
              value={currentFile.bytes}
              max={currentFile.total}
            />
          </div>
          <div class="muted small" style="margin-top: 0.25rem">
            {bytes(currentFile.bytes)} / {bytes(currentFile.total)}
            {#if currentFile.status !== 'active'} · {currentFile.status}{/if}
            {#if currentFile.error} · {currentFile.error}{/if}
          </div>
        {/if}
      {:else}
        <div class="muted small">Current file</div>
        <div class="current-placeholder">
          <span class="current-placeholder-text">Waiting for the next file…</span>
          <span class="current-placeholder-skeleton skeleton skeleton-line"></span>
        </div>
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
  .status-pill.cancelled { color: var(--warn); }
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
    display: grid;
    gap: 0.35rem;
  }
  .current-placeholder-skeleton {
    height: 0.8rem;
    width: 72%;
    border-radius: 999px;
  }
  .loading-card {
    margin-top: 0.75rem;
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
  .activity-pill.cancelled { color: var(--warn); }
  .activity-meta {
    margin-top: 0.35rem;
    color: var(--muted);
    overflow-wrap: anywhere;
  }
</style>
