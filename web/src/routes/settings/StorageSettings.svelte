<script lang="ts">
  import { api, type Config, type TestResult } from '../../lib/api';

  type Props = { cfg: Config };
  let { cfg = $bindable() }: Props = $props();

  let storageTest = $state<TestResult | null>(null);

  // Multipart threshold UI state — the wire format is bytes, but the
  // user picks a value + unit so they don't have to compute "16 MiB =
  // 16777216" by hand. The unit defaults to the largest one that
  // divides the stored byte value cleanly, falling back to MiB for 0.
  type ByteUnit = 'B' | 'KiB' | 'MiB' | 'GiB';
  const byteUnitMul: Record<ByteUnit, number> = {
    B: 1,
    KiB: 1024,
    MiB: 1024 * 1024,
    GiB: 1024 * 1024 * 1024,
  };
  let multipartValue = $state(0);
  let multipartUnit = $state<ByteUnit>('MiB');

  function decodeBytes(b: number): { value: number; unit: ByteUnit } {
    if (!b) return { value: 0, unit: 'MiB' };
    const order: ByteUnit[] = ['GiB', 'MiB', 'KiB', 'B'];
    for (const u of order) {
      if (b % byteUnitMul[u] === 0) return { value: b / byteUnitMul[u], unit: u };
    }
    return { value: b, unit: 'B' };
  }

  // Re-decode whenever the parent swaps cfg (load / save round-trip).
  let lastSeenBytes = $state(NaN);
  $effect(() => {
    const b = cfg.s3.multipart_threshold;
    if (b !== lastSeenBytes) {
      const { value, unit } = decodeBytes(b);
      multipartValue = value;
      multipartUnit = unit;
      lastSeenBytes = b;
    }
  });

  let multipartMax = $derived(Math.floor((5 * byteUnitMul.GiB) / byteUnitMul[multipartUnit]));

  $effect(() => {
    const bytes = multipartValue * byteUnitMul[multipartUnit];
    cfg.s3.multipart_threshold = bytes;
    lastSeenBytes = bytes;
  });

  async function testStorage() {
    storageTest = null;
    try { storageTest = await api.testStorage(); }
    catch (e) { storageTest = { ok: false, message: String(e) }; }
  }
</script>

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
  <label>
    <span>Multipart threshold (0 = default 5&nbsp;GiB; lower for parallel parts)</span>
    <div class="row-2">
      <input type="number" min="0" max={multipartMax} bind:value={multipartValue} />
      <select bind:value={multipartUnit}>
        <option value="B">B</option>
        <option value="KiB">KiB</option>
        <option value="MiB">MiB</option>
        <option value="GiB">GiB</option>
      </select>
    </div>
    <small class="muted">Bodies at or above this size route through the multipart uploader. Lowering it earns parallel-part throughput and finer-grained retry on medium-sized objects.</small>
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
