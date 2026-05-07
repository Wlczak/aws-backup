<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api, subscribeEvents, type Status, type FileStats } from '../lib/api';
  import { bytes, formatDate, relativeTime, expiresIn } from '../lib/format';
  import { toast } from '../lib/toast';
  import StatusBadge from '../components/StatusBadge.svelte';
  import ProgressBar from '../components/ProgressBar.svelte';

  let status = $state<Status | null>(null);
  let stats = $state<FileStats | null>(null);
  let logLines = $state<string[]>([]);
  let scanSeen = $state(0);
  let scanNew = $state(0);
  let scanChanged = $state(0);
  let scanActive = $state(false);
  // `total` is authoritative from the `upload_plan` SSE event; the other
  // counters are derived from `itemProgress` so an SSE replay or
  // reconnect can't double-count them. (#199)
  let uploadsTotal = $state(0);
  // Persisted upload count from the run row, refreshed on every status
  // poll. After a page reload mid-run, itemProgress is empty (it only
  // tracks events received this session) so the progress bar would
  // otherwise reset to 0 / N. The engine writes files_uploaded to the
  // run row on every group completion, so this seed catches up within
  // one poll interval and the displayed count stays correct across
  // reloads. (#remember-completed)
  let seededFilesUploaded = $state(0);
  let triggering = $state(false);

  type ItemProgress = {
    key: string;
    bytesUploaded: number;
    size: number;
    percent: number;
    status: 'active' | 'done' | 'failed';
    phase: 'copy' | 'upload';
    // Number of source files this item represents — 1 for an
    // individual upload, len(group.Files) for a zip group. The engine
    // emits this on `upload_start` / `upload_complete` for zips so the
    // dashboard counters reflect file count, not group count. (#count-zips)
    files: number;
    error?: string;
    updatedAt: number;
  };
  let itemProgress = $state<Record<string, ItemProgress>>({});

  type DBSync = {
    reason: 'stop' | 'cancel' | 'complete' | string;
    bytes: number;
    total: number;
    percent: number;
    status: 'active' | 'done' | 'failed';
    error?: string;
  };
  let dbSync = $state<DBSync | null>(null);
  let dbSyncHideTimer: number | undefined;

  // Cap retained terminal (done/failed) items so a 50k-file run doesn't
  // keep every row in $state for the tab lifetime — itemList re-sorts the
  // whole map on every event. Active items are never evicted. (#200)
  const TERMINAL_ITEM_CAP = 200;

  function upsertItem(key: string, patch: Partial<ItemProgress>) {
    const prev = itemProgress[key];
    itemProgress[key] = {
      key,
      bytesUploaded: prev?.bytesUploaded ?? 0,
      size: prev?.size ?? 0,
      percent: prev?.percent ?? 0,
      status: prev?.status ?? 'active',
      phase: prev?.phase ?? 'copy',
      files: prev?.files ?? 1,
      error: prev?.error,
      updatedAt: Date.now(),
      ...patch,
    };
    const next = itemProgress[key];
    if (next.status === 'done' || next.status === 'failed') {
      const terminal: ItemProgress[] = [];
      for (const it of Object.values(itemProgress)) {
        if (it.status === 'done' || it.status === 'failed') terminal.push(it);
      }
      if (terminal.length > TERMINAL_ITEM_CAP) {
        terminal.sort((a, b) => a.updatedAt - b.updatedAt);
        const drop = terminal.length - TERMINAL_ITEM_CAP;
        for (let i = 0; i < drop; i++) delete itemProgress[terminal[i].key];
      }
    }
  }

  // Derive counters from `itemProgress` so they're idempotent under SSE
  // replay/reconnect. `total` still comes from the `upload_plan` event. (#199)
  let uploads = $derived.by(() => {
    let completed = 0, failed = 0, started = 0;
    for (const it of Object.values(itemProgress)) {
      const n = it.files || 1;
      if (it.status === 'done') { completed += n; started += n; }
      else if (it.status === 'failed') { failed += n; started += n; }
      else if (it.phase === 'upload') started += n;
    }
    // Persisted count survives page reloads; live events catch up within
    // a poll interval. Take the max so neither source can drag the bar
    // backward as the other one races ahead.
    completed = Math.max(completed, seededFilesUploaded);
    started = Math.max(started, completed);
    return { completed, failed, started, total: uploadsTotal };
  });

  let itemList = $derived(
    Object.values(itemProgress).sort((a, b) => {
      // Active before terminal.
      if (a.status === 'active' && b.status !== 'active') return -1;
      if (b.status === 'active' && a.status !== 'active') return 1;
      // Among active items, pin uploads above copies — the upload phase
      // is the slow, network-bound one users want to watch, and copy
      // events fire constantly enough to otherwise bury it. (#136)
      if (a.status === 'active' && b.status === 'active' && a.phase !== b.phase) {
        return a.phase === 'upload' ? -1 : 1;
      }
      return b.updatedAt - a.updatedAt;
    }),
  );

  let es: { close: () => void } | null = null;
  let pollTimer: number | undefined;
  let pollDestroyed = false;

  // SSE drives most updates; polling is a backstop for missed events and
  // to surface server-side state changes the bus doesn't emit. Use 1s
  // while a run is active so the headline numbers (files scanned /
  // uploaded / bytes) feel live even if a few SSE frames were dropped;
  // 30s when idle — an idle dashboard otherwise burns ~1200 status
  // calls/hour for no value. (#70)
  function scheduleNextPoll() {
    if (pollDestroyed) return;
    const delay = status?.current ? 1000 : 30000;
    pollTimer = window.setTimeout(async () => {
      await refresh();
      scheduleNextPoll();
    }, delay);
  }

  async function refresh() {
    try {
      status = await api.status();
      stats = await api.fileStats();
      // Seed scanSeen from the run row so a missed scan_progress SSE
      // frame (or a reconnect mid-run) doesn't strand the counter at
      // 0. The engine writes files_scanned on every scan flush so this
      // tracks within ~3 s of the live count. We only bump scanSeen
      // upward — the SSE path may have a fresher value already.
      const live = status?.current?.files_scanned ?? 0;
      if (live > scanSeen) scanSeen = live;
      // Seed completed-uploads from the run row so a reload mid-run
      // recovers the progress count instead of starting at 0.
      const liveUp = status?.current?.files_uploaded ?? 0;
      if (liveUp > seededFilesUploaded) seededFilesUploaded = liveUp;
    } catch (e) {
      toast.error(String(e));
    }
  }

  onMount(() => {
    refresh().then(() => scheduleNextPoll());
    es = subscribeEvents((type, data) => {
      const d = data as any;
      const payload = d?.data ?? {};
      if (type === 'scan_start') {
        // Flip the indicator on. Don't reset scanSeen here — run_start
        // already reset it for a fresh run (live: undefined → 0; replay:
        // seeded from files_scanned), and resetting here just flashes
        // 0 even when the SSE event arrived microseconds after a
        // refresh() that already populated it from the run row.
        scanNew = 0;
        scanChanged = 0;
        scanActive = true;
      }
      if (type === 'scan_progress') {
        scanSeen = payload.seen ?? scanSeen;
        scanNew = payload.new ?? scanNew;
        scanChanged = payload.changed ?? scanChanged;
        scanActive = true;
      }
      if (type === 'scan_complete') {
        scanSeen = payload.seen ?? 0;
        scanActive = false;
      }
      if (type === 'upload_plan') uploadsTotal = payload.total_files ?? 0;
      if (type === 'copy_progress' && payload.key) {
        upsertItem(payload.key, {
          bytesUploaded: payload.bytes_copied ?? 0,
          size: payload.size ?? 0,
          percent: payload.percent ?? 0,
          status: 'active',
          phase: 'copy',
        });
      }
      if (type === 'upload_start') {
        if (payload.key) {
          // Reset bar to 0 on phase switch — the upload size differs from
          // the copy size for zips (compression) so we can't carry over.
          upsertItem(payload.key, {
            bytesUploaded: 0,
            size: payload.size ?? 0,
            percent: 0,
            status: 'active',
            phase: 'upload',
            files: payload.files ?? 1,
            error: undefined,
          });
        }
      }
      if (type === 'upload_progress' && payload.key) {
        upsertItem(payload.key, {
          bytesUploaded: payload.bytes_uploaded ?? 0,
          size: payload.size ?? 0,
          percent: payload.percent ?? 0,
          status: 'active',
          phase: 'upload',
        });
      }
      if (type === 'upload_complete') {
        if (payload.key) {
          upsertItem(payload.key, {
            bytesUploaded: payload.size ?? itemProgress[payload.key]?.size ?? 0,
            size: payload.size ?? itemProgress[payload.key]?.size ?? 0,
            percent: 100,
            status: 'done',
            phase: 'upload',
            files: payload.files ?? itemProgress[payload.key]?.files ?? 1,
          });
        }
      }
      if (type === 'upload_failed' && payload.key) {
        upsertItem(payload.key, {
          status: 'failed',
          error: payload.error ?? '',
        });
      }
      if (type === 'run_start') {
        itemProgress = {};
        uploadsTotal = 0;
        // run_start also fires on SSE reconnect with files_uploaded
        // populated from the DB, so a mid-run reconnect keeps the
        // count; a brand-new run sends 0 and resets the bar.
        seededFilesUploaded = payload.files_uploaded ?? 0;
        // Don't toggle scanActive here — run_start fires both for live
        // run starts AND on every SSE reconnect (replay), and a
        // post-scan reconnect would otherwise stick the "Scanning…"
        // indicator on with no follow-up scan_complete to clear it.
        // The dedicated scan_start event drives that toggle now.
        // Seed scanSeen from the run row (replayed run_start carries
        // files_scanned from the DB so a mid-run reconnect doesn't show
        // 0 until the next scan_progress tick).
        scanSeen = payload.files_scanned ?? 0;
        scanNew = 0;
        scanChanged = 0;
        logLines = [];
        // Clear any leftover DB-sync card from the previous run so it
        // doesn't linger across a new triggerRun.
        if (dbSyncHideTimer) { clearTimeout(dbSyncHideTimer); dbSyncHideTimer = undefined; }
        dbSync = null;
      }
      if (type === 'db_sync_start') {
        if (dbSyncHideTimer) { clearTimeout(dbSyncHideTimer); dbSyncHideTimer = undefined; }
        dbSync = {
          reason: payload.reason ?? 'complete',
          bytes: 0,
          total: payload.size ?? 0,
          percent: 0,
          status: 'active',
        };
      }
      if (type === 'db_sync_progress' && dbSync) {
        dbSync = {
          ...dbSync,
          reason: payload.reason ?? dbSync.reason,
          bytes: payload.bytes ?? dbSync.bytes,
          total: payload.total ?? dbSync.total,
          percent: payload.percent ?? dbSync.percent,
          status: 'active',
        };
      }
      if (type === 'db_sync_complete') {
        dbSync = {
          reason: payload.reason ?? dbSync?.reason ?? 'complete',
          bytes: payload.size ?? dbSync?.total ?? 0,
          total: payload.size ?? dbSync?.total ?? 0,
          percent: 100,
          status: 'done',
        };
        // Auto-hide the success badge so an idle dashboard doesn't keep
        // stale "Index DB → S3 done" state forever.
        if (dbSyncHideTimer) clearTimeout(dbSyncHideTimer);
        dbSyncHideTimer = window.setTimeout(() => { dbSync = null; }, 8000);
      }
      if (type === 'db_sync_failed') {
        // Cancel any pending auto-hide from a recent `db_sync_complete`
        // so it doesn't wipe the failure banner 8s later. (#205)
        if (dbSyncHideTimer) { clearTimeout(dbSyncHideTimer); dbSyncHideTimer = undefined; }
        dbSync = {
          reason: payload.reason ?? dbSync?.reason ?? 'complete',
          bytes: dbSync?.bytes ?? 0,
          total: dbSync?.total ?? 0,
          percent: dbSync?.percent ?? 0,
          status: 'failed',
          error: payload.error ?? '',
        };
      }
      if (type === 'run_complete') scanActive = false;
      // Skip high-frequency progress events: they fire continuously per
      // upload thread and would otherwise allocate + re-render the log
      // hundreds of times per second, evicting the run_log lines users
      // actually want to read. The progress bar/items list already
      // surfaces that data. (#263)
      if (
        type === 'run_log' ||
        type === 'run_start' ||
        type === 'run_complete' ||
        type === 'run_failed' ||
        type === 'run_cancelled' ||
        type === 'run_stopped'
      ) {
        const line = type === 'run_log'
          ? `[${payload.level ?? 'log'}] ${payload.message ?? ''}`
          : `[${type}] ${JSON.stringify(d.data ?? d)}`;
        logLines = [...logLines.slice(-49), line];
      }
      if (type === 'run_complete' || type === 'run_start') {
        // Reset the poll cadence so an idle 30s timer doesn't keep ticking
        // after a run starts (and vice versa after it ends). (#70)
        if (pollTimer) clearTimeout(pollTimer);
        refresh().then(() => scheduleNextPoll());
      }
    });
  });

  onDestroy(() => {
    pollDestroyed = true;
    es?.close();
    if (pollTimer) clearTimeout(pollTimer);
    if (dbSyncHideTimer) clearTimeout(dbSyncHideTimer);
  });

  function dbSyncLabel(reason: string): string {
    if (reason === 'stop') return 'after stop';
    if (reason === 'cancel') return 'after cancel';
    return 'after run';
  }

  async function triggerRun(mode: 'full' | 'scan' | 'upload' = 'full') {
    triggering = true;
    try {
      await api.triggerRun({ mode });
      // Don't reset state here — the `run_start` SSE handler is the source
      // of truth and clears uploads/scan/itemProgress/logLines as soon as
      // the engine actually starts. Resetting after the await races with
      // events that may already have arrived. (#198)
    } catch (e) {
      toast.error(String(e));
    } finally {
      triggering = false;
    }
  }

  async function cancel() {
    if (!status?.current) return;
    try {
      await api.cancelRun(status.current.id);
    } catch (e) {
      toast.error(String(e));
    }
  }

  let stopping = $state(false);
  async function stop() {
    if (!status?.current || stopping) return;
    stopping = true;
    try {
      await api.stopRun(status.current.id);
      // Reflect the new state immediately instead of waiting for the
      // next 3s status poll, so the button label flips on click.
      if (status) status.stop_requested = true;
    } catch (e) {
      toast.error(String(e));
    } finally {
      stopping = false;
    }
  }

  async function continueRun() {
    if (!status?.current || stopping) return;
    stopping = true;
    try {
      await api.continueRun(status.current.id);
      if (status) status.stop_requested = false;
    } catch (e) {
      toast.error(String(e));
    } finally {
      stopping = false;
    }
  }

  let syncing = $state(false);
  let syncInfo = $state('');
  let fullSyncResult = $state<{
    local_missing_count: number;
    local_missing_from_cloud?: string[];
    cloud_missing_count: number;
    cloud_missing_from_local?: string[];
    cloud_file_count: number;
    local_file_count: number;
    zip_indexes_consumed: number;
  } | null>(null);

  let syncingRestore = $state(false);
  let scanningRestore = $state(false);
  let restoreSyncInfo = $state('');
  async function syncRestore() {
    syncingRestore = true;
    restoreSyncInfo = '';
    try {
      const r = await api.restoreSyncStatus();
      restoreSyncInfo = r.processed === 0
        ? 'No new restore events.'
        : `Processed ${r.processed} restore event(s).`;
      await refresh();
    } catch (e) {
      toast.error(String(e));
    } finally {
      syncingRestore = false;
    }
  }
  async function fullScanRestore() {
    scanningRestore = true;
    restoreSyncInfo = '';
    try {
      const r = await api.restoreScanFull();
      restoreSyncInfo = `Full scan: ${r.scanned} HEADed, ${r.updated} updated, ${r.errors} error(s).`;
      await refresh();
    } catch (e) {
      toast.error(String(e));
    } finally {
      scanningRestore = false;
    }
  }

  // --- fix-action state ---
  let backingUp = $state(false);
  let backupStarted = $state(false);

  let showRestoreForm = $state(false);
  let restoreDays = $state(7);
  let restoring = $state(false);
  let restoreResult = $state<{
    keys_requested: number;
    keys_already_in_progress: number;
    keys_already_available: number;
    files_affected: number;
    bytes_affected: number;
    unknown_paths?: string[];
    errors?: string[];
  } | null>(null);

  let showDeleteConfirm = $state(false);
  let deleting = $state(false);
  let deleteResult = $state<{ deleted_standalone: number; deleted_zips: number; skipped_partial_zip: number; errors?: string[] } | null>(null);

  function resetFixState() {
    backupStarted = false;
    showRestoreForm = false; restoreResult = null;
    showDeleteConfirm = false; deleteResult = null;
  }

  async function backUpMissing() {
    if (!fullSyncResult?.local_missing_from_cloud?.length) return;
    backingUp = true; backupStarted = false;
    try {
      // Force-reset these paths to pending regardless of their current DB
      // status — files may be marked uploaded/zipped even though they're
      // absent from the cloud index (e.g. zip exists but has no .index.txt).
      await api.retryByPaths(fullSyncResult.local_missing_from_cloud);
      await api.triggerRun({ mode: 'upload' });
      backupStarted = true;
      await refresh();
    } catch (e) {
      // Toast over inline `err small` per CLAUDE.md feedback convention. (#226)
      toast.error(String(e));
    } finally {
      backingUp = false;
    }
  }

  async function restoreMissing() {
    if (!fullSyncResult?.cloud_missing_from_local?.length) return;
    if (!Number.isInteger(restoreDays) || restoreDays < 1 || restoreDays > 30) {
      toast.error('days must be an integer between 1 and 30');
      return;
    }
    restoring = true; restoreResult = null;
    try {
      restoreResult = await api.restoreTrigger(fullSyncResult.cloud_missing_from_local, restoreDays);
    } catch (e) {
      toast.error(String(e)); // (#226)
    } finally {
      restoring = false;
    }
  }

  async function deleteCloudMissing() {
    if (!fullSyncResult?.cloud_missing_from_local?.length) return;
    deleting = true; deleteResult = null;
    try {
      deleteResult = await api.deleteCloudPaths(fullSyncResult.cloud_missing_from_local);
    } catch (e) {
      toast.error(String(e)); // (#226)
    } finally {
      deleting = false;
      showDeleteConfirm = false;
    }
  }

  async function syncWithS3() {
    syncing = true;
    syncInfo = '';
    fullSyncResult = null;
    try {
      const r = await api.sync();
      const total = r.missing_zips + r.missing_individual;
      if (total === 0) {
        const checked = r.zip_names_in_db + r.individual_keys_in_db;
        syncInfo = `Index in sync — ${checked} object(s) checked.`;
      } else {
        syncInfo = `Sync complete: ${r.missing_zips} zip(s) + ${r.missing_individual} individual file(s) missing from S3, ${r.files_reset} file(s) reset to pending.`;
      }
      await refresh();
    } catch (e) {
      toast.error(String(e));
    } finally {
      syncing = false;
    }
  }

  async function fullSync() {
    syncing = true;
    syncInfo = '';
    fullSyncResult = null;
    resetFixState();
    try {
      const r = await api.syncFull();
      fullSyncResult = r;
      const existence = r.missing_zips + r.missing_individual;
      const parts = [
        `${r.zip_indexes_consumed} zip index(es) consumed`,
        `${r.cloud_file_count} cloud file(s) / ${r.local_file_count} local file(s)`,
      ];
      if (existence > 0) parts.push(`${existence} S3 object(s) missing`);
      syncInfo = `Full sync: ${parts.join(' · ')}.`;
      await refresh();
    } catch (e) {
      toast.error(String(e));
    } finally {
      syncing = false;
    }
  }
