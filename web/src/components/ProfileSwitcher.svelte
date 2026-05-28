<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type ProfileInfo } from '../lib/api';
  import { toast } from '../lib/toast';

  let profiles = $state<ProfileInfo[]>([]);
  let active = $state('');
  let blocked = $state(false);
  let reason = $state('');
  let loading = $state(true);
  let switching = $state(false);
  let creating = $state(false);
  let newName = $state('');

  async function load() {
    loading = true;
    try {
      const resp = await api.profiles();
      profiles = resp.profiles;
      active = resp.active_profile;
      blocked = resp.switch_blocked;
      reason = resp.blocked_reason ?? '';
    } catch (e) {
      toast.error(`Failed to load profiles: ${e}`);
    } finally {
      loading = false;
    }
  }

  async function changeProfile(e: Event) {
    const next = (e.currentTarget as HTMLSelectElement).value;
    if (!next || next === active) return;
    switching = true;
    try {
      const info = await api.switchProfile(next);
      active = info.name;
      toast.success(`Switched to profile ${info.name}`);
      await load();
      window.setTimeout(() => window.location.reload(), 250);
    } catch (err) {
      toast.error(`Failed to switch profile: ${err}`);
      await load();
    } finally {
      switching = false;
    }
  }

  async function createAndSwitch() {
    const name = newName.trim();
    if (!name) return;
    switching = true;
    try {
      await api.createProfile(name, true);
      const info = await api.switchProfile(name);
      active = info.name;
      toast.success(`Created and switched to profile ${info.name}`);
      await load();
      window.setTimeout(() => window.location.reload(), 250);
    } catch (err) {
      toast.error(`Failed to create profile: ${err}`);
      await load();
    } finally {
      switching = false;
      creating = false;
      newName = '';
    }
  }

  onMount(load);
</script>

<div class="profile-switcher" title={blocked ? reason : 'Active backup profile'}>
  <span>Profile</span>
  <select bind:value={active} onchange={changeProfile} disabled={loading || switching || blocked}>
    {#each profiles as p}
      <option value={p.name}>{p.name}{p.bucket ? ` · ${p.bucket}` : ''}</option>
    {/each}
  </select>
  {#if creating}
    <input
      bind:value={newName}
      aria-label="New profile name"
      placeholder="new-profile"
      disabled={switching}
      onkeydown={(e) => { if (e.key === 'Enter') createAndSwitch(); }}
    />
    <button type="button" onclick={createAndSwitch} disabled={switching || !newName.trim()}>Create</button>
    <button type="button" onclick={() => { creating = false; newName = ''; }} disabled={switching}>Cancel</button>
  {:else}
    <button type="button" onclick={() => { creating = true; }} disabled={loading || switching || blocked}>New</button>
  {/if}
</div>

<style>
  .profile-switcher {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 0.45rem;
    font-size: 0.85rem;
    color: var(--muted);
  }
  select {
    max-width: 16rem;
  }
  input {
    width: 9rem;
  }
  button {
    padding: 0.35rem 0.6rem;
  }
  @media (max-width: 720px) {
    .profile-switcher {
      width: 100%;
      margin-left: 0;
      justify-content: flex-start;
    }
  }
</style>
