<script lang="ts">
  import { api, type RestoreEstimate } from '../lib/api';
  import { bytes } from '../lib/format';

  let raw = $state('');
  let estimate = $state<RestoreEstimate | null>(null);
  let err = $state('');
  let info = $state('');
  let loading = $state(false);
  let confirmTrigger = $state(false);

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

  async function doTrigger() {
    const p = paths();
    if (!confirmTrigger) {
      confirmTrigger = true;
      return;
    }
    loading = true;
    err = '';
    info = '';
    try {
      const res = await api.restoreTrigger(p);
      info = res?.error ?? 'restore requested';
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
  <div class="label">Paths (one per line)</div>
  <p class="muted">Enter top-level directories or full file paths. Prefix match is applied — e.g. <code class="mono">photos</code> selects every file under <code class="mono">photos/</code>.</p>
  <textarea bind:value={raw} placeholder={"photos\ndocs/2024\nfamily-archive.zip"}></textarea>
  <div class="actions">
    <button class="primary" on:click={doEstimate} disabled={loading} type="button">
      {loading ? 'Estimating…' : 'Estimate cost'}
    </button>
    <button on:click={doTrigger} disabled={loading || !estimate} type="button">
      {confirmTrigger ? 'Click again to confirm' : 'Initiate restore'}
    </button>
  </div>
</div>

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
  .actions { display: flex; gap: 0.5rem; margin-top: 0.75rem; }
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 1rem;
  }
  .big { font-size: 1.3rem; font-weight: 500; }
</style>