</script>

<h1>Dashboard</h1>

<div class="grid">
  <div class="card">
    <div class="label">Current run</div>
    {#if status?.current}
      <div class="big">
        run #{status.current.id}
        <StatusBadge status={status.stop_requested ? 'stopping' : status.current.status} />
      </div>
      <div class="muted">started {relativeTime(status.current.started_at)}</div>
      <div class="run-actions" style="margin-top: 0.5rem">
        {#if status.stop_requested}
          <button class="primary" onclick={continueRun} type="button" disabled={stopping} title="Cancel the pending stop and keep uploading">
            {stopping ? 'Continuing…' : 'Continue'}
          </button>
        {:else}
          <button class="primary" onclick={stop} type="button" disabled={stopping} title="Finish the in-flight upload, then stop">
            {stopping ? 'Stopping…' : 'Stop'}
          </button>
        {/if}
        <button class="danger" onclick={cancel} type="button" title="Kill the in-flight upload immediately">
          Force cancel
        </button>
      </div>
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
      Quick: existence check by S3 key — missing objects reset to pending.<br />
      Full: also downloads every zip index and diffs local vs cloud file sets.
    </div>
    {#if syncInfo}<div class="sync-info">{syncInfo}</div>{/if}
    <div class="run-actions">
      <button onclick={syncWithS3} type="button" disabled={syncing || !!status?.current}>
        {syncing ? 'Syncing…' : 'Sync with S3'}
      </button>
      <button onclick={fullSync} type="button" disabled={syncing || !!status?.current}
              title="Heavier: downloads every .index.txt sidecar and compares file contents">
        Full sync
      </button>
    </div>
  </div>

  <div class="card">
    <div class="label">Glacier restores</div>
    {#if stats}
      {@const inProg = stats.by_restore_status?.['in_progress'] ?? 0}
      {@const restored = stats.by_restore_status?.['restored'] ?? 0}
      {#if inProg === 0 && restored === 0}
        <div class="big muted">None</div>
        <div class="muted small">No files have a Glacier restore in flight.</div>
      {:else}
        <div class="big">{(inProg + restored).toLocaleString()} file(s)</div>
        <div class="pills">
          {#if inProg > 0}<span class="pill"><StatusBadge status="in_progress" /> {inProg.toLocaleString()}</span>{/if}
          {#if restored > 0}<span class="pill"><StatusBadge status="restored" /> {restored.toLocaleString()}</span>{/if}
        </div>
        {#if stats.restore_soonest_expires_at}
          <div class="muted small" style="margin-top: 0.4rem"
               title={stats.restore_soonest_expires_at}>
            soonest expires {expiresIn(stats.restore_soonest_expires_at)}
            ({formatDate(stats.restore_soonest_expires_at)})
          </div>
        {/if}
      {/if}
      {#if restoreSyncInfo}<div class="sync-info" style="margin-top: 0.5rem">{restoreSyncInfo}</div>{/if}
      <div class="run-actions">
        <button onclick={syncRestore} type="button" disabled={syncingRestore || scanningRestore}
                title="Drain SQS now instead of waiting on the background poll">
          {syncingRestore ? 'Syncing…' : 'Sync now'}
        </button>
        <button onclick={fullScanRestore} type="button" disabled={syncingRestore || scanningRestore}
                title="HEAD every uploaded file to authoritatively reconcile restore status">
          {scanningRestore ? 'Scanning…' : 'Full scan'}
        </button>
      </div>
    {/if}
  </div>
</div>

{#if fullSyncResult}
  <div class="card">
    <div class="label">Full sync result</div>
    <div class="sync-grid">

      <!-- Local files not in cloud -->
      <details open={fullSyncResult.local_missing_count > 0}>
        <summary>
          <strong>{fullSyncResult.local_missing_count}</strong> local file(s) missing from cloud
          {#if fullSyncResult.local_missing_count > (fullSyncResult.local_missing_from_cloud?.length ?? 0)}
            (showing first {fullSyncResult.local_missing_from_cloud?.length})
          {/if}
        </summary>
        {#if fullSyncResult.local_missing_from_cloud?.length}
          <ul class="mono small">
            {#each fullSyncResult.local_missing_from_cloud as p}<li>{p}</li>{/each}
          </ul>
        {/if}
        {#if fullSyncResult.local_missing_count > 0}
          <div class="fix-row">
            {#if backupStarted}
              <span class="ok-text">Upload run started — watch Live progress below.</span>
            {:else}
              <button
                class="primary fix-btn"
                onclick={backUpMissing}
                type="button"
                disabled={backingUp || !!status?.current}
                title={status?.current ? 'A run is already in progress' : ''}
              >
                {backingUp ? 'Starting…' : `Back up ${fullSyncResult.local_missing_count} missing file(s)`}
              </button>
              {#if status?.current}<span class="muted small">run in progress</span>{/if}
            {/if}
          </div>
        {/if}
      </details>

      <!-- Cloud files not in local -->
      <details open={fullSyncResult.cloud_missing_count > 0}>
        <summary>
          <strong>{fullSyncResult.cloud_missing_count}</strong> cloud file(s) missing locally
          {#if fullSyncResult.cloud_missing_count > (fullSyncResult.cloud_missing_from_local?.length ?? 0)}
            (showing first {fullSyncResult.cloud_missing_from_local?.length})
          {/if}
        </summary>
        {#if fullSyncResult.cloud_missing_from_local?.length}
          <ul class="mono small">
            {#each fullSyncResult.cloud_missing_from_local as p}<li>{p}</li>{/each}
          </ul>
        {/if}
        {#if fullSyncResult.cloud_missing_count > 0}
          <div class="fix-row">
            {#if restoreResult}
              <span class="ok-text">
                Retrieval requested · {restoreResult.keys_requested} key(s) · {restoreResult.files_affected} file(s) · {bytes(restoreResult.bytes_affected)}
                {#if restoreResult.keys_already_in_progress > 0} · {restoreResult.keys_already_in_progress} already thawing{/if}
                {#if restoreResult.keys_already_available > 0} · {restoreResult.keys_already_available} already available{/if}
                {#if restoreResult.errors?.length} · {restoreResult.errors.length} error(s){/if}
              </span>
            {:else if deleteResult}
              <span class="ok-text">
                Deleted {deleteResult.deleted_standalone} standalone + {deleteResult.deleted_zips} zip(s)
                {#if deleteResult.skipped_partial_zip > 0} · {deleteResult.skipped_partial_zip} partial zip(s) skipped{/if}
                {#if deleteResult.errors?.length} · {deleteResult.errors.length} error(s){/if}
              </span>
            {:else if showRestoreForm}
              <div class="fix-form">
                <label class="muted small" style="display: flex; align-items: center; gap: 0.4rem">
                  Days to keep restored
                  <input
                    type="number"
                    bind:value={restoreDays}
                    min="1"
                    max="30"
                    step="1"
                    class="path-input"
                    style="width: 5rem"
                  />
                </label>
                <button
                  class="primary fix-btn"
                  onclick={restoreMissing}
                  type="button"
                  disabled={restoring}
                >
                  {restoring ? 'Requesting…' : `Request retrieval for ${fullSyncResult.cloud_missing_from_local?.length ?? fullSyncResult.cloud_missing_count} file(s)`}
                </button>
                <button onclick={() => showRestoreForm = false} type="button">Cancel</button>
              </div>
            {:else if showDeleteConfirm}
              <div class="fix-form">
                <span class="warn-text small">
                  Delete {fullSyncResult.cloud_missing_from_local?.length ?? fullSyncResult.cloud_missing_count} cloud object(s)?
                  Standalone files are removed immediately; zips are only deleted when all their contents are targeted.
                </span>
                <button
                  class="danger fix-btn"
                  onclick={deleteCloudMissing}
                  type="button"
                  disabled={deleting}
                >
                  {deleting ? 'Deleting…' : 'Confirm delete'}
                </button>
                <button onclick={() => showDeleteConfirm = false} type="button">Cancel</button>
              </div>
            {:else}
              <button class="fix-btn" onclick={() => showRestoreForm = true} type="button">
                Restore to directory…
              </button>
              <button class="fix-btn danger" onclick={() => showDeleteConfirm = true} type="button">
                Delete from cloud…
              </button>
            {/if}
          </div>
        {/if}
      </details>

    </div>
  </div>
{/if}

{#if dbSync}
  <div class="card db-sync-card db-sync-{dbSync.status}">
    <div class="label">Index DB → S3 ({dbSyncLabel(dbSync.reason)})</div>
    <div class="db-sync-head">
      <span class="db-sync-status">
        {#if dbSync.status === 'active'}uploading…{:else if dbSync.status === 'done'}done{:else}failed{/if}
      </span>
      <span class="db-sync-pct mono">
        {#if dbSync.status === 'failed'}—{:else}{dbSync.percent.toFixed(1)}%{/if}
      </span>
    </div>
    <div class="bar"><div class="fill" style="width: {Math.min(100, dbSync.percent)}%"></div></div>
    <div class="db-sync-foot mono">
      {bytes(dbSync.bytes)} / {bytes(dbSync.total)}
      {#if dbSync.error}<span class="err"> · {dbSync.error}</span>{/if}
    </div>
  </div>
{/if}

{#if status?.current || logLines.length}
  <div class="card">
    <div class="label">Live progress</div>
    {#if scanActive}
      <div class="scan-row">
        <div class="bar bar-indeterminate"><div class="fill"></div></div>
        <div class="scan-foot mono">
          Scanning… {scanSeen.toLocaleString()} seen{#if scanNew > 0} · {scanNew.toLocaleString()} new{/if}{#if scanChanged > 0} · {scanChanged.toLocaleString()} changed{/if}
        </div>
      </div>
    {/if}
    <div class="live-stats">
      <span>scanned: <strong>{scanSeen.toLocaleString()}</strong></span>
      <span>started: <strong>{uploads.started}</strong></span>
      <span>completed: <strong>{uploads.completed}</strong></span>
      <span>failed: <strong>{uploads.failed}</strong></span>
    </div>
    {#if uploads.total > 0 || uploads.started > 0}
      <ProgressBar value={uploads.completed} max={uploads.total || uploads.started} label="Uploads" />
    {/if}
    {#if itemList.length > 0}
      <div class="items">
        {#each itemList as item (item.key)}
          <div class="item item-{item.status}">
            <div class="item-head">
              <span class="item-key mono" title={item.key}>{item.key}</span>
              {#if item.status === 'active'}
                <span class="item-phase">{item.phase === 'copy' ? 'copying' : 'uploading'}</span>
              {/if}
              <span class="item-pct">
                {#if item.status === 'failed'}
                  failed
                {:else if item.status === 'done'}
                  100%
                {:else}
                  {item.percent.toFixed(1)}%
                {/if}
              </span>
            </div>
            <div class="bar"><div class="fill" style="width: {Math.min(100, item.percent)}%"></div></div>
            <div class="item-foot mono">
              {bytes(item.bytesUploaded)} / {bytes(item.size)}
              {#if item.error}<span class="err"> · {item.error}</span>{/if}
            </div>
          </div>
        {/each}
      </div>
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
  .sync-grid { display: flex; flex-direction: column; gap: 0.75rem; }
  .small { font-size: 0.85rem; }
  details { border: 1px solid var(--border); border-radius: 4px; padding: 0.5rem 0.75rem; }
  details[open] { background: var(--bg); }
  details summary { cursor: pointer; }
  details ul { margin: 0.5rem 0 0; padding-left: 1.25rem; max-height: 220px; overflow: auto; }
  .fix-row { margin-top: 0.75rem; display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; }
  .fix-form { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; width: 100%; }
  .fix-btn { font-size: 0.85rem; }
  .danger { border-color: var(--err); color: var(--err); }
  .danger:hover:not(:disabled) { background: var(--err); color: #fff; }
  .path-input { flex: 1 1 240px; min-width: 0; padding: 0.3rem 0.6rem; font-size: 0.85rem; font-family: var(--mono, monospace); border: 1px solid var(--border); background: var(--bg); color: var(--fg); border-radius: 4px; }
  .ok-text { color: var(--ok, #22c55e); font-size: 0.85rem; }
  .warn-text { color: var(--warn, #f59e0b); }
  .items {
    display: grid;
    gap: 0.5rem;
    margin: 0.5rem 0;
    max-height: 320px;
    overflow: auto;
    padding-right: 0.25rem;
  }
  .item {
    display: grid;
    gap: 0.25rem;
    padding: 0.4rem 0.6rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--bg);
  }
  .item-done { opacity: 0.7; }
  .item-failed { border-color: var(--err); }
  .item-head {
    display: flex;
    justify-content: space-between;
    gap: 0.5rem;
    align-items: baseline;
    font-size: 0.85rem;
  }
  .item-key {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    flex: 1 1 auto;
    min-width: 0;
  }
  .item-pct { font-size: 0.8rem; color: var(--muted); flex: 0 0 auto; }
  .item-phase { font-size: 0.75rem; color: var(--muted); flex: 0 0 auto; font-style: italic; }
  .bar { height: 6px; background: var(--bg); border: 1px solid var(--border); border-radius: 3px; overflow: hidden; }
  .fill { height: 100%; background: var(--accent); transition: width 0.2s ease; }
  .scan-row { margin-bottom: 0.5rem; }
  .scan-foot { font-size: 0.75rem; color: var(--muted); margin-top: 0.25rem; }
  .bar-indeterminate .fill {
    width: 35%;
    transition: none;
    animation: scan-slide 1.5s linear infinite;
  }
  @keyframes scan-slide {
    0%   { transform: translateX(-100%); }
    100% { transform: translateX(285%); }
  }
  .item-foot { font-size: 0.75rem; color: var(--muted); }
  .db-sync-head { display: flex; justify-content: space-between; align-items: baseline; font-size: 0.85rem; margin: 0.25rem 0; }
  .db-sync-status { color: var(--muted); }
  .db-sync-pct { color: var(--muted); }
  .db-sync-foot { font-size: 0.75rem; color: var(--muted); margin-top: 0.25rem; }
  .db-sync-done { opacity: 0.85; }
  .db-sync-failed { border-color: var(--err); }
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
    /* Long JSON event lines were pushing the page wider than the
       1200px shell; wrap them inside the log pane instead. */
    min-width: 0;
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
