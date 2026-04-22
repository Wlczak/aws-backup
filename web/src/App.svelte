<script lang="ts">
  import { route, go } from './lib/router';
  import Dashboard from './routes/Dashboard.svelte';
  import Files from './routes/Files.svelte';
  import Logs from './routes/Logs.svelte';
  import Settings from './routes/Settings.svelte';
  import Restore from './routes/Restore.svelte';

  const tabs = [
    { id: 'dashboard', label: 'Dashboard' },
    { id: 'files', label: 'Files' },
    { id: 'logs', label: 'Logs' },
    { id: 'settings', label: 'Settings' },
    { id: 'restore', label: 'Restore' },
  ];
</script>

<div class="shell">
  <header>
    <div class="brand">aws-backup</div>
    <nav>
      {#each tabs as t}
        <button
          class:active={$route === t.id}
          on:click={() => go(t.id)}
          type="button"
        >{t.label}</button>
      {/each}
    </nav>
  </header>

  <main>
    {#if $route === 'dashboard'}<Dashboard />
    {:else if $route === 'files' || $route === 'index'}<Files />
    {:else if $route === 'logs'}<Logs />
    {:else if $route === 'settings'}<Settings />
    {:else if $route === 'restore'}<Restore />
    {:else}<p class="muted">Unknown route: {$route}</p>
    {/if}
  </main>
</div>

<style>
  .shell {
    max-width: 1200px;
    margin: 0 auto;
    padding: 1rem 1.5rem 3rem;
  }
  header {
    display: flex;
    align-items: center;
    gap: 1.5rem;
    padding: 0.5rem 0 1rem;
    border-bottom: 1px solid var(--border);
    margin-bottom: 1.5rem;
  }
  .brand {
    font-weight: 600;
    font-size: 1.1rem;
    letter-spacing: 0.02em;
  }
  nav {
    display: flex;
    gap: 0.5rem;
  }
  nav button {
    background: transparent;
    border: 1px solid transparent;
  }
  nav button.active {
    border-color: var(--accent);
    color: var(--accent);
  }
  main {
    display: grid;
    gap: 1.25rem;
  }
</style>
