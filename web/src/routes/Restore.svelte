<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import {
    api,
    subscribeEvents,
    type RestoreEstimate,
    type RestoreScanResult,
    type InventoryStatus,
  } from '../lib/api';
  import { bytes } from '../lib/format';
  import { toast } from '../lib/toast';
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
  let info = $state('');
  let loading = $state(false);
  let confirmTrigger = $state(false);
  let syncing = $state(false);

  // Reset the double-click confirm if the user edits the inputs between
  // the first and second click — otherwise switching path A → B fires the
  // restore on B with no second confirm. (#209)
  $effect(() => {
    void raw;
    void targetDir;
    confirmTrigger = false;
  });

  // Restore-status sync state.
  let scanBusy = $state(false);
  let scanResult = $state<RestoreScanResult | null>(null);
  let scanProgress = $state<{ scanned: number; total: number; mode: string } | null>(null);

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
      info = `Pre-filled ${pre.length} path(s) from Files selection.`;
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
    info = '';
    scanResult = null;
    try {
      scanResult = await api.restoreScanFull();
      info = `Full scan: ${scanResult.scanned} HEADed, ${scanResult.updated} updated, ${scanResult.errors} error(s).`;
    } catch (e) {
      toast.error(String(e));
    } finally {
      scanBusy = false;
    }
  }

  async function doScanPending() {
    scanBusy = true;
    info = '';
    scanResult = null;
    try {
      scanResult = await api.restoreScanPending();
      info =
        scanResult.scanned === 0
          ? 'No files in restore-pending state.'
          : `Pending scan: ${scanResult.scanned} HEADed, ${scanResult.updated} updated.`;
    } catch (e) {
      toast.error(String(e));
    } finally {
      scanBusy = false;
    }
  }

  async function doInventoryEnable() {
    inventoryBusy = true;
    info = '';
    try {
      inventory = await api.inventoryPut(inventoryFreq);
      info = `Inventory enabled (${inventory.frequency}).`;
    } catch (e) {
      toast.error(String(e));
    } finally {
      inventoryBusy = false;
    }
  }

  async function doInventoryDisable() {
    inventoryBusy = true;
    info = '';
    try {
      await api.inventoryDelete();
      inventory = { enabled: false };
      info = 'Inventory disabled.';
    } catch (e) {
      toast.error(String(e));
    } finally {
      inventoryBusy = false;
    }
  }

  async function doInventorySync() {
    inventoryBusy = true;
    info = '';
    scanResult = null;
    try {
      scanResult = await api.inventorySync();
      info = `Inventory sync: ${scanResult.scanned} keys HEADed, ${scanResult.updated} updated.`;
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
    info = '';
    estimate = null;
    try {
      estimate = await api.restoreEstimate(p);
    } catch (e) {
      toast.error(String(e));
    } finally {
      loading = false;
    }
  }

  async function doSyncStatus() {
    syncing = true;
    info = '';
    try {
      const r = await api.restoreSyncStatus();
      info =
        r.processed === 0
          ? 'No new restore events on the queue.'
          : `Synced ${r.processed} restore event(s).`;
    } catch (e) {
      toast.error(String(e));
    } finally {
      syncing = false;
    }
  }

  async function doTrigger() {
    const p = paths();
    if (!targetDir.trim()) {
      toast.error('target directory is required');
      confirmTrigger = false;
      return;
    }
    if (!confirmTrigger) {
      confirmTrigger = true;
      return;
    }
    loading = true;
    info = '';
    triggerResult = null;
    try {
      triggerResult = await api.restoreTrigger(p, targetDir.trim());
      info = `Restored ${triggerResult.files_written.toLocaleString()} file(s) (${bytes(triggerResult.bytes_written)}).`;
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
