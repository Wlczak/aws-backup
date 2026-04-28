<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type RestoreEstimate } from '../lib/api';
  import { bytes } from '../lib/format';
  import { paths as selectionPaths, clear as clearSelection } from '../lib/selection';

  let raw = $state('');
  let targetDir = $state('');
  let estimate = $state<RestoreEstimate | null>(null);
  let triggerResult = $state<{
    files_written: number;
    bytes_written: number;
    skipped?: string[];
    errors?: string[];
  } | null>(null);
  let err = $state('');
  let info = $state('');
  let loading = $state(false);
  let confirmTrigger = $state(false);
  let syncing = $state(false);

  onMount(() => {
    const pre = selectionPaths();
    if (pre.length > 0) {
      raw = pre.join('\n');
      info = `Pre-filled ${pre.length} path(s) from Files selection.`;
      clearSelection();
    }
  });

  function paths(): string[] {
    return raw
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
  }

  async function doEstimate() {
    const p = paths();
    if (p.length === 0) {
      err = 'enter at least one path';
      return;
    }
    loading = true;
    err = '';
    info = '';
    estimate = null;
    try {
      estimate = await api.restoreEstimate(p);
    } catch (e) {
      err = String(e);
    } finally {
      loading = false;
    }
  }

  async function doSyncStatus() {
    syncing = true;
    err = '';
    info = '';
    try {
      const r = await api.restoreSyncStatus();
      info =
        r.processed === 0
          ? 'No new restore events on the queue.'
          : `Synced ${r.processed} restore event(s).`;
    } catch (e) {
      err = String(e);
    } finally {
      syncing = false;
    }
  }

  async function doTrigger() {
    const p = paths();
    if (!targetDir.trim()) {
      err = 'target directory is required';
      confirmTrigger = false;
      return;
    }
    if (!confirmTrigger) {
      confirmTrigger = true;
      return;
    }
    loading = true;
    err = '';
    info = '';
    triggerResult = null;
    try {
      triggerResult = await api.restoreTrigger(p, targetDir.trim());
      info = `Restored ${triggerResult.files_written.toLocaleString()} file(s) (${bytes(triggerResult.bytes_written)}).`;
    } catch (e) {
      err = String(e);
    } finally {
      loading = false;
      confirmTrigger = false;
    }
  }
</script>

<h1>Restore from Glacier Deep Archive</h1>

<div class="card">
  <div class="label">Restore status</div>
  <p class="muted">
    Drains the SQS queue of pending S3 Glacier restore events and applies them to
    the local index. The background poller already does this on its own cadence —
    use this button to force an immediate sync after a Glacier restore lands.
  </p>
  <div class="actions">
    <button onclick={doSyncStatus} disabled={syncing} type="button">
      {syncing ? 'Syncing…' : 'Sync restore status'}
    </button>
  </div>
</div>

<div class="card">
  <div class="label">Paths (one per line)</div>
  <p class="muted">
    Enter top-level directories or full file paths. Prefix match is applied — e.g.
    <code class="mono">photos</code> selects every file under <code class="mono">photos/</code>.
    Enter <code class="mono">/</code> to select <strong>all files</strong>.
  </p>
  <textarea bind:value={raw} placeholder={"photos\ndocs/2024\nfamily-archive.zip"}></textarea>

  <div class="label" style="margin-top: 0.75rem">Target directory (absolute path)</div>
  <p class="muted">Restored files are written here, preserving their source-relative paths.</p>
  <input type="text" bind:value={targetDir} placeholder="/home/me/restored" class="mono targetdir" />

  <div class="actions">
    <button class="primary" onclick={doEstimate} disabled={loading} type="button">
      {loading ? 'Estimating…' : 'Estimate cost'}
    </button>
    <button onclick={doTrigger} disabled={loading || !estimate} type="button">
      {confirmTrigger ? 'Click again to confirm' : 'Download & restore'}
    </button>
    <button type="button" onclick={() => { raw = '/'; estimate = null; }} title="Select all files">
      Select all
    </button>
  </div>
