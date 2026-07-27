<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Config } from '../lib/api';
  import { toast } from '../lib/toast';
  import { route, go } from '../lib/router';
  import SourceSettings from './settings/SourceSettings.svelte';
  import StorageSettings from './settings/StorageSettings.svelte';
  import SqsSettings from './settings/SqsSettings.svelte';
  import BackupSettings from './settings/BackupSettings.svelte';
  import ServerSettings from './settings/ServerSettings.svelte';
  import AboutSettings from './settings/AboutSettings.svelte';

  let cfg = $state<Config | null>(null);
  let saving = $state(false);
  let scheduleHuman = $state('');
  let pendingApply = $state(false);

  const sections = [
    { id: 'source',  label: 'Source'   },
    { id: 'storage', label: 'Storage'  },
    { id: 'sqs',     label: 'SQS'      },
    { id: 'backup',  label: 'Backup'   },
    { id: 'server',  label: 'Schedule & server' },
    { id: 'about',   label: 'About' },
  ] as const;
  type SectionId = typeof sections[number]['id'];

  // Active sub-section is derived from the hash route. `settings` alone
  // (no sub-path) lands on the first section.
  let active: SectionId = $derived.by<SectionId>(() => {
    const r = $route;
    const slash = r.indexOf('/');
    if (slash === -1) return 'source';
    const sub = r.slice(slash + 1);
    return sections.some(s => s.id === sub) ? (sub as SectionId) : 'source';
  });

  async function load() {
    try {
      const resp = await api.settings();
      const { pending_apply, ...rest } = resp;
      cfg = rest as Config;
      pendingApply = pending_apply;
      scheduleHuman = humanize(cfg.backup.schedule);
    } catch (e) {
      toast.error(`Failed to load settings: ${e}`);
    }
  }

  async function save() {
    if (!cfg) return;
    saving = true;
    try {
      const resp = await api.updateSettings(cfg);
      const { pending_apply, ...rest } = resp;
      cfg = rest as Config;
      pendingApply = pending_apply;
      scheduleHuman = humanize(cfg.backup.schedule);
      toast.success(pending_apply
        ? 'Saved — will apply when the current backup finishes'
        : 'Settings saved');
    } catch (e) {
      toast.error(`Failed to save settings: ${e}`);
    } finally {
      saving = false;
    }
  }

  function humanize(expr: string): string {
    if (!expr) return '(no schedule — manual only)';
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
    if (cfg) scheduleHuman = humanize(cfg.backup.schedule);
  });
</script>

<h1>Settings</h1>

<nav class="subnav">
  {#each sections as s}
    <button
      class:active={active === s.id}
      onclick={() => go('settings/' + s.id)}
      type="button"
    >{s.label}</button>
  {/each}
</nav>

{#if pendingApply}
  <div class="banner">
    Settings saved — will apply when the current backup finishes.
  </div>
{/if}

{#if active === 'about'}
  <AboutSettings />
{:else if cfg}
  {#if active === 'source'}
    <SourceSettings bind:cfg />
  {:else if active === 'storage'}
    <StorageSettings bind:cfg />
  {:else if active === 'sqs'}
    <SqsSettings bind:cfg />
  {:else if active === 'backup'}
    <BackupSettings bind:cfg />
  {:else if active === 'server'}
    <ServerSettings bind:cfg {scheduleHuman} />
  {/if}

  <div class="actions">
    <button class="primary" onclick={save} disabled={saving} type="button">
      {saving ? 'Saving…' : 'Save'}
    </button>
    <button onclick={load} disabled={saving} type="button">Reload</button>
  </div>
{/if}

<style>
  :global(.card h2) { font-size: 1rem; margin: 0 0 0.75rem; }
  .subnav {
    display: flex;
    gap: 0.25rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    margin-bottom: 1rem;
  }
  .subnav button {
    background: transparent;
    border: 1px solid transparent;
    border-bottom: none;
    border-radius: 4px 4px 0 0;
    padding: 0.4rem 0.8rem;
    margin-bottom: -1px;
    color: var(--muted);
  }
  .subnav button.active {
    border-color: var(--border);
    background: var(--bg);
    color: var(--text);
  }
  .actions { display: flex; gap: 0.5rem; margin-top: 0.5rem; }
  .banner {
    padding: 0.5rem 0.75rem;
    margin-bottom: 0.75rem;
    border: 1px solid var(--border);
    border-left: 3px solid #d29922;
    border-radius: 4px;
    background: rgba(210, 153, 34, 0.08);
    font-size: 0.9rem;
  }
  :global(.card .row) { display: flex; align-items: center; gap: 0.75rem; margin-top: 0.75rem; flex-wrap: wrap; }
  :global(.card .row-2) {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.75rem;
  }
  :global(.card label) {
    display: grid;
    gap: 0.3rem;
    font-size: 0.85rem;
    color: var(--muted);
    margin-bottom: 0.65rem;
  }
  :global(.card .folder-field) {
    display: grid;
    gap: 0.3rem;
    margin-bottom: 0.65rem;
  }
  :global(.card .folder-field > label) {
    margin-bottom: 0;
  }
  :global(.card label.checkbox) {
    grid-template-columns: auto 1fr;
    align-items: start;
    gap: 0.5rem;
  }
  :global(.card label.checkbox input) { margin-top: 0.15rem; }
  :global(.card label input[type="text"]),
  :global(.card label input[type="number"]),
  :global(.card label input[type="password"]),
  :global(.card label select) {
    font-family: inherit;
    padding: 0.35rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--bg);
    color: var(--text);
  }
  :global(.card label input[type="number"]) { max-width: 12rem; }
  @media (max-width: 640px) {
    :global(.card .row-2) { grid-template-columns: 1fr; }
  }
</style>
