<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type UpdateStatus } from '../../lib/api';
  import { toast } from '../../lib/toast';
  import { updateStatus } from '../../lib/update';

  let status = $state<UpdateStatus | null>(null);
  let checking = $state(false);
  let saving = $state(false);

  function apply(next: UpdateStatus) {
    status = next;
    updateStatus.set(next);
  }

  async function load() {
    try { apply(await api.updateStatus()); }
    catch (e) { toast.error(`Failed to load update information: ${e}`); }
  }

  async function check() {
    checking = true;
    try {
      const next = await api.checkForUpdate();
      apply(next);
      if (next.state === 'up_to_date') toast.success('This exact release is installed.');
      else if (next.state === 'error') toast.error(next.error ?? 'Update check failed.');
    } catch (e) { toast.error(`Update check failed: ${e}`); }
    finally { checking = false; }
  }

  async function setAutoCheck(enabled: boolean) {
    saving = true;
    try {
      apply(await api.saveUpdateSettings(enabled));
      toast.success(enabled ? 'Automatic update checks enabled.' : 'Automatic update checks disabled.');
    } catch (e) { toast.error(`Failed to save update preference: ${e}`); }
    finally { saving = false; }
  }

  onMount(load);
</script>

<section class="card about">
  <div>
    <h2>About aws-backup</h2>
    <p class="muted">A self-contained backup service for local folders and SMB shares, backed by Amazon S3.</p>
  </div>

  <dl>
    <div><dt>Installed version</dt><dd class="mono">{status?.current_version ?? 'Loading…'}</dd></div>
    {#if status?.latest}<div><dt>Newest release</dt><dd class="mono">{status.latest.tag_name}</dd></div>{/if}
  </dl>

  <label class="checkbox">
    <input
      type="checkbox"
      checked={status?.auto_check ?? false}
      disabled={!status || saving}
      onchange={(e) => void setAutoCheck(e.currentTarget.checked)}
    />
    <span><strong>Automatically check for updates on startup</strong><small>Checks GitHub after the server starts and asks before installing anything.</small></span>
  </label>

  <div class="row">
    <button class="primary" type="button" onclick={check} disabled={checking}>{checking ? 'Checking…' : 'Check for updates'}</button>
    {#if status?.state === 'error'}<span class="error">{status.error}</span>
    {:else if status?.state === 'up_to_date'}<span class="ok">Exact release installed</span>
    {:else if status?.state === 'available'}<span class="warn">Update available</span>
    {:else if status?.state === 'ignored'}<span class="muted">Update ignored until next startup</span>{/if}
  </div>

  <div class="links">
    <a href="https://github.com/Wlczak/aws-backup" target="_blank" rel="noreferrer">GitHub repository</a>
    <a href="https://github.com/Wlczak/aws-backup/releases" target="_blank" rel="noreferrer">Release history</a>
  </div>
  <p class="muted credits">Created by Adam Vlček. Built with Go and Svelte.</p>
</section>

<style>
  .about { display: grid; gap: 1.2rem; max-width: 760px; }
  h2, p { margin: 0; }
  dl { display: grid; gap: 0.55rem; margin: 0; }
  dl div { display: grid; grid-template-columns: 10rem 1fr; gap: 1rem; }
  dt { color: var(--muted); }
  dd { margin: 0; }
  .checkbox span { display: grid; gap: 0.2rem; }
  .checkbox small { color: var(--muted); font-weight: normal; }
  .links { display: flex; gap: 1rem; flex-wrap: wrap; }
  .credits { font-size: 0.9rem; }
  .ok { color: var(--ok); }
  .warn { color: var(--warn); }
  .error { color: var(--err); }
  @media (max-width: 520px) { dl div { grid-template-columns: 1fr; gap: 0.15rem; } }
</style>