</div>

{#if triggerResult}
  <div class="card">
    <div class="label">Restore result</div>
    <div class="stats">
      <div><div class="muted">Files written</div><div class="big">{triggerResult.files_written.toLocaleString()}</div></div>
      <div><div class="muted">Bytes</div><div class="big">{bytes(triggerResult.bytes_written)}</div></div>
    </div>
    {#if triggerResult.skipped?.length}
      <details style="margin-top: 0.75rem">
        <summary>{triggerResult.skipped.length} skipped</summary>
        <ul class="mono small">{#each triggerResult.skipped as p}<li>{p}</li>{/each}</ul>
      </details>
    {/if}
    {#if triggerResult.errors?.length}
      <details open style="margin-top: 0.5rem" class="err">
        <summary>{triggerResult.errors.length} error(s)</summary>
        <ul class="mono small">{#each triggerResult.errors as p}<li>{p}</li>{/each}</ul>
      </details>
    {/if}
  </div>
{/if}

{#if err}<div class="card err">{err}</div>{/if}
{#if info}<div class="card info">{info}</div>{/if}

{#if estimate}
  <div class="card">
    <div class="label">Estimate</div>
    <div class="stats">
      <div>
        <div class="muted">Files</div>
        <div class="big">{estimate.file_count.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">Data</div>
        <div class="big">{bytes(estimate.total_bytes)}</div>
      </div>
      <div>
        <div class="muted">Wait</div>
        <div class="big">{estimate.wait_hours_min}–{estimate.wait_hours_max}h</div>
      </div>
      <div>
        <div class="muted">Total (USD)</div>
        <div class="big">${estimate.total_fee_usd.toFixed(2)}</div>
      </div>
    </div>

    <table style="margin-top: 1rem">
      <thead>
        <tr><th>Fee</th><th>USD</th></tr>
      </thead>
      <tbody>
        <tr><td>Request fees ({estimate.file_count.toLocaleString()} files × $0.10 / 1000)</td><td class="mono">${estimate.request_fee_usd.toFixed(2)}</td></tr>
        <tr><td>Retrieval ({bytes(estimate.total_bytes)} × $0.02 / GB)</td><td class="mono">${estimate.retrieval_fee_usd.toFixed(2)}</td></tr>
        <tr><td>Egress (first 100 GB free, then $0.09 / GB)</td><td class="mono">${estimate.egress_fee_usd.toFixed(2)}</td></tr>
        <tr><td><strong>Total</strong></td><td class="mono"><strong>${estimate.total_fee_usd.toFixed(2)}</strong></td></tr>
      </tbody>
    </table>

    {#if estimate.unknown_paths?.length}
      <div class="warn" style="margin-top: 0.75rem">
        Paths not found in index:
        <ul>
          {#each estimate.unknown_paths as p}<li class="mono">{p}</li>{/each}
        </ul>
      </div>
    {/if}
  </div>
{/if}

<style>
  .err { color: var(--err); border-color: var(--err); }
  .info { color: var(--accent); border-color: var(--accent); }
  .warn { color: var(--warn); }
  .label { font-size: 0.8rem; color: var(--muted); margin-bottom: 0.25rem; }
  textarea {
    width: 100%;
    min-height: 160px;
    font-family: ui-monospace, monospace;
    font-size: 0.9rem;
    margin-top: 0.5rem;
  }
  .targetdir {
    width: 100%;
    padding: 0.4rem 0.5rem;
    font-size: 0.9rem;
  }
  .small { font-size: 0.85rem; }
  details ul { margin: 0.5rem 0 0; padding-left: 1.25rem; max-height: 220px; overflow: auto; }
  .actions { display: flex; gap: 0.5rem; margin-top: 0.75rem; }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 1rem;
  }
  .big { font-size: 1.3rem; font-weight: 500; }
</style>
