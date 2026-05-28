<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '../lib/api';
  import { go } from '../lib/router';

  let active = $state('');

  async function load() {
    try {
      const resp = await api.profiles();
      active = resp.active_profile;
    } catch {
      active = '';
    }
  }

  onMount(load);
</script>

<div class="profile-switcher">
  <span>{active || 'No profile'}</span>
  <button type="button" onclick={() => go('profiles')}>Switch profile</button>
</div>

<style>
  .profile-switcher {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.85rem;
    color: var(--muted);
  }
  button {
    padding: 0.35rem 0.65rem;
  }
  @media (max-width: 720px) {
    .profile-switcher {
      width: 100%;
      margin-left: 0;
      justify-content: flex-start;
    }
  }
</style>
