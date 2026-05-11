<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import ProgressBar from '../components/ProgressBar.svelte';
  import { api, ApiError, subscribeEvents, type RestoreToDirResult } from '../lib/api';
  import { bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import { clear as clearSelection, paths as selectionPaths } from '../lib/selection';

  type DownloadTone = 'idle' | 'running' | 'ok' | 'warn' | 'err';
  type DownloadStatus = {
    tone: DownloadTone;
    label: string;
    detail: string;
  };

  let raw = $state('');
  let targetDir = $state('');
  let verifyChecksum = $state(true);
  let loading = $state(false);
  let result = $state<RestoreToDirResult | null>(null);
  let lastVerifyChecksum = $state(true);
  let downloadStatus = $state<DownloadStatus>({
    tone: 'idle',
    label: 'Idle',
    detail: 'Select paths and a target directory to start a download.',
  });
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
  let aborted = false;

  onMount(() => {
    const pre = selectionPaths();
    if (pre.length > 0) {
      raw = pre.join('\n');
      toast.info(`Pre-filled ${pre.length} path(s) from Files selection.`);
      clearSelection();
    }
    try {
      const lastTarget = localStorage.getItem('aws-backup:download-target-dir');
      if (lastTarget) targetDir = lastTarget;
    } catch {
      // Private mode / storage denial.
    }

    const sub = subscribeEvents((type, data) => {
      if (type === 'restore_download_start') {
        const d = data as { total: number; total_bytes: number };
        downloadProgress = {
          processed: 0,
          total: d.total ?? 0,
          total_bytes: d.total_bytes ?? 0,
          files_written: 0,
          bytes_written: 0,
          errors: 0,
        };
        downloadStatus = {
          tone: 'running',
          label: 'Downloading',
          detail: `${(d.total ?? 0).toLocaleString()} file(s) queued.`,
        };
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
          total_bytes: d.total_bytes,
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
        downloadStatus = {
          tone: 'running',
          label: 'Downloading',
          detail: parts.join(' · '),
        };
      } else if (type === 'restore_download_complete') {
        const d = data as { files_written: number; bytes_written: number; total_bytes: number; errors: number };
        downloadProgress = null;
        downloadStatus = d.errors > 0
          ? {
            tone: 'warn',
            label: 'Complete with warnings',
            detail: `${d.files_written.toLocaleString()} written · ${bytes(d.bytes_written)} · ${d.errors.toLocaleString()} error(s).`,
          }
          : {
            tone: 'ok',
            label: 'Complete',
            detail: `${d.files_written.toLocaleString()} written · ${bytes(d.bytes_written)}.`,
          };
      } else if (type === 'restore_download_failed') {
        const d = data as { files_written: number; bytes_written: number; errors: number; error?: string };
        downloadProgress = null;
        downloadStatus = {
          tone: 'err',
          label: 'Failed',
          detail: [
            `${d.files_written.toLocaleString()} written`,
            bytes(d.bytes_written),
            `${d.errors.toLocaleString()} error(s)`,
            d.error,
          ].filter(Boolean).join(' · '),
        };
      }
    });

    return () => sub.close();
  });

  onDestroy(() => {
    aborted = true;
  });

  function paths(): string[] {
    return raw
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
  }

  function isAbsoluteTargetDir(p: string): boolean {
    return /^([A-Za-z]:[\\/]|[\\/]{2}|[\\/])/.test(p);
  }

  async function doRestore() {
    const p = paths();
    if (p.length === 0) {
      toast.error('enter at least one path');
      return;
    }
    if (!targetDir.trim()) {
      toast.error('enter a target directory');
      return;
    }
    if (!isAbsoluteTargetDir(targetDir.trim())) {
      toast.error('target directory must be an absolute path');
      return;
    }

    loading = true;
    result = null;
    downloadProgress = null;
    downloadStatus = {
      tone: 'running',
      label: 'Submitting',
      detail: 'Starting download…',
    };
    try {
      lastVerifyChecksum = verifyChecksum;
      const next = await api.restoreToDir(p, targetDir.trim(), verifyChecksum);
      if (aborted) return;
      result = next;
      try {
        localStorage.setItem('aws-backup:download-target-dir', targetDir.trim());
      } catch {
        // ignore storage denial
      }
      downloadStatus = next.errors?.length
        ? {
          tone: 'warn',
          label: 'Complete with warnings',
          detail: `${next.files_written.toLocaleString()} written · ${bytes(next.bytes_written)} · ${next.errors.length.toLocaleString()} error(s).`,
        }
        : {
          tone: 'ok',
          label: 'Complete',
          detail: `${next.files_written.toLocaleString()} written · ${bytes(next.bytes_written)}.`,
        };
      toast.success(`Downloaded ${next.files_written.toLocaleString()} file(s).`);
    } catch (e) {
      if (aborted || (e instanceof ApiError && e.kind === 'abort')) return;
      downloadStatus = {
        tone: 'err',
        label: 'Failed',
        detail: String(e),
      };
      toast.error(String(e));
    } finally {
      loading = false;
    }
  }
</script>

<h1>Download, unzip, and verify</h1>

<div class="card">
  <p class="muted">
    Enter source-relative paths from the index and an absolute local destination directory.
    The server will download matching S3 objects, extract zip archives, and verify written
    files against the MD5 stored in the database unless you turn verification off.
  </p>

  <div class="label">Paths (one per line)</div>
  <textarea bind:value={raw} placeholder={"photos\nnotes.txt\narchives/2024"}></textarea>

  <div class="grid">
    <label>
      Target directory
      <input
        type="text"
        bind:value={targetDir}
        placeholder={typeof window !== 'undefined' && window.navigator.platform?.startsWith('Win')
          ? 'C:\\\\restore'
          : '/mnt/restore'}
        class="mono"
      />
    </label>

    <label class="verify">
      <input type="checkbox" bind:checked={verifyChecksum} />
      Verify hashes against stored MD5
    </label>
  </div>

  <div class="actions">
    <button class="primary" onclick={doRestore} disabled={loading} type="button">
      {loading ? 'Downloading…' : 'Download and verify'}
    </button>
    <button
      type="button"
      onclick={() => {
        raw = '/';
        result = null;
      }}
    >
      Select all
    </button>
  </div>

  <div class="statusline" aria-live="polite">
    <span class={`status-pill ${downloadStatus.tone}`}>{downloadStatus.label}</span>
    <span class="status-detail">{downloadStatus.detail}</span>
  </div>

  {#if downloadProgress}
    <div style="margin-top: 0.75rem">
      <ProgressBar
        value={downloadProgress.processed}
        max={downloadProgress.total}
        label="Files processed"
      />
      <div class="muted small" style="margin-top: 0.4rem">
        {downloadProgress.files_written.toLocaleString()} written · {bytes(downloadProgress.bytes_written)}
        {#if downloadProgress.path} · {downloadProgress.path}{/if}
        {#if downloadProgress.error} · {downloadProgress.error}{/if}
      </div>
    </div>
  {/if}
</div>

{#if result}
  <div class="card">
    <div class="label">Result</div>
    <div class="stats">
      <div>
        <div class="muted">Files written</div>
        <div class="big">{result.files_written.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">Bytes written</div>
        <div class="big">{bytes(result.bytes_written)}</div>
      </div>
      <div>
        <div class="muted">Verification</div>
        <div class="big">{lastVerifyChecksum ? 'On' : 'Off'}</div>
      </div>
    </div>

    {#if result.skipped?.length}
      <details open style="margin-top: 0.75rem">
        <summary>{result.skipped.length} skipped path(s)</summary>
        <ul class="mono small">{#each result.skipped as p}<li>{p}</li>{/each}</ul>
      </details>
    {/if}

    {#if result.blocked?.length}
      <details open style="margin-top: 0.75rem">
        <summary>{result.blocked.length} blocked path(s)</summary>
        <ul class="mono small">{#each result.blocked as p}<li>{p}</li>{/each}</ul>
      </details>
    {/if}

    {#if result.errors?.length}
      <details open style="margin-top: 0.75rem" class="err">
        <summary>{result.errors.length} error(s)</summary>
        <ul class="mono small">{#each result.errors as p}<li>{p}</li>{/each}</ul>
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
  .grid {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 1rem;
    margin-top: 1rem;
    align-items: end;
  }
  .verify {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding-bottom: 0.45rem;
    white-space: nowrap;
  }
  input[type="text"] {
    width: 100%;
    margin-top: 0.35rem;
    padding: 0.45rem 0.55rem;
    font-size: 0.95rem;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin-top: 1rem;
  }
  .statusline {
    display: flex;
    gap: 0.6rem;
    align-items: center;
    margin-top: 1rem;
    flex-wrap: wrap;
  }
  .status-pill {
    display: inline-flex;
    align-items: center;
    border-radius: 999px;
    padding: 0.25rem 0.65rem;
    font-size: 0.75rem;
    font-weight: 600;
    letter-spacing: 0.02em;
    border: 1px solid var(--border);
    background: var(--bg);
  }
  .status-pill.idle { color: var(--muted); }
  .status-pill.running { color: var(--accent); }
  .status-pill.ok { color: var(--ok); }
  .status-pill.warn { color: var(--warn); }
  .status-pill.err { color: var(--err); }
  .status-detail {
    font-size: 0.92rem;
    color: var(--muted);
    line-height: 1.4;
  }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
    gap: 0.75rem;
    margin-top: 0.75rem;
  }
  .big { font-size: 1.1rem; font-weight: 600; }
  ul {
    margin: 0.5rem 0 0;
    padding-left: 1.2rem;
    max-height: 320px;
    overflow: auto;
  }
  @media (max-width: 760px) {
    .grid { grid-template-columns: 1fr; }
    .verify { white-space: normal; }
  }
</style>
