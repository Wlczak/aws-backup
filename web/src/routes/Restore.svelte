<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import {
    api,
    subscribeEvents,
    type RestoreEstimate,
    type RestoreJobSummary,
    type RestoreTier,
    type Status,
    type RestoreScanResult,
    type InventoryStatus,
  } from '../lib/api';
  import { bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import { paths as selectionPaths, clear as clearSelection } from '../lib/selection';
  import Skeleton from '../components/Skeleton.svelte';

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
  let restoreStatusTimer: ReturnType<typeof setInterval> | null = null;

  // Reset the double-click confirm if the user edits the inputs between
  // the first and second click — otherwise switching path A → B fires the
  // restore on B with no second confirm. (#209)
  $effect(() => {
    void raw;
    void days;
    confirmTrigger = false;
    estimate = null;
    triggerResult = null;
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
  let inventoryManifestProgress = $state<{ stage: string; processed: number; total: number; manifest_key?: string } | null>(null);

  // Live progress while a "Request retrieval" is in flight — driven by
  // restore_request_* SSE events from RequestRestore. A 5000-file restore
  // issues hundreds of S3 RestoreObject calls, so without this the button
  // just sat in "Requesting…" with no signal anything was happening.
  let triggerProgress = $state<{ processed: number; total: number } | null>(null);

  // Inventory state.
  let inventory = $state<InventoryStatus | null>(null);
  let inventoryFreq = $state<'daily' | 'weekly'>('daily');
  let inventoryBusy = $state(false);
  let inventoryLoaded = $state(false);
  let restoreStatusLoaded = $state(false);
  let restoreJobCurrent = $state<RestoreJobSummary | null>(null);
  let inventoryJobActive = $derived(restoreJobCurrent?.status === 'running' && restoreJobCurrent.kind === 'inventory');

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
    loadRestoreStatus();
    restoreStatusTimer = setInterval(() => {
      void loadRestoreStatus();
    }, 2500);
    const sub = subscribeEvents((event) => {
      switch (event.type) {
        case 'restore_manifest_start':
        case 'restore_manifest_progress':
        case 'restore_manifest_complete':
          inventoryManifestProgress = {
            stage: event.data.stage ?? 'manifest',
            processed: event.data.processed ?? 0,
            total: event.data.total ?? 0,
            manifest_key: event.data.manifest_key,
          };
          break;
        case 'restore_manifest_failed':
          inventoryManifestProgress = null;
          break;
        case 'restore_request_start':
          triggerProgress = { processed: 0, total: event.data.total };
          break;
        case 'restore_request_progress':
          triggerProgress = {
            processed: event.data.processed ?? triggerProgress?.processed ?? 0,
            total: event.data.total ?? triggerProgress?.total ?? 0,
          };
          break;
        case 'restore_request_complete':
          triggerProgress = null;
          break;
        case 'restore_request_failed':
          triggerProgress = null;
          break;
        case 'restore_scan_progress':
          scanProgress = { scanned: event.data.scanned, total: event.data.total, mode: event.data.mode };
          break;
        case 'restore_scan_complete':
        case 'restore_scan_failed':
          scanProgress = null;
          break;
      }
    });
    return () => {
      sub.close();
      if (restoreStatusTimer) clearInterval(restoreStatusTimer);
    };
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
    } finally {
      inventoryLoaded = true;
    }
  }

  async function loadRestoreStatus() {
    try {
      const status = await api.status();
      if (aborted) return;
      applyRestoreStatus(status);
    } catch {
      if (aborted) return;
    } finally {
      restoreStatusLoaded = true;
    }
  }

  function applyRestoreStatus(status: Status) {
    const job = status.restore_job_current ?? status.restore_job_last;
    restoreJobCurrent = job ?? null;
    if (!job) return;
    if (job.status === 'running') {
      if (job.kind === 'trigger') {
        triggerProgress = { processed: job.processed, total: job.total };
        triggerResult = null;
      } else if (job.kind === 'inventory') {
        if (job.phase === 'manifest') {
          inventoryManifestProgress = {
            stage: 'manifest',
            processed: job.processed,
            total: job.total,
            manifest_key: job.manifest_key,
          };
          scanResult = null;
        } else if (job.phase === 'scan') {
          scanProgress = { scanned: job.scanned || job.processed, total: job.total, mode: 'inventory' };
          inventoryManifestProgress = null;
        }
      }
      return;
    }

    if (job.kind === 'trigger') {
      triggerProgress = null;
      if (job.status === 'completed') {
        triggerResult = triggerResultFromJob(job);
      }
    } else if (job.kind === 'inventory') {
      inventoryManifestProgress = null;
      scanProgress = null;
      if (job.status === 'completed') {
        scanResult = scanResultFromJob(job);
      }
    }
  }

  function triggerResultFromJob(job: RestoreJobSummary) {
    return {
      keys_requested: job.keys_requested ?? 0,
      keys_already_in_progress: job.keys_already_in_progress ?? 0,
      keys_already_available: job.keys_already_available ?? 0,
      files_affected: job.files_affected ?? 0,
      bytes_affected: job.bytes_affected ?? 0,
      files_skipped_in_progress: job.files_skipped_in_progress ?? 0,
      bytes_skipped_in_progress: job.bytes_skipped_in_progress ?? 0,
      files_skipped_restored: job.files_skipped_restored ?? 0,
      bytes_skipped_restored: job.bytes_skipped_restored ?? 0,
      unknown_paths: job.unknown_paths ?? [],
      errors: job.error_message ? [job.error_message] : [],
    };
  }

  function scanResultFromJob(job: RestoreJobSummary): RestoreScanResult {
    return {
      mode: 'inventory',
      scanned: job.scanned,
      updated: job.updated,
      errors: job.errors,
      duration_ns: 0,
    };
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
    if (inventoryJobActive) {
      toast.info(`Inventory sync job #${restoreJobCurrent?.id} is already running.`);
      return;
    }
    inventoryBusy = true;
    scanResult = null;
    try {
      const job = await api.inventorySync();
      toast.info(`Inventory sync started as job #${job.restore_job_id}.`);
    } catch (e) {
      toast.error(String(e));
    } finally {
      inventoryBusy = false;
    }
  }

  function paths(): string[] {
    const parsed = raw
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean);
    if (parsed.includes('/')) return ['/'];
    return parsed;
  }

  async function doEstimate() {
    const p = paths();
    if (p.length === 0) {
      toast.error('enter at least one path, or / for all files');
      return;
    }
    if (!Number.isInteger(days) || days < 1 || days > 180) {
      toast.error('days must be an integer between 1 and 180');
      return;
    }
    loading = true;
    estimate = null;
    try {
      estimate = await api.restoreEstimate(p, days, tier);
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
    if (!Number.isInteger(days) || days < 1 || days > 180) {
      toast.error('days must be an integer between 1 and 180');
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
      const job = await api.restoreTrigger(p, days, tier);
      toast.info(`Restore request started as job #${job.restore_job_id}.`);
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
  {:else if !restoreStatusLoaded}
    <Skeleton lines={1} widths={['42%']} height="0.95rem" />
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
  {:else if !inventoryLoaded}
    <div class="skeleton-card" style="margin-top: 0.5rem">
      <div class="stats">
        <div>
          <Skeleton lines={1} widths={['4rem']} height="0.9rem" />
          <Skeleton lines={1} widths={['6rem']} height="1.35rem" />
        </div>
        <div>
          <Skeleton lines={1} widths={['5rem']} height="0.9rem" />
          <Skeleton lines={1} widths={['5.5rem']} height="1.35rem" />
        </div>
        <div>
          <Skeleton lines={1} widths={['6rem']} height="0.9rem" />
          <Skeleton lines={1} widths={['9rem']} height="1.35rem" />
        </div>
      </div>
      <Skeleton lines={1} widths={['58%']} height="0.95rem" />
    </div>
  {/if}
  {#if inventoryManifestProgress}
    {@const manifestPct = inventoryManifestProgress.total > 0
      ? Math.min(100, Math.round((inventoryManifestProgress.processed / inventoryManifestProgress.total) * 100))
      : 0}
    <p class="muted small" style="margin-top: 0.75rem">
      Inventory manifest {inventoryManifestProgress.stage}: {inventoryManifestProgress.processed.toLocaleString()} / {inventoryManifestProgress.total.toLocaleString()}
      {#if inventoryManifestProgress.manifest_key}
        <span class="mono">({inventoryManifestProgress.manifest_key})</span>
      {/if}
    </p>
    <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={manifestPct} style="margin-top: 0.35rem">
      <div class="fill" style="width: {manifestPct}%"></div>
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
      <button onclick={doInventorySync} disabled={inventoryBusy || scanBusy || inventoryJobActive} type="button">
        Sync from inventory
      </button>
    {/if}
  </div>
  {#if inventoryJobActive}
    <div class="muted small" style="margin-top: 0.4rem">
      Inventory sync job #{restoreJobCurrent?.id} is already running; the button stays disabled until it finishes.
    </div>
  {/if}
</div>

<div class="card">
  <div class="label">Paths (one per line)</div>
  <p class="muted">
    Enter top-level directories or full file paths. Prefix match is applied — e.g.
    <code class="mono">photos</code> selects every file under <code class="mono">photos/</code>.
    Enter <code class="mono">/</code> to select <strong>all files</strong>.
  </p>
  <textarea bind:value={raw} placeholder={"photos\ndocs/2024\nfamily-archive.zip"}></textarea>

  <div class="label" style="margin-top: 0.75rem">Days to keep restored (1–180)</div>
  <p class="muted">
    AWS will keep the thawed copy in standard storage for this many days, and
    you'll pay S3 Standard storage for that period. After that, the object
    reverts to the archive class and a new restore must be issued. The actual
    download is done out-of-band once status flips to
    <code class="mono">restored</code> — track via "Drain SQS now" or
    "Scan pending only" above.
  </p>
  <div class="restore-controls">
    <select bind:value={tier} class="mono" aria-label="Restore tier">
      <option value="bulk">Bulk - lowest cost, up to 48h</option>
      <option value="standard">Standard - faster, up to 12h</option>
    </select>
    <input type="number" bind:value={days} min="1" max="180" step="1" class="mono daysinput" />
  </div>

  <div class="actions">
    <button class="primary" onclick={doEstimate} disabled={loading} type="button">
      {loading ? 'Estimating…' : 'Estimate cost'}
    </button>
    <button onclick={doTrigger} disabled={loading || !estimate || !!triggerProgress} type="button">
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

{#if loading && !triggerProgress && !triggerResult && !estimate}
  <div class="card">
    <div class="label">Working</div>
    <div class="skeleton-card">
      <Skeleton lines={1} widths={['72%']} height="0.95rem" />
      <div class="stats">
        {#each Array(5) as _}
          <div>
            <Skeleton lines={1} widths={['5rem']} height="0.85rem" />
            <Skeleton lines={1} widths={['4rem']} height="1.3rem" />
          </div>
        {/each}
      </div>
      <Skeleton lines={1} widths={['86%']} height="0.95rem" />
    </div>
  </div>
{/if}

{#if scanResult && scanResult.mode === 'inventory'}
  <div class="card">
    <div class="label">Inventory sync result</div>
    <p class="muted">
      Inventory sync completed: {scanResult.scanned.toLocaleString()} keys HEADed,
      {scanResult.updated.toLocaleString()} updated, {scanResult.errors.toLocaleString()} error(s).
    </p>
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
        <tr>
          <td>Standard storage ({days} day(s) × $0.023 / GB-month)</td>
          <td class="mono">${estimate.storage_fee_usd.toFixed(2)}</td>
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
