<script lang="ts">
  import { onMount } from 'svelte';
  import FolderInput from '../components/FolderInput.svelte';
  import { api, type AuthStatus, type Config } from '../lib/api';
  import { go } from '../lib/router';
  import { toast } from '../lib/toast';

  type Props = {
    status: AuthStatus;
    onStatus: (status: AuthStatus, loggedOutMessage?: string) => void;
  };
  let { status, onStatus }: Props = $props();

  const steps = ['Password', 'Source', 'S3 storage', 'Done'];
  let step = $state(0);
  let cfg = $state<Config | null>(null);
  let password = $state('');
  let confirmation = $state('');
  let busy = $state(false);
  let loading = $state(false);
  let error = $state('');
  let sourceMessage = $state('');
  let storageMessage = $state('');
  let completedStatus = $state<AuthStatus | null>(null);

  async function loadSettings() {
    loading = true;
    error = '';
    try {
      const response = await api.settings();
      const { pending_apply: _, ...settings } = response;
      cfg = settings as Config;
    } catch (e) {
      error = `Could not load setup settings: ${e}`;
    } finally {
      loading = false;
    }
  }

  async function createPassword() {
    error = '';
    if (!password.trim()) {
      error = 'Enter a password.';
      return;
    }
    if (password !== confirmation) {
      error = 'The passwords do not match.';
      return;
    }
    busy = true;
    try {
      const nextStatus = await api.setupPassword(password);
      password = '';
      confirmation = '';
      toast.success('Password created.');
      onStatus(nextStatus, 'Password created. Sign in to continue setup.');
    } catch (e) {
      error = String(e);
      toast.error(`Could not create password: ${e}`);
    } finally {
      busy = false;
    }
  }

  function sourceReady(): boolean {
    if (!cfg) return false;
    if (cfg.source.type === 'localdir') return cfg.source.localdir.root.trim() !== '';
    if (cfg.source.type === 'smb') {
      return cfg.source.smb.host.trim() !== '' && cfg.source.smb.share.trim() !== '';
    }
    return false;
  }

  async function saveSource() {
    if (!cfg || !sourceReady()) {
      error = 'Choose a local folder or enter the SMB host and share.';
      return;
    }
    busy = true;
    error = '';
    sourceMessage = '';
    try {
      const response = await api.updateSettings(cfg);
      const { pending_apply: _, ...settings } = response;
      cfg = settings as Config;
      const result = await api.testSource();
      sourceMessage = result.message ?? (result.ok ? 'Source is reachable.' : 'Source test failed.');
      if (!result.ok) {
        error = sourceMessage;
        return;
      }
      toast.success(sourceMessage);
      step = 2;
    } catch (e) {
      error = String(e);
      toast.error(`Could not save source: ${e}`);
    } finally {
      busy = false;
    }
  }

  function storageReady(): boolean {
    return !!cfg?.s3.bucket.trim() && !!cfg?.s3.region.trim() && !!cfg?.s3.storage_class;
  }

  async function saveStorage() {
    if (!cfg || !storageReady()) {
      error = 'Bucket, region, and storage class are required.';
      return;
    }
    busy = true;
    error = '';
    storageMessage = '';
    try {
      const response = await api.updateSettings(cfg);
      const { pending_apply: _, ...settings } = response;
      cfg = settings as Config;
      const result = await api.testStorage();
      storageMessage = result.message ?? (result.ok ? 'Bucket is reachable.' : 'Storage test failed.');
      if (!result.ok) {
        error = storageMessage;
        return;
      }
      completedStatus = await api.completeSetup();
      toast.success(storageMessage);
      step = 3;
    } catch (e) {
      error = String(e);
      toast.error(`Could not complete storage setup: ${e}`);
    } finally {
      busy = false;
    }
  }

  function openDashboard() {
    if (!completedStatus) return;
    go('dashboard');
    onStatus(completedStatus);
  }

  onMount(() => {
    if (status.password_set && status.authenticated) {
      step = 1;
      void loadSettings();
    }
  });
</script>

