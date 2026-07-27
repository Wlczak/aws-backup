<script lang="ts">
  import { route, go } from './lib/router';
  import { api, setUnauthorizedHandler, type AuthStatus } from './lib/api';
  import { toast } from './lib/toast';
  import { clientLogDebug, installClientLogging, setClientLogDebug } from './lib/client-logs';
  import { updateStatus } from './lib/update';
  import Dashboard from './routes/Dashboard.svelte';
  import Download from './routes/Download.svelte';
  import Files from './routes/Files.svelte';
  import Logs from './routes/Logs.svelte';
  import Settings from './routes/Settings.svelte';
  import Restore from './routes/Restore.svelte';
  import Profiles from './routes/Profiles.svelte';
  import Toaster from './components/Toaster.svelte';
  import ApiActivity from './components/ApiActivity.svelte';
  import ProfileSwitcher from './components/ProfileSwitcher.svelte';
  import Onboarding from './routes/Onboarding.svelte';
  import { onMount } from 'svelte';

  const tabs = [
    { id: 'dashboard', label: 'Dashboard' },
    { id: 'files', label: 'Files' },
    { id: 'download', label: 'Download' },
    { id: 'logs', label: 'Logs' },
    { id: 'settings', label: 'Settings' },
    { id: 'restore', label: 'Restore' },
  ];

  type AuthPhase = 'checking' | 'password-setup' | 'logged-out' | 'onboarding' | 'authenticated';

  let authPhase = $state<AuthPhase>('checking');
  let authStatus = $state<AuthStatus>({ password_set: false, authenticated: false, setup_required: true });
  let authPassword = $state('');
  let authBusy = $state(false);
  let authMessage = $state('');
  let updateBusy = $state(false);
  let updateTransition = $state<'restart' | 'shutdown' | null>(null);

  function applyAuth(status: AuthStatus, loggedOutMessage?: string) {
    authStatus = status;
    if (!status.password_set) {
      authPhase = 'password-setup';
      authMessage = 'No password is configured yet.';
      authPassword = '';
      return;
    }
    authPhase = !status.authenticated
      ? 'logged-out'
      : status.setup_required
        ? 'onboarding'
        : 'authenticated';
    authMessage = status.authenticated
      ? ''
      : (loggedOutMessage ?? 'Enter the password to unlock the app.');
    if (status.authenticated) {
      authPassword = '';
      if (!status.setup_required) void refreshUpdateStatus();
    }
  }

  async function refreshUpdateStatus() {
    try {
      const next = await api.updateStatus();
      updateStatus.set(next);
      if (next.state === 'checking') setTimeout(() => void refreshUpdateStatus(), 750);
    } catch { /* Update checks must never interfere with login or startup. */ }
  }

  async function ignoreUpdate() {
    try { updateStatus.set(await api.ignoreUpdate()); }
    catch (e) { toast.error(`Could not ignore update: ${e}`); }
  }

  async function installUpdate(action: 'restart' | 'shutdown') {
    if (updateBusy) return;
    updateBusy = true;
    try {
      await api.installUpdate(action);
      updateTransition = action;
      if (action === 'restart') setTimeout(waitForRestart, 1200);
    } catch (e) {
      toast.error(`Update installation failed: ${e}`);
      updateBusy = false;
    }
  }

  async function waitForRestart() {
    try {
      await api.authStatus();
      window.location.reload();
    } catch {
      setTimeout(waitForRestart, 750);
    }
  }

  async function refreshAuth() {
    try {
      applyAuth(await api.authStatus());
    } catch (e) {
      authPhase = 'logged-out';
      authMessage = String(e);
    }
  }

  async function login() {
    if (authBusy || authPassword.trim() === '') return;
    authBusy = true;
    try {
      applyAuth(await api.login(authPassword));
      toast.success('Signed in.');
    } catch (e) {
      toast.error(String(e));
      authMessage = 'Enter the password to unlock the app.';
    } finally {
      authBusy = false;
      authPassword = '';
    }
  }

  async function logout() {
    if (authBusy) return;
    authBusy = true;
    try {
      applyAuth(await api.logout());
      updateStatus.set(null);
      toast.info('Signed out.');
    } catch (e) {
      toast.error(String(e));
    } finally {
      authBusy = false;
      authPassword = '';
    }
  }

  onMount(() => {
    const teardownLogging = installClientLogging({
      onError: (message) => toast.error(message),
    });
    setUnauthorizedHandler(() => {
      if (authPhase === 'authenticated' || authPhase === 'onboarding') {
        applyAuth(
          { ...authStatus, authenticated: false },
          'Session expired. Please sign in again to continue.',
        );
      }
    });
    void refreshAuth();
    return () => {
      teardownLogging();
      setUnauthorizedHandler(null);
    };
  });
</script>

<ApiActivity />

