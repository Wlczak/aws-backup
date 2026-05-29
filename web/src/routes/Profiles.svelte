<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type ProfileInfo } from '../lib/api';
  import { toast } from '../lib/toast';

  let profiles = $state<ProfileInfo[]>([]);
  let active = $state('');
  let switchBlocked = $state(false);
  let blockedReason = $state('');
  let loading = $state(true);
  let busy = $state('');
  let newName = $state('');
  let renameOld = $state('');
  let renameName = $state('');

  let lastProfile = $derived(profiles.length <= 1);

  async function load() {
    loading = true;
    try {
      const resp = await api.profiles();
      profiles = resp.profiles;
      active = resp.active_profile;
      switchBlocked = resp.switch_blocked;
      blockedReason = resp.blocked_reason ?? '';
    } catch (e) {
      toast.error(`Failed to load profiles: ${e}`);
    } finally {
      loading = false;
    }
  }

  function reloadSoon() {
    window.setTimeout(() => window.location.reload(), 250);
  }

  async function activate(name: string) {
    if (name === active || switchBlocked) return;
    busy = `activate:${name}`;
    try {
      const info = await api.switchProfile(name);
      toast.success(`Switched to profile ${info.name}`);
      await load();
      reloadSoon();
    } catch (e) {
      toast.error(`Failed to switch profile: ${e}`);
      await load();
    } finally {
      busy = '';
    }
  }

  async function createProfile() {
    const name = newName.trim();
    if (!name) return;
    busy = 'create';
    try {
      await api.createProfile(name, true);
      toast.success(`Created profile ${name}`);
      newName = '';
      await load();
    } catch (e) {
      toast.error(`Failed to create profile: ${e}`);
    } finally {
      busy = '';
    }
  }

  function startRename(p: ProfileInfo) {
    renameOld = p.name;
    renameName = p.name;
  }

  async function renameProfile() {
    const name = renameName.trim();
    if (!renameOld || !name || name === renameOld) return;
    busy = `rename:${renameOld}`;
    const wasActive = renameOld === active;
    try {
      const info = await api.renameProfile(renameOld, name);
      toast.success(`Renamed profile to ${info.name}`);
      renameOld = '';
      renameName = '';
      await load();
      if (wasActive) reloadSoon();
    } catch (e) {
      toast.error(`Failed to rename profile: ${e}`);
      await load();
    } finally {
      busy = '';
    }
  }

  async function deleteProfile(name: string) {
    if (name === active || lastProfile) return;
    if (!window.confirm(`Delete profile "${name}"? This removes its local config and index folder.`)) return;
    busy = `delete:${name}`;
    try {
      await api.deleteProfile(name);
      toast.success(`Deleted profile ${name}`);
      await load();
    } catch (e) {
      toast.error(`Failed to delete profile: ${e}`);
    } finally {
      busy = '';
    }
  }

  onMount(load);
</script>

<h1>Profiles</h1>

{#if switchBlocked}
  <div class="banner">
    Profile switching and active-profile rename are blocked: {blockedReason}
  </div>
{/if}

<section class="card create">
  <div>
    <h2>Create profile</h2>
    <p class="muted">New profiles clone the active profile but leave bucket and SQS queue URL blank. Switch to the new profile, then finish Settings.</p>
  </div>
  <div class="create-row">
    <input
      bind:value={newName}
      placeholder="profile-name"
      aria-label="New profile name"
      disabled={busy !== ''}
      onkeydown={(e) => { if (e.key === 'Enter') createProfile(); }}
    />
    <button type="button" class="primary" onclick={createProfile} disabled={busy !== '' || !newName.trim()}>
      Create
    </button>
  </div>
</section>

<section class="profiles">
  {#if loading}
    <p class="muted">Loading profiles…</p>
  {:else}
    {#each profiles as p}
      <article class="card profile-card">
        <div class="profile-main">
          <div class="profile-title">
            {#if renameOld === p.name}
              <input
                bind:value={renameName}
                aria-label="Rename profile"
                disabled={busy !== ''}
                onkeydown={(e) => { if (e.key === 'Enter') renameProfile(); }}
              />
            {:else}
              <h2>{p.name}</h2>
            {/if}
            {#if p.active}<span class="badge running">active</span>{/if}
          </div>
          <dl>
            <div><dt>Bucket</dt><dd>{p.bucket || 'not configured'}</dd></div>
            <div><dt>Config</dt><dd class="mono">{p.config_path}</dd></div>
            <div><dt>Index</dt><dd class="mono">{p.index_path}</dd></div>
          </dl>
        </div>
        <div class="profile-actions">
          {#if renameOld === p.name}
            <button type="button" class="primary" onclick={renameProfile} disabled={busy !== '' || !renameName.trim() || renameName.trim() === p.name}>Save</button>
            <button type="button" onclick={() => { renameOld = ''; renameName = ''; }} disabled={busy !== ''}>Cancel</button>
          {:else}
            <button type="button" onclick={() => activate(p.name)} disabled={p.active || switchBlocked || busy !== ''}>
              Activate
            </button>
            <button type="button" onclick={() => startRename(p)} disabled={(p.active && switchBlocked) || busy !== ''}>
              Rename
            </button>
            <button type="button" onclick={() => deleteProfile(p.name)} disabled={p.active || lastProfile || busy !== ''}>
              Delete
            </button>
          {/if}
        </div>
      </article>
    {/each}
  {/if}
</section>

<style>
  .banner {
    padding: 0.65rem 0.8rem;
    border: 1px solid var(--border);
    border-left: 3px solid var(--warn);
    border-radius: 6px;
    background: rgba(247, 181, 0, 0.08);
  }
  .create {
    display: flex;
    justify-content: space-between;
    gap: 1rem;
    align-items: end;
  }
  .create h2,
  .profile-card h2 {
    margin: 0;
    font-size: 1rem;
  }
  .create p {
    margin: 0.35rem 0 0;
  }
  .create-row,
  .profile-actions,
  .profile-title {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .profiles {
    display: grid;
    gap: 0.75rem;
  }
  .profile-card {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: 1rem;
  }
  .profile-main {
    display: grid;
    gap: 0.75rem;
    min-width: 0;
  }
  dl {
    display: grid;
    gap: 0.35rem;
    margin: 0;
  }
  dl div {
    display: grid;
    grid-template-columns: 5rem minmax(0, 1fr);
    gap: 0.75rem;
  }
  dt {
    color: var(--muted);
  }
  dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
  .profile-actions {
    justify-content: flex-end;
    align-content: start;
  }
  @media (max-width: 760px) {
    .create,
    .profile-card {
      grid-template-columns: 1fr;
      display: grid;
    }
    .profile-actions {
      justify-content: flex-start;
    }
  }
</style>
