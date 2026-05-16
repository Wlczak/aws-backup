<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api, subscribeEvents, type RestoreDownloadEstimate, type RestoreDownloadResponse } from '../lib/api';
  import { bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import { paths as selectionPaths, clear as clearSelection } from '../lib/selection';
  import ProgressBar from '../components/ProgressBar.svelte';

  let raw = $state('');
  let downloadTargetDir = $state('');
  let downloadEstimate = $state<RestoreDownloadEstimate | null>(null);
  let downloadResult = $state<RestoreDownloadResponse | null>(null);
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
  let downloadProgress = $state<{
    processed: number;
    total: number;
    total_bytes: number;
    files_written: number;
    bytes_written: number;
    path?: string;
    error?: string;
    errors: number;
  } | null>(null);

  $effect(() => {
    void raw;
    void downloadTargetDir;
    downloadEstimate = null;
    downloadResult = null;
    if (!downloadBusy && !downloadProgress) {
      downloadStatus = idleStatus();
    }
  });

  let aborted = false;
  onDestroy(() => { aborted = true; });

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
    const sub = subscribeEvents((type, data) => {
      if (type === 'restore_download_start') {
        const d = data as { total: number; total_bytes: number };
        downloadTotalBytes = d.total_bytes ?? 0;
        downloadProgress = { processed: 0, total: d.total, total_bytes: downloadTotalBytes, files_written: 0, bytes_written: 0, errors: 0 };
        downloadStatus = runningStatus(`0 / ${d.total.toLocaleString()} files checked.`);
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
        downloadProgress = {
          processed: d.processed,
          total: d.total,
          total_bytes: d.total_bytes ?? downloadTotalBytes,
          files_written: d.files_written,
          bytes_written: d.bytes_written,
          path: d.path,
          error: d.error,
          errors: d.errors,
        };
        const parts = [
          `${d.files_written.toLocaleString()} written`,
          `${d.processed.toLocaleString()} / ${d.total.toLocaleString()} checked`,
        ];
        if (d.path) parts.push(d.path);
        if (d.error) parts.push(d.error);
        downloadStatus = runningStatus(parts.join(' · '));
      } else if (type === 'restore_download_complete') {
        const d = data as { files_written: number; bytes_written: number; total_bytes: number; errors: number };
        downloadTotalBytes = d.total_bytes ?? downloadTotalBytes;
        downloadProgress = null;
        downloadStatus = d.errors > 0
          ? warnStatus(`${d.files_written.toLocaleString()} written · ${bytes(d.bytes_written)} · ${d.errors.toLocaleString()} error(s).`)
          : okStatus(`${d.files_written.toLocaleString()} written · ${bytes(d.bytes_written)}.`);
      } else if (type === 'restore_download_failed') {
        const d = data as { files_written: number; bytes_written: number; total_bytes: number; errors: number; error?: string };
        downloadTotalBytes = d.total_bytes ?? downloadTotalBytes;
        downloadProgress = null;
        const detail = [
          `${d.files_written.toLocaleString()} written`,
          bytes(d.bytes_written),
          `${d.errors.toLocaleString()} error(s)`,
          d.error,
        ].filter(Boolean).join(' · ');
        downloadStatus = errStatus(detail);
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
    downloadResult = null;
    downloadTotalBytes = 0;
    downloadStatus = runningStatus('Submitting download request…');
    try {
      lastVerifyChecksum = verifyChecksum;
      const r = await api.restoreDownload(p, downloadTargetDir.trim(), verifyChecksum);
      if (aborted) return;
      downloadResult = r;
      downloadTotalBytes = r.total_bytes ?? downloadTotalBytes;
      const skipped = r.skipped?.length ?? 0;
      const errorCount = r.errors?.length ?? 0;
      const parts = [
        `${r.files_written.toLocaleString()} written`,
        bytes(r.bytes_written),
      ];
      if (skipped > 0) parts.push(`${skipped.toLocaleString()} skipped`);
      if (errorCount > 0) parts.push(`${errorCount.toLocaleString()} error(s)`);
      downloadStatus = errorCount > 0
        ? warnStatus(parts.join(' · '))
        : okStatus(parts.join(' · '));
      if (r.errors?.length) {
        toast.info(`Downloaded ${r.files_written.toLocaleString()} file(s) with ${r.errors.length} error(s).`);
      } else {
        toast.success(`Downloaded ${r.files_written.toLocaleString()} file(s) to ${downloadTargetDir.trim()}.`);
      }
    } catch (e) {
      downloadStatus = errStatus(String(e));
      toast.error(String(e));
    } finally {
      downloadBusy = false;
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
      disabled={downloadBusy || downloadTargetDir.trim() === ''}
      type="button"
    >
      {#if downloadBusy}
        Downloading…
      {:else}
        Download and verify
      {/if}
    </button>
    <button type="button" onclick={doEstimate} disabled={estimating || downloadBusy}>
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
        <div class="muted">Skipped</div>
        <div class="big">{downloadResult.skipped?.length ?? 0}</div>
      </div>
      <div>
        <div class="muted">Errors</div>
        <div class="big">{downloadResult.errors?.length ?? 0}</div>
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
    {#if downloadResult.skipped?.length}
      <details style="margin-top: 0.75rem">
        <summary>{downloadResult.skipped.length} skipped path(s)</summary>
        <ul class="mono small">
          {#each downloadResult.skipped as p}<li>{p}</li>{/each}
        </ul>
      </details>
    {/if}
    {#if downloadResult.errors?.length}
      <details open style="margin-top: 0.75rem" class="err">
        <summary>{downloadResult.errors.length} error(s)</summary>
        <ul class="mono small">
          {#each downloadResult.errors as p}<li>{p}</li>{/each}
        </ul>
      </details>
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
