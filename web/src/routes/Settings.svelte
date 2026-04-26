<script lang="ts">
  import { onMount } from 'svelte';
  import { api, type Config, type TestResult } from '../lib/api';

  let cfg = $state<Config | null>(null);
  let err = $state('');
  let msg = $state('');
  let saving = $state(false);
  let sourceTest = $state<TestResult | null>(null);
  let storageTest = $state<TestResult | null>(null);
  let scheduleHuman = $state('');

  async function load() {
    try {
      cfg = (await api.settings()) as Config;
      scheduleHuman = humanize(cfg.backup.schedule);
      err = '';
      msg = '';
    } catch (e) {
      err = String(e);
    }
  }

  async function save() {
    if (!cfg) return;
    saving = true;
    err = '';
    msg = '';
    try {
      cfg = (await api.updateSettings(cfg)) as Config;
      scheduleHuman = humanize(cfg.backup.schedule);
      msg = 'Saved.';
    } catch (e) {
      err = String(e);
    } finally {
      saving = false;
    }
  }

  async function testSource() {
    sourceTest = null;
    try { sourceTest = await api.testSource(); }
    catch (e) { sourceTest = { ok: false, message: String(e) }; }
  }
  async function testStorage() {
    storageTest = null;
    try { storageTest = await api.testStorage(); }
    catch (e) { storageTest = { ok: false, message: String(e) }; }
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
{#if err}<div class="card err">{err}</div>{/if}
{#if msg}<div class="card ok">{msg}</div>{/if}

{#if cfg}
  <!-- SOURCE -->
  <div class="card">
    <h2>Source</h2>
    <label>
      <span>Type</span>
      <select bind:value={cfg.source.type}>
        <option value="localdir">Local directory</option>
        <option value="smb">SMB share</option>
      </select>
    </label>

    {#if cfg.source.type === 'localdir'}
      <label>
        <span>Root path</span>
        <input type="text" bind:value={cfg.source.localdir.root} placeholder="/path/to/files" />
      </label>
    {:else if cfg.source.type === 'smb'}
      <div class="row-2">
        <label>
          <span>Host</span>
          <input type="text" bind:value={cfg.source.smb.host} placeholder="nas.local" />
        </label>
        <label>
          <span>Port</span>
          <input type="number" min="1" max="65535" bind:value={cfg.source.smb.port} />
        </label>
      </div>
      <div class="row-2">
        <label>
          <span>Share</span>
          <input type="text" bind:value={cfg.source.smb.share} />
        </label>
        <label>
          <span>Path</span>
          <input type="text" bind:value={cfg.source.smb.path} placeholder="subdir (optional)" />
        </label>
      </div>
      <div class="row-2">
        <label>
          <span>Username</span>
          <input type="text" bind:value={cfg.source.smb.username} autocomplete="off" />
        </label>
        <label>
          <span>Password</span>
          <input type="password" bind:value={cfg.source.smb.password} autocomplete="new-password" />
        </label>
      </div>
      <label>
        <span>Domain</span>
        <input type="text" bind:value={cfg.source.smb.domain} placeholder="optional" />
      </label>
    {/if}

    <div class="row">
      <button onclick={testSource} type="button">Test source</button>
      {#if sourceTest}
        <span class="badge {sourceTest.ok ? 'ok' : 'err'}">{sourceTest.ok ? 'ok' : 'fail'}</span>
        <span class="muted">{sourceTest.message ?? ''}</span>
      {/if}
    </div>
  </div>

  <!-- S3 -->
  <div class="card">
    <h2>S3 storage</h2>
    <div class="row-2">
      <label>
        <span>Bucket</span>
        <input type="text" bind:value={cfg.s3.bucket} />
      </label>
      <label>
        <span>Region</span>
        <input type="text" bind:value={cfg.s3.region} placeholder="us-east-1" />
      </label>
    </div>
    <div class="row-2">
      <label>
        <span>Endpoint</span>
        <input type="text" bind:value={cfg.s3.endpoint} placeholder="http://localhost:9000" />
        <small class="muted">Leave empty to use real AWS S3. Set for MinIO or other S3-compatible services.</small>
      </label>
      <label class="checkbox">
        <input type="checkbox" bind:checked={cfg.s3.use_path_style} />
        <span>Use path-style addressing</span>
        <small class="muted">Required by MinIO and most S3-compatible services. Disable for real AWS S3.</small>
      </label>
    </div>
    <label>
      <span>Key prefix</span>
      <input type="text" bind:value={cfg.s3.key_prefix} placeholder="backups/" />
    </label>
    <label>
      <span>Storage class</span>
      <select bind:value={cfg.s3.storage_class}>
        <option value="DEEP_ARCHIVE">Glacier Deep Archive (cheapest, 180-day min, slow retrieve)</option>
        <option value="STANDARD">Standard (instant, most expensive)</option>
      </select>
      <small class="muted">DEEP_ARCHIVE / GLACIER / GLACIER_IR are only supported on real AWS S3.</small>
    </label>
    <div class="row-2">
      <label>
        <span>Access key ID</span>
        <input type="text" bind:value={cfg.s3.access_key_id} autocomplete="off" />
        <small class="muted">Leave empty to use the default AWS credential chain (env vars, IAM role, ~/.aws/credentials).</small>
      </label>
      <label>
        <span>Secret access key</span>
        <input type="password" bind:value={cfg.s3.secret_access_key} autocomplete="new-password" />
      </label>
    </div>
    <p class="muted"><code>***</code> in a credential field preserves the stored value on save.</p>

    <div class="row">
      <button onclick={testStorage} type="button">Test storage</button>
      {#if storageTest}
        <span class="badge {storageTest.ok ? 'ok' : 'err'}">{storageTest.ok ? 'ok' : 'fail'}</span>
        <span class="muted">{storageTest.message ?? ''}</span>
      {/if}
    </div>
  </div>

  <!-- BACKUP -->
  <div class="card">
    <h2>Backup behavior</h2>
    <label>
      <span>Temp directory</span>
      <input type="text" bind:value={cfg.backup.tmp_dir} />
    </label>
    <div class="row-2">
      <label>
        <span>Chunk size (individual files per batch)</span>
        <input type="number" min="1" bind:value={cfg.backup.chunk_size} />
      </label>
      <label>
        <span>Zip threshold (files per dir before zipping)</span>
        <input type="number" min="0" bind:value={cfg.backup.zip_threshold} />
      </label>
    </div>
    <div class="row-2">
      <label>
        <span>Min zip dir files</span>
        <input type="number" min="0" bind:value={cfg.backup.min_zip_dir_files} />
      </label>
      <label>
        <span>Zip max bytes (0 = engine default, 2&nbsp;GiB)</span>
        <input type="number" min="0" bind:value={cfg.backup.zip_max_bytes} />
      </label>
    </div>
    <label class="checkbox">
      <input type="checkbox" bind:checked={cfg.backup.enable_zip_index} />
      <span>Upload <code>.index.txt</code> sidecar next to each zip (STANDARD tier; lets you list zip contents without a Glacier restore)</span>
    </label>
    <label class="checkbox">
      <input type="checkbox" bind:checked={cfg.backup.retry_failed} />
      <span>Auto-retry failed files on the next run</span>
    </label>
  </div>

  <!-- SCHEDULE + SERVER -->
  <div class="card">
    <h2>Schedule &amp; server</h2>
    <label>
      <span>Schedule (cron)</span>
      <input type="text" bind:value={cfg.backup.schedule} placeholder="0 2 * * *" />
    </label>
    <p class="muted">{scheduleHuman}</p>

    <div class="row-2">
      <label>
        <span>Bind host</span>
        <input type="text" bind:value={cfg.server.host} placeholder="127.0.0.1" />
      </label>
      <label>
        <span>Bind port</span>
        <input type="number" min="1" max="65535" bind:value={cfg.server.port} />
      </label>
    </div>
  </div>

  <div class="actions">
    <button class="primary" onclick={save} disabled={saving} type="button">
      {saving ? 'Saving…' : 'Save'}
    </button>
    <button onclick={load} disabled={saving} type="button">Reload</button>
  </div>
{/if}

<style>
  h2 { font-size: 1rem; margin: 0 0 0.75rem; }
  .err { color: var(--err); border-color: var(--err); }
  .ok { color: var(--ok); border-color: var(--ok); }
  .row { display: flex; align-items: center; gap: 0.75rem; margin-top: 0.75rem; flex-wrap: wrap; }
  .row-2 {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 0.75rem;
  }
  .actions { display: flex; gap: 0.5rem; margin-top: 0.5rem; }
  label {
    display: grid;
    gap: 0.3rem;
    font-size: 0.85rem;
    color: var(--muted);
    margin-bottom: 0.65rem;
  }
  label.checkbox {
    grid-template-columns: auto 1fr;
    align-items: start;
    gap: 0.5rem;
  }
  label.checkbox input { margin-top: 0.15rem; }
  label input[type="text"],
  label input[type="number"],
  label input[type="password"],
  label select {
    font-family: inherit;
    padding: 0.35rem 0.5rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    background: var(--bg);
    color: var(--text);
  }
  label input[type="number"] { max-width: 12rem; }
  @media (max-width: 640px) {
    .row-2 { grid-template-columns: 1fr; }
  }
</style>
