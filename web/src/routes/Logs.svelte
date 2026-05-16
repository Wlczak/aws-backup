<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { api, ApiError, type Run, type RunDetail } from '../lib/api';
  import { formatDate, bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import StatusBadge from '../components/StatusBadge.svelte';

  let runs = $state<Run[]>([]);
  let selectedID = $state<number | null>(null);
  let detail = $state<RunDetail | null>(null);

  // Mirrors the AbortController pattern in Files.svelte (#204): cancel
  // the previous in-flight detail load on rapid row clicks AND on
  // unmount so a late response can't write to torn-down state. (#230)
  let detailCtrl: AbortController | null = null;
  let listCtrl: AbortController | null = null;
  let aborted = false;
  let clearing = $state(false);

  async function loadRuns() {
    listCtrl?.abort();
    const ctl = new AbortController();
    listCtrl = ctl;
    try {
      const page = await api.runs(1, 50, ctl.signal);
      if (aborted || ctl.signal.aborted) return;
      runs = page.runs;
    } catch (e) {
      if (e instanceof ApiError && e.kind === 'abort') return;
      if (!aborted) toast.error(String(e));
    }
  }

  async function selectRun(id: number) {
    detailCtrl?.abort();
    const ctl = new AbortController();
    detailCtrl = ctl;
    selectedID = id;
    detail = null;
    try {
      const d = await api.run(id, ctl.signal);
      if (aborted || ctl.signal.aborted) return;
      detail = d;
    } catch (e) {
      if (e instanceof ApiError && e.kind === 'abort') return;
      if (!aborted) toast.error(String(e));
    }
  }

  async function clearAllLogs() {
    if (!confirm('Delete every log line in the database? This keeps the run rows.')) return;
    clearing = true;
    try {
      const res = await api.deleteRunLogs();
      toast.success(`Cleared ${res.affected.toLocaleString()} log line(s).`);
      await loadRuns();
      if (selectedID !== null) {
        await selectRun(selectedID);
      } else {
        detail = null;
      }
    } catch (e) {
      if (e instanceof ApiError && e.kind === 'abort') return;
      if (!aborted) toast.error(String(e));
    } finally {
      clearing = false;
    }
  }

  onMount(loadRuns);
  onDestroy(() => {
    aborted = true;
    detailCtrl?.abort();
    listCtrl?.abort();
  });
</script>

<h1>Run logs</h1>

<div class="toolbar">
  <button class="danger" onclick={clearAllLogs} disabled={clearing} type="button">
    {clearing ? 'Clearing…' : 'Clear all logs'}
  </button>
</div>

<div class="grid">
  <div class="card nopad runs">
    <table>
      <thead>
        <tr>
          <th>#</th>
          <th>Started</th>
          <th>Status</th>
          <th>Uploaded</th>
          <th>Bytes</th>
        </tr>
      </thead>
      <tbody>
        {#each runs as r (r.id)}
          <tr
            class:selected={selectedID === r.id}
            role="button"
            tabindex="0"
            onclick={() => selectRun(r.id)}
            onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); selectRun(r.id); } }}
          >
            <td>{r.id}</td>
            <td>{formatDate(r.started_at)}</td>
            <td><StatusBadge status={r.status} /></td>
            <td>{r.files_uploaded.toLocaleString()}</td>
            <td>{bytes(r.bytes_uploaded)}</td>
          </tr>
        {/each}
        {#if runs.length === 0}
          <tr><td colspan="5" class="muted" style="text-align: center; padding: 1.5rem">No runs yet</td></tr>
        {/if}
      </tbody>
    </table>
  </div>

  <div class="card">
    {#if detail}
      <div class="row"><span class="label">Run</span>
        <span>#{detail.run.id} <StatusBadge status={detail.run.status} /></span>
      </div>
      <div class="row"><span class="label">Started</span><span>{formatDate(detail.run.started_at)}</span></div>
      <div class="row"><span class="label">Finished</span><span>{detail.run.finished_at ? formatDate(detail.run.finished_at) : '—'}</span></div>
      <div class="row"><span class="label">Scanned</span><span>{detail.run.files_scanned.toLocaleString()}</span></div>
      <div class="row"><span class="label">Uploaded</span><span>{detail.run.files_uploaded.toLocaleString()} ({bytes(detail.run.bytes_uploaded)})</span></div>
      {#if detail.run.error_message}
        <div class="row"><span class="label">Error</span><span class="err mono">{detail.run.error_message}</span></div>
      {/if}
      <h3 style="margin: 1rem 0 0.5rem">Log</h3>
      <pre class="log">{detail.logs.map((l) => `${formatDate(l.timestamp)} [${l.level}] ${l.message}`).join('\n') || '(no log lines)'}</pre>
    {:else}
      <p class="muted">Select a run on the left to view its log.</p>
    {/if}
  </div>
</div>

<style>
  .grid { display: grid; grid-template-columns: minmax(360px, 480px) 1fr; gap: 1rem; align-items: start; }
  .toolbar { display: flex; justify-content: flex-end; }
  .danger {
    border-color: var(--err);
    color: var(--err);
  }
  .nopad { padding: 0; overflow: hidden; }
  tbody tr { cursor: pointer; }
  tbody tr:hover { background: rgba(255, 255, 255, 0.03); }
  tbody tr.selected { background: rgba(76, 139, 245, 0.08); }
  .row { display: grid; grid-template-columns: 110px 1fr; gap: 0.5rem; padding: 0.25rem 0; }
  .label { color: var(--muted); font-size: 0.85rem; }
  .err { color: var(--err); }
  .log {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.75rem;
    margin: 0;
    max-height: 480px;
    overflow: auto;
    font-size: 0.82rem;
    white-space: pre-wrap;
  }
  @media (max-width: 800px) {
    .grid { grid-template-columns: 1fr; }
  }
</style>
