<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import {
    api,
    subscribeEvents,
    type RestoreEstimate,
    type RestoreTier,
    type RestoreScanResult,
    type InventoryStatus,
  } from '../lib/api';
  import { bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import { paths as selectionPaths, clear as clearSelection } from '../lib/selection';

  let raw = $state('');
  let tier = $state<RestoreTier>('bulk');
  let days = $state(7);
  let estimate = $state<RestoreEstimate | null>(null);
  let triggerResult = $state<{
    keys_requested: number;
    keys_already_in_progress: number;
    keys_already_available: number;
    files_affected: number;
    bytes_affected: number;
    files_skipped_in_progress: number;
    bytes_skipped_in_progress: number;
    files_skipped_restored: number;
    bytes_skipped_restored: number;
    unknown_paths?: string[];
    errors?: string[];
  } | null>(null);
  let loading = $state(false);
  let confirmTrigger = $state(false);
  let syncing = $state(false);

  // Reset the double-click confirm if the user edits the inputs between
  // the first and second click — otherwise switching path A → B fires the
  // restore on B with no second confirm. (#209)
  $effect(() => {
    void raw;
    void days;
    confirmTrigger = false;
  });

  $effect(() => {
    void tier;
    confirmTrigger = false;
    estimate = null;
    triggerResult = null;
  });

  // Restore-status sync state.
  let scanBusy = $state(false);
  let scanResult = $state<RestoreScanResult | null>(null);
  let scanProgress = $state<{ scanned: number; total: number; mode: string } | null>(null);

  // Live progress while a "Request retrieval" is in flight — driven by
  // restore_request_* SSE events from RequestRestore. A 5000-file restore
  // issues hundreds of S3 RestoreObject calls, so without this the button
  // just sat in "Requesting…" with no signal anything was happening.
  let triggerProgress = $state<{ processed: number; total: number } | null>(null);

  // Inventory state.
  let inventory = $state<InventoryStatus | null>(null);
  let inventoryFreq = $state<'daily' | 'weekly'>('daily');
  let inventoryBusy = $state(false);

  // Guard async writes against assigning to torn-down state if the route
  // unmounts mid-fetch — same pattern as Files.svelte. (#202)
  let aborted = false;
  onDestroy(() => { aborted = true; });

  onMount(() => {
    const pre = selectionPaths();
    if (pre.length > 0) {
      raw = pre.join('\n');
      toast.info(`Pre-filled ${pre.length} path(s) from Files selection.`);
      clearSelection();
    }
    // Best-effort load of inventory status; absence is normal.
    loadInventory();
    const sub = subscribeEvents((type, data) => {
      if (type === 'restore_scan_progress') {
        const d = data as { scanned: number; total: number; mode: string };
        scanProgress = { scanned: d.scanned, total: d.total, mode: d.mode };
      } else if (type === 'restore_scan_complete') {
        scanProgress = null;
      } else if (type === 'restore_scan_failed') {
        scanProgress = null;
      } else if (type === 'restore_request_start') {
        const d = data as { total: number };
        triggerProgress = { processed: 0, total: d.total };
      } else if (type === 'restore_request_progress') {
        const d = data as { processed: number; total: number };
        triggerProgress = { processed: d.processed, total: d.total };
      } else if (type === 'restore_request_complete' || type === 'restore_request_failed') {
        triggerProgress = null;
      }
    });
    return () => sub.close();
  });

  async function loadInventory() {
    try {
      const inv = await api.inventoryGet();
      if (aborted) return;
      inventory = inv;
      if (inv.frequency === 'Weekly') inventoryFreq = 'weekly';
      else inventoryFreq = 'daily';
    } catch {
      if (aborted) return;
      inventory = null;
    }
  }

  async function doScanFull() {
    scanBusy = true;
    scanResult = null;
    try {
      scanResult = await api.restoreScanFull();
      toast.success(`Full scan: ${scanResult.scanned} HEADed, ${scanResult.updated} updated, ${scanResult.errors} error(s).`);
    } catch (e) {
      toast.error(String(e));
    } finally {
      scanBusy = false;
    }
  }

  async function doScanPending() {
    scanBusy = true;
    scanResult = null;
    try {
      scanResult = await api.restoreScanPending();
      if (scanResult.scanned === 0) {
        toast.info('No files in restore-pending state.');
      } else {
        toast.success(`Pending scan: ${scanResult.scanned} HEADed, ${scanResult.updated} updated.`);
      }
    } catch (e) {
      toast.error(String(e));
    } finally {
      scanBusy = false;
    }
  }

  async function doInventoryEnable() {
    inventoryBusy = true;
    try {
      inventory = await api.inventoryPut(inventoryFreq);
      toast.success(`Inventory enabled (${inventory.frequency}).`);
    } catch (e) {
      toast.error(String(e));
    } finally {
      inventoryBusy = false;
    }
  }

  async function doInventoryDisable() {
    inventoryBusy = true;
    try {
      await api.inventoryDelete();
      inventory = { enabled: false };
      toast.success('Inventory disabled.');
    } catch (e) {
      toast.error(String(e));
    } finally {
      inventoryBusy = false;
    }
  }

  async function doInventorySync() {
    inventoryBusy = true;
    scanResult = null;
    try {
      scanResult = await api.inventorySync();
      toast.success(`Inventory sync: ${scanResult.scanned} keys HEADed, ${scanResult.updated} updated.`);
    } catch (e) {
      toast.error(String(e));
    } finally {
      inventoryBusy = false;
    }
  }

  function paths(): string[] {
    return raw
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
  }

  async function doEstimate() {
    const p = paths();
    if (p.length === 0) {
      toast.error('enter at least one path');
      return;
    }
    loading = true;
    estimate = null;
    try {
      estimate = await api.restoreEstimate(p, tier);
    } catch (e) {
      toast.error(String(e));
    } finally {
      loading = false;
    }
  }

  async function doSyncStatus() {
    syncing = true;
    try {
      const r = await api.restoreSyncStatus();
      if (r.processed === 0) {
        toast.info('No new restore events on the queue.');
      } else {
        toast.success(`Synced ${r.processed} restore event(s).`);
      }
    } catch (e) {
      toast.error(String(e));
    } finally {
      syncing = false;
    }
  }

  async function doTrigger() {
    const p = paths();
    if (!Number.isInteger(days) || days < 1 || days > 30) {
      toast.error('days must be an integer between 1 and 30');
      confirmTrigger = false;
      return;
    }
    if (!confirmTrigger) {
      confirmTrigger = true;
      return;
    }
    loading = true;
    triggerResult = null;
    try {
      triggerResult = await api.restoreTrigger(p, days, tier);
      const r = triggerResult;
      const parts: string[] = [];
      if (r.keys_requested > 0) parts.push(`${r.keys_requested} requested`);
      if (r.keys_already_in_progress > 0) parts.push(`${r.keys_already_in_progress} already thawing`);
      if (r.keys_already_available > 0) parts.push(`${r.keys_already_available} already available`);
      const skipped = r.files_skipped_in_progress + r.files_skipped_restored;
      if (skipped > 0) parts.push(`${skipped.toLocaleString()} skipped (already thawed)`);
      const summary = parts.length > 0 ? parts.join(' · ') : 'no keys to restore';
      toast.success(`Retrieval ${summary} for ${r.files_affected.toLocaleString()} file(s).`);
    } catch (e) {
      toast.error(String(e));
    } finally {
      loading = false;
      confirmTrigger = false;
    }
  }
</script>

<h1>Restore from Glacier Deep Archive</h1>

<div class="card">
  <div class="label">Restore status sync</div>
  <p class="muted">
    Three ways to reconcile local restore status with what S3 actually reports.
    The background SQS poller already updates state on its own cadence — these
    buttons force an immediate refresh.
  </p>
  <div class="actions">
    <button onclick={doSyncStatus} disabled={syncing || scanBusy} type="button">
      {syncing ? 'Draining…' : 'Drain SQS now'}
    </button>
    <button onclick={doScanPending} disabled={scanBusy || syncing} type="button">
      {scanBusy ? 'Scanning…' : 'Scan pending only'}
    </button>
    <button onclick={doScanFull} disabled={scanBusy || syncing} type="button">
      {scanBusy ? 'Scanning…' : 'Full HEAD scan'}
    </button>
  </div>
  {#if scanProgress}
    <p class="muted" style="margin-top: 0.5rem">
      {scanProgress.mode}: {scanProgress.scanned.toLocaleString()} / {scanProgress.total.toLocaleString()} HEADed
    </p>
  {/if}
</div>

<div class="card">
  <div class="label">S3 Inventory</div>
  <p class="muted">
    A scheduled S3 inventory report enumerates every key in the bucket so the
    "Sync from inventory" action can refresh restore status without paying for
    a full ListObjectsV2 sweep. Reports are written under
    <code class="mono">_inventory/</code> on the same backup bucket. First report
    lands within 24–48&nbsp;h after enabling.
  </p>
  {#if inventory}
    <div class="stats" style="margin-top: 0.5rem">
      <div>
        <div class="muted">State</div>
        <div class="big">{inventory.enabled ? 'Enabled' : 'Disabled'}</div>
      </div>
      {#if inventory.enabled}
        <div>
          <div class="muted">Frequency</div>
          <div class="big">{inventory.frequency ?? '—'}</div>
        </div>
        <div>
          <div class="muted">Destination</div>
          <div class="mono small">{inventory.destination ?? '—'}</div>
        </div>
      {/if}
    </div>
  {/if}
  <div class="actions">
    <select bind:value={inventoryFreq} disabled={inventoryBusy}>
      <option value="daily">Daily</option>
      <option value="weekly">Weekly</option>
    </select>
    <button onclick={doInventoryEnable} disabled={inventoryBusy} type="button">
      {inventory?.enabled ? 'Update' : 'Enable'}
    </button>
    {#if inventory?.enabled}
      <button onclick={doInventoryDisable} disabled={inventoryBusy} type="button">
        Disable
      </button>
      <button onclick={doInventorySync} disabled={inventoryBusy || scanBusy} type="button">
        Sync from inventory
      </button>
    {/if}
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

  <div class="label" style="margin-top: 0.75rem">Days to keep restored (1–30)</div>
  <p class="muted">
    AWS will keep the thawed copy in standard storage for this many days. After
    that, the object reverts to the archive class and a new restore must be
    issued. The actual download is done out-of-band once status flips to
    <code class="mono">restored</code> — track via "Drain SQS now" or
    "Scan pending only" above.
  </p>
  <div class="restore-controls">
    <select bind:value={tier} class="mono" aria-label="Restore tier">
      <option value="bulk">Bulk - lowest cost, up to 48h</option>
      <option value="standard">Standard - faster, up to 12h</option>
    </select>
    <input type="number" bind:value={days} min="1" max="30" step="1" class="mono daysinput" />
  </div>

  <div class="actions">
    <button class="primary" onclick={doEstimate} disabled={loading} type="button">
      {loading ? 'Estimating…' : 'Estimate cost'}
    </button>
    <button onclick={doTrigger} disabled={loading || !estimate} type="button">
      {#if loading && triggerProgress}
        Requesting… {triggerProgress.processed.toLocaleString()} / {triggerProgress.total.toLocaleString()}
      {:else if loading}
        Requesting…
      {:else if confirmTrigger}
        Click again to confirm
      {:else}
        Request retrieval
      {/if}
    </button>
    <button type="button" onclick={() => { raw = '/'; estimate = null; }} title="Select all files">
      Select all
    </button>
  </div>
  {#if triggerProgress}
    {@const pct = triggerProgress.total > 0
      ? Math.min(100, Math.round((triggerProgress.processed / triggerProgress.total) * 100))
      : 0}
    <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={pct} style="margin-top: 0.75rem">
      <div class="fill" style="width: {pct}%"></div>
    </div>
    <p class="muted small" style="margin-top: 0.25rem">
      Issuing S3 RestoreObject calls — {pct}% of {triggerProgress.total.toLocaleString()} key(s).
    </p>
  {/if}
</div>

{#if triggerResult}
  <div class="card">
    <div class="label">Retrieval request result</div>
    <p class="muted">
      AWS has accepted the restore request. Glacier objects typically thaw
      within about {tier === 'bulk' ? '48 h' : '12 h'} for the selected tier.
      The matching files are now marked <code class="mono">in_progress</code>; status flips to
      <code class="mono">restored</code> once SQS notifies us or a HEAD scan
      observes <code class="mono">x-amz-restore</code>.
    </p>
    <div class="stats">
      <div><div class="muted">Keys requested</div><div class="big">{triggerResult.keys_requested.toLocaleString()}</div></div>
      <div><div class="muted">Already thawing</div><div class="big">{triggerResult.keys_already_in_progress.toLocaleString()}</div></div>
      <div><div class="muted">Already available</div><div class="big">{triggerResult.keys_already_available.toLocaleString()}</div></div>
      <div><div class="muted">Files affected</div><div class="big">{triggerResult.files_affected.toLocaleString()}</div></div>
      <div><div class="muted">Data</div><div class="big">{bytes(triggerResult.bytes_affected)}</div></div>
    </div>
    {#if triggerResult.files_skipped_in_progress > 0 || triggerResult.files_skipped_restored > 0}
      <p class="muted small" style="margin-top: 0.75rem">
        Skipped (already thawed, no fresh request issued):
        {#if triggerResult.files_skipped_restored > 0}
          {triggerResult.files_skipped_restored.toLocaleString()} restored ({bytes(triggerResult.bytes_skipped_restored)}){#if triggerResult.files_skipped_in_progress > 0}, {/if}
        {/if}
        {#if triggerResult.files_skipped_in_progress > 0}
          {triggerResult.files_skipped_in_progress.toLocaleString()} thawing ({bytes(triggerResult.bytes_skipped_in_progress)})
        {/if}
      </p>
    {/if}
    {#if triggerResult.unknown_paths?.length}
      <details style="margin-top: 0.75rem">
        <summary>{triggerResult.unknown_paths.length} unknown path(s)</summary>
        <ul class="mono small">{#each triggerResult.unknown_paths as p}<li>{p}</li>{/each}</ul>
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

{#if estimate}
  <div class="card">
    <div class="label">Estimate</div>
    <div class="stats">
      <div>
        <div class="muted">S3 objects</div>
        <div class="big">{estimate.file_count.toLocaleString()}</div>
      </div>
      <div>
        <div class="muted">Data</div>
        <div class="big">{bytes(estimate.total_bytes)}</div>
      </div>
      <div>
        <div class="muted">Wait</div>
        <div class="big">
          {#if estimate.wait_hours_min === estimate.wait_hours_max}
            {estimate.wait_hours_min}h
          {:else}
            {estimate.wait_hours_min}–{estimate.wait_hours_max}h
          {/if}
        </div>
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
        <tr>
          <td>Request fees ({estimate.file_count.toLocaleString()} object(s) × {tier === 'bulk' ? '$0.025 / 1000' : '$0.11 / 1000'})</td>
          <td class="mono">${estimate.request_fee_usd.toFixed(2)}</td>
        </tr>
        <tr>
          <td>Retrieval ({bytes(estimate.total_bytes)} × {tier === 'bulk' ? '$0.003 / GB' : '$0.02 / GB'})</td>
          <td class="mono">${estimate.retrieval_fee_usd.toFixed(2)}</td>
        </tr>
        <tr><td>Egress (first 100 GB free, then $0.09 / GB)</td><td class="mono">${estimate.egress_fee_usd.toFixed(2)}</td></tr>
        <tr><td><strong>Total</strong></td><td class="mono"><strong>${estimate.total_fee_usd.toFixed(2)}</strong></td></tr>
      </tbody>
    </table>

    {#if estimate.already_in_progress_count > 0 || estimate.already_restored_count > 0}
      <p class="muted small" style="margin-top: 0.75rem">
        Excluded from this estimate:
        {#if estimate.already_restored_count > 0}
          <strong>{estimate.already_restored_count.toLocaleString()}</strong> file(s)
          ({bytes(estimate.already_restored_bytes)}) already restored{#if estimate.already_in_progress_count > 0}, {/if}
        {/if}
        {#if estimate.already_in_progress_count > 0}
          <strong>{estimate.already_in_progress_count.toLocaleString()}</strong> file(s)
          ({bytes(estimate.already_in_progress_bytes)}) currently thawing
        {/if}
        — re-issuing on these would extend their AWS expiry and re-bill retrieval, so they're skipped.
      </p>
    {/if}

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
  .warn { color: var(--warn); }
  .label { font-size: 0.8rem; color: var(--muted); margin-bottom: 0.25rem; }
  textarea {
    width: 100%;
    min-height: 160px;
    font-family: ui-monospace, monospace;
    font-size: 0.9rem;
    margin-top: 0.5rem;
  }
  .daysinput {
    width: 6rem;
    padding: 0.4rem 0.5rem;
    font-size: 0.9rem;
  }
  .restore-controls {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    align-items: center;
  }
  .restore-controls select {
    min-width: 16rem;
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
  .bar { height: 6px; background: var(--bg); border: 1px solid var(--border); border-radius: 3px; overflow: hidden; }
  .fill { height: 100%; background: var(--accent); transition: width 0.2s ease; }
</style>
