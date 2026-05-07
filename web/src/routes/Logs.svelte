<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Run, type RunDetail } from '../lib/api';
  import { formatDate, bytes } from '../lib/format';
  import { toast } from '../lib/toast';
  import StatusBadge from '../components/StatusBadge.svelte';

  let runs = $state<Run[]>([]);
  let selectedID = $state<number | null>(null);
  let detail = $state<RunDetail | null>(null);

  async function loadRuns() {
    try {
      const page = await api.runs(1, 50);
      runs = page.runs;
    } catch (e) {
      toast.error(String(e));
    }
  }

  async function selectRun(id: number) {
    selectedID = id;
    detail = null;
    try {
      detail = await api.run(id);
    } catch (e) {
      toast.error(String(e));
    }
  }

  onMount(loadRuns);
</script>

<h1>Run logs</h1>

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
          <tr class:selected={selectedID === r.id} onclick={() => selectRun(r.id)}>
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
