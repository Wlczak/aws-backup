<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Config, type TestResult } from '../lib/api';

  let cfg = $state<Config | null>(null);
  let raw = $state('');
  let err = $state('');
  let msg = $state('');
  let saving = $state(false);
  let sourceTest = $state<TestResult | null>(null);
  let storageTest = $state<TestResult | null>(null);
  let scheduleInput = $state('');
  let scheduleHuman = $state('');

  async function load() {
    try {
      cfg = await api.settings();
      raw = JSON.stringify(cfg, null, 2);
      scheduleInput = (cfg as any)?.backup?.schedule ?? '';
      scheduleHuman = humanize(scheduleInput);
      err = '';
    } catch (e) {
      err = String(e);
    }
  }

  async function save() {
    saving = true;
    err = '';
    msg = '';
    try {
      const parsed = JSON.parse(raw);
      // reflect current schedule input field in the saved config
      if (parsed.backup) parsed.backup.schedule = scheduleInput;
      cfg = await api.updateSettings(parsed);
      raw = JSON.stringify(cfg, null, 2);
      msg = 'Saved.';
    } catch (e) {
      err = String(e);
    } finally {
      saving = false;
    }
  }

  async function testSource() {
    sourceTest = null;
    try {
      sourceTest = await api.testSource();
    } catch (e) {
      sourceTest = { ok: false, message: String(e) };
    }
  }
  async function testStorage() {
    storageTest = null;
    try {
      storageTest = await api.testStorage();
    } catch (e) {
      storageTest = { ok: false, message: String(e) };
    }
  }

  function humanize(expr: string): string {
    if (!expr) return '(no schedule — manual only)';
    // Map a few common patterns; fall back to the raw expression.
    const presets: Record<string, string> = {
      '0 2 * * *': 'Every day at 02:00',
      '0 */6 * * *': 'Every 6 hours',
      '*/15 * * * *': 'Every 15 minutes',
      '0 0 * * 0': 'Weekly (Sunday midnight)',
      '0 0 1 * *': 'Monthly (1st of month)',
    };
    return presets[expr] ?? expr;
  }

  onMount(load);
  $effect(() => {
    scheduleHuman = humanize(scheduleInput);
  });
</script>

<h1>Settings</h1>
{#if err}<div class="card err">{err}</div>{/if}
{#if msg}<div class="card ok">{msg}</div>{/if}

<div class="card">
  <label>
    Schedule (cron)
    <input type="text" bind:value={scheduleInput} placeholder="0 2 * * *" />
  </label>
  <p class="muted">{scheduleHuman}</p>
</div>

<div class="card">
  <div class="row">
    <button on:click={testSource} type="button">Test source</button>
    {#if sourceTest}
      <span class="badge {sourceTest.ok ? 'ok' : 'err'}">{sourceTest.ok ? 'ok' : 'fail'}</span>
      <span class="muted">{sourceTest.message ?? ''}</span>
    {/if}
  </div>
  <div class="row">
    <button on:click={testStorage} type="button">Test storage</button>
    {#if storageTest}
      <span class="badge {storageTest.ok ? 'ok' : 'err'}">{storageTest.ok ? 'ok' : 'fail'}</span>
      <span class="muted">{storageTest.message ?? ''}</span>
    {/if}
  </div>
</div>

<div class="card">
  <div class="label">config.json</div>
  <p class="muted">Edit as JSON. Fields shown as <code>***</code> preserve their stored value when you save.</p>
  <textarea bind:value={raw} spellcheck="false"></textarea>
  <div class="actions">
    <button class="primary" on:click={save} disabled={saving} type="button">
      {saving ? 'Saving…' : 'Save'}
    </button>
    <button on:click={load} disabled={saving} type="button">Reload</button>
  </div>
</div>

<style>
  .err { color: var(--err); border-color: var(--err); }
  .ok { color: var(--ok); border-color: var(--ok); }
  .label { font-size: 0.8rem; color: var(--muted); margin-bottom: 0.25rem; }
  textarea {
    width: 100%;
    min-height: 420px;
    font-family: ui-monospace, monospace;
    font-size: 0.85rem;
    margin-top: 0.5rem;
  }
  .row { display: flex; align-items: center; gap: 0.75rem; margin: 0.4rem 0; }
  .actions { display: flex; gap: 0.5rem; margin-top: 0.5rem; }
  label { display: grid; gap: 0.3rem; font-size: 0.85rem; color: var(--muted); }
  label input { font-family: ui-monospace, monospace; }
</style>
