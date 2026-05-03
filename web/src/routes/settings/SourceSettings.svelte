<script lang="ts">
  import { api, type Config, type TestResult } from '../../lib/api';

  type Props = { cfg: Config };
  let { cfg = $bindable() }: Props = $props();

  let sourceTest = $state<TestResult | null>(null);

  async function testSource() {
    sourceTest = null;
    try { sourceTest = await api.testSource(); }
    catch (e) { sourceTest = { ok: false, message: String(e) }; }
  }
</script>

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