{#if authPhase === 'authenticated'}
  <div class="shell">
    <header>
      <div class="brand">aws-backup</div>
      <nav>
        {#each tabs as t}
          <button
            class:active={$route === t.id || $route.startsWith(t.id + '/')}
            onclick={() => go(t.id)}
            type="button"
          >{t.label}</button>
        {/each}
      </nav>
      <label class="debug-toggle" title="Capture info/debug console entries and keep them on the Logs page">
        <input
          checked={$clientLogDebug}
          onchange={(e) => setClientLogDebug((e.currentTarget as HTMLInputElement).checked)}
          type="checkbox"
        />
        <span>Debug logs</span>
      </label>
      <ProfileSwitcher />
      <button class="logout" onclick={logout} disabled={authBusy} type="button">
        {authBusy ? 'Signing out…' : 'Sign out'}
      </button>
    </header>

    <main>
      {#if $route === 'dashboard'}<Dashboard />
      {:else if $route === 'files' || $route === 'index'}<Files />
      {:else if $route === 'download'}<Download />
      {:else if $route === 'logs'}<Logs />
      {:else if $route === 'settings' || $route.startsWith('settings/')}<Settings />
      {:else if $route === 'restore'}<Restore />
      {:else if $route === 'profiles'}<Profiles />
      {:else if $route === 'download'}<Download />
      {:else}<p class="muted">Unknown route: {$route}</p>
      {/if}
    </main>
  </div>
{:else if authPhase === 'password-setup' || authPhase === 'onboarding'}
  <Onboarding status={authStatus} onStatus={applyAuth} />
{:else}
  <div class="auth-shell">
    <div class="auth-card">
      <div class="brand">aws-backup</div>
      {#if authPhase === 'checking'}
        <p class="muted">Checking authentication…</p>
      {:else}
        <h1>Sign in</h1>
        <p class="muted">{authMessage}</p>
        <label>
          Password
          <input
            bind:value={authPassword}
            autocomplete="current-password"
            onkeydown={(e) => { if (e.key === 'Enter') void login(); }}
            type="password"
          />
        </label>
        <div class="actions">
          <button class="primary" onclick={login} disabled={authBusy || authPassword.trim() === ''} type="button">
            {authBusy ? 'Signing in…' : 'Sign in'}
          </button>
        </div>
      {/if}
    </div>
  </div>
{/if}

{#if authPhase === 'authenticated' && $updateStatus?.state === 'available' && !updateTransition}
  <div class="modal-backdrop" role="presentation">
    <div class="update-modal" role="dialog" aria-modal="true" aria-labelledby="update-title" tabindex="-1">
      <h2 id="update-title">Update available</h2>
      <p>
        Installed <span class="mono">{$updateStatus.current_version}</span> does not exactly match
        <span class="mono">{$updateStatus.latest?.tag_name}</span>.
      </p>
      <p class="muted">The release binary will be verified against its published SHA-256 checksum before replacing this executable.</p>
      <p class="warning">Exact matching is enabled. Installing always replaces this binary with the newest published release, which may be older than a dirty or development build and may not contain its unreleased features.</p>
      {#if !$updateStatus.install_supported}<p class="error">No update binary is published for this operating system and architecture.</p>{/if}
      <div class="modal-actions">
        <button type="button" onclick={ignoreUpdate} disabled={updateBusy}>Ignore</button>
        <button type="button" onclick={() => installUpdate('shutdown')} disabled={updateBusy || !$updateStatus.install_supported}>Install & shut down</button>
        <button class="primary" type="button" onclick={() => installUpdate('restart')} disabled={updateBusy || !$updateStatus.install_supported}>
          {updateBusy ? 'Installing…' : 'Install & restart'}
        </button>
      </div>
    </div>
  </div>
{/if}

{#if updateTransition}
  <div class="modal-backdrop" role="presentation">
    <section class="update-modal" role="status">
      <h2>{updateTransition === 'restart' ? 'Restarting with the update…' : 'Update installed'}</h2>
      <p class="muted">{updateTransition === 'restart' ? 'This page will reconnect automatically.' : 'The application has been shut down. You can close this page.'}</p>
    </section>
  </div>
{/if}

<Toaster />

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
    flex-wrap: wrap;
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
  .logout {
    margin-left: auto;
    background: transparent;
    border: 1px solid var(--border);
  }
  .debug-toggle {
    display: inline-flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.35rem 0.55rem;
    border: 1px solid var(--border);
    border-radius: 999px;
    color: var(--muted);
    font-size: 0.9rem;
    user-select: none;
  }
  .debug-toggle input {
    margin: 0;
  }
  .auth-shell {
    min-height: calc(100vh - 6rem);
    display: grid;
    place-items: center;
    padding: 2rem 1rem;
  }
  .auth-card {
    width: min(560px, 100%);
    display: grid;
    gap: 1rem;
    padding: 2rem;
    border: 1px solid var(--border);
    border-radius: 16px;
    background: var(--bg);
    box-shadow: 0 24px 80px rgba(0, 0, 0, 0.12);
  }
  .auth-card h1 {
    margin: 0;
    font-size: 1.6rem;
  }
  .auth-card label {
    display: grid;
    gap: 0.35rem;
    color: var(--muted);
  }
  .auth-card input {
    font: inherit;
    padding: 0.55rem 0.7rem;
    border: 1px solid var(--border);
    border-radius: 8px;
    background: var(--bg);
    color: var(--text);
  }
  .auth-card .actions {
    display: flex;
    gap: 0.5rem;
  }
  .modal-backdrop {
    position: fixed;
    inset: 0;
    z-index: 900;
    display: grid;
    place-items: center;
    padding: 1rem;
    background: rgba(0, 0, 0, 0.7);
  }
  .update-modal {
    width: min(580px, 100%);
    display: grid;
    gap: 1rem;
    padding: 1.5rem;
    border: 1px solid var(--border);
    border-radius: 12px;
    background: var(--surface);
    box-shadow: 0 24px 80px rgba(0, 0, 0, 0.45);
  }
  .update-modal h2, .update-modal p { margin: 0; }
  .modal-actions { display: flex; justify-content: flex-end; gap: 0.6rem; flex-wrap: wrap; }
  .error { color: var(--err); }
  .warning { color: var(--warn); }
</style>