<div class="setup-shell">
  <section class="setup-card">
    <div class="brand">aws-backup</div>
    <div class="heading">
      <div>
        <div class="eyebrow">First-time setup</div>
        <h1>Let’s configure your backup</h1>
      </div>
      <span class="step-count">Step {step + 1} of {steps.length}</span>
    </div>

    <ol class="stepper" aria-label="Setup progress">
      {#each steps as label, index}
        <li class:active={index === step} class:complete={index < step}>
          <span>{index < step ? '✓' : index + 1}</span>
          {label}
        </li>
      {/each}
    </ol>

    {#if error}
      <div class="field-error" role="alert">{error}</div>
    {/if}

    {#if step === 0}
      <div class="panel">
        <h2>Create your login password</h2>
        <p class="muted">This password protects the local web app. Only a bcrypt hash is stored.</p>
        <label>
          <span>Password</span>
          <input type="password" bind:value={password} autocomplete="new-password" />
        </label>
        <label>
          <span>Confirm password</span>
          <input
            type="password"
            bind:value={confirmation}
            autocomplete="new-password"
            onkeydown={(e) => { if (e.key === 'Enter') void createPassword(); }}
          />
        </label>
        <div class="actions">
          <button class="primary" type="button" onclick={() => void createPassword()} disabled={busy}>
            {busy ? 'Creating…' : 'Create password'}
          </button>
        </div>
      </div>
    {:else if loading}
      <div class="panel muted">Loading setup settings…</div>
    {:else if step > 0 && !cfg}
      <div class="panel">
        <h2>Settings are unavailable</h2>
        <p class="muted">Retry loading the active profile before continuing.</p>
        <div class="actions">
          <button type="button" onclick={() => void loadSettings()}>Retry</button>
        </div>
      </div>
    {:else if step === 1 && cfg}
      <div class="panel">
        <h2>Choose what to back up</h2>
        <label>
          <span>Source type</span>
          <select bind:value={cfg.source.type}>
            <option value="">Choose a source…</option>
            <option value="localdir">Local folder</option>
            <option value="smb">SMB network share</option>
          </select>
        </label>
        {#if cfg.source.type === 'localdir'}
          <div class="field">
            <span>Folder</span>
            <FolderInput id="setup-source" bind:value={cfg.source.localdir.root} placeholder="Choose a folder" ariaLabel="Backup source folder" />
          </div>
        {:else if cfg.source.type === 'smb'}
          <div class="grid">
            <label><span>Host</span><input type="text" bind:value={cfg.source.smb.host} placeholder="nas.local" /></label>
            <label><span>Port</span><input type="number" min="1" max="65535" bind:value={cfg.source.smb.port} /></label>
            <label><span>Share</span><input type="text" bind:value={cfg.source.smb.share} /></label>
            <label><span>Path (optional)</span><input type="text" bind:value={cfg.source.smb.path} /></label>
            <label><span>Username</span><input type="text" bind:value={cfg.source.smb.username} autocomplete="off" /></label>
            <label><span>Password</span><input type="password" bind:value={cfg.source.smb.password} autocomplete="new-password" /></label>
          </div>
          <label><span>Domain (optional)</span><input type="text" bind:value={cfg.source.smb.domain} /></label>
        {/if}
        {#if sourceMessage}<p class="result">{sourceMessage}</p>{/if}
        <div class="actions">
          <button class="primary" type="button" onclick={() => void saveSource()} disabled={busy || !sourceReady()}>
            {busy ? 'Saving and testing…' : 'Save and test source'}
          </button>
        </div>
      </div>
    {:else if step === 2 && cfg}
      <div class="panel">
        <h2>Connect an existing S3 bucket</h2>
        <p class="muted">The app checks access but never creates the bucket. Leave credentials empty to use the default AWS credential chain.</p>
        <div class="grid">
          <label><span>Bucket</span><input type="text" bind:value={cfg.s3.bucket} /></label>
          <label><span>Region</span><input type="text" bind:value={cfg.s3.region} placeholder="us-east-1" /></label>
        </div>
        <label>
          <span>Endpoint (optional)</span>
          <input type="text" bind:value={cfg.s3.endpoint} placeholder="Leave empty for AWS S3" />
        </label>
        <label class="checkbox">
          <input type="checkbox" bind:checked={cfg.s3.use_path_style} />
          <span>Use path-style addressing for this S3-compatible provider</span>
        </label>
        <div class="grid">
          <label><span>Access key ID</span><input type="text" bind:value={cfg.s3.access_key_id} autocomplete="off" /></label>
          <label><span>Secret access key</span><input type="password" bind:value={cfg.s3.secret_access_key} autocomplete="new-password" /></label>
        </div>
        <div class="grid">
          <label><span>Key prefix</span><input type="text" bind:value={cfg.s3.key_prefix} placeholder="backups/" /></label>
          <label>
            <span>Storage class</span>
            <select bind:value={cfg.s3.storage_class}>
              <option value="DEEP_ARCHIVE">Glacier Deep Archive</option>
              <option value="STANDARD">Standard</option>
            </select>
          </label>
        </div>
        {#if storageMessage}<p class="result">{storageMessage}</p>{/if}
        <div class="actions split">
          <button type="button" onclick={() => { error = ''; step = 1; }} disabled={busy}>Back</button>
          <button class="primary" type="button" onclick={() => void saveStorage()} disabled={busy || !storageReady()}>
            {busy ? 'Saving and testing…' : 'Save, test, and finish'}
          </button>
        </div>
      </div>
    {:else if step === 3}
      <div class="panel done">
        <div class="done-mark">✓</div>
        <h2>Setup complete</h2>
        <p class="muted">Your password, source, and S3 bucket are configured. Backups remain manual until you choose a schedule.</p>
        <div class="actions">
          <button class="primary" type="button" onclick={openDashboard}>Open dashboard</button>
        </div>
      </div>
    {/if}
  </section>
</div>

<style>
  .setup-shell { min-height: 100vh; display: grid; place-items: center; padding: 2rem 1rem; }
  .setup-card {
    width: min(820px, 100%);
    display: grid;
    gap: 1.25rem;
    padding: 2rem;
    border: 1px solid var(--border);
    border-radius: 16px;
    background: var(--surface);
    box-shadow: 0 24px 80px rgba(0, 0, 0, 0.22);
  }
  .brand { font-weight: 700; letter-spacing: 0.03em; }
  .heading, .actions, .split { display: flex; align-items: center; gap: 0.75rem; }
  .heading { justify-content: space-between; }
  h1, h2 { margin: 0; }
  h1 { font-size: clamp(1.55rem, 4vw, 2.2rem); }
  h2 { font-size: 1.2rem; }
  .eyebrow { color: var(--accent); font-size: 0.72rem; font-weight: 700; letter-spacing: 0.12em; text-transform: uppercase; }
  .step-count { color: var(--muted); white-space: nowrap; }
  .stepper { display: grid; grid-template-columns: repeat(4, 1fr); gap: 0.5rem; padding: 0; margin: 0; list-style: none; }
  .stepper li { display: flex; align-items: center; gap: 0.45rem; color: var(--muted); font-size: 0.85rem; }
  .stepper li span { width: 1.65rem; height: 1.65rem; display: grid; place-items: center; border: 1px solid var(--border); border-radius: 50%; }
  .stepper li.active { color: var(--text); }
  .stepper li.active span { border-color: var(--accent); color: var(--accent); }
  .stepper li.complete span { border-color: var(--ok); color: var(--ok); }
  .panel { display: grid; gap: 0.9rem; padding: 1.25rem; border: 1px solid var(--border); border-radius: 10px; background: var(--bg); }
  label, .field { display: grid; gap: 0.35rem; color: var(--muted); font-size: 0.88rem; }
  .grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0.8rem; }
  .checkbox { display: grid; grid-template-columns: auto 1fr; align-items: center; }
  .checkbox input { margin: 0; }
  .actions { justify-content: flex-end; margin-top: 0.25rem; }
  .split { justify-content: space-between; }
  .field-error { padding: 0.7rem 0.85rem; border: 1px solid var(--err); border-radius: 8px; color: var(--err); background: rgba(239, 80, 80, 0.08); }
  .result { margin: 0; color: var(--ok); }
  .done { text-align: center; justify-items: center; padding: 2rem; }
  .done-mark { width: 3.5rem; height: 3.5rem; display: grid; place-items: center; border: 2px solid var(--ok); border-radius: 50%; color: var(--ok); font-size: 1.8rem; }
  @media (max-width: 640px) {
    .setup-card { padding: 1.1rem; }
    .grid { grid-template-columns: 1fr; }
    .stepper li { font-size: 0; }
    .stepper li span { font-size: 0.85rem; }
  }
</style>
