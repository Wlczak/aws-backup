<script lang="ts">
  import { type Config } from '../../lib/api';

  type Props = { cfg: Config };
  let { cfg = $bindable() }: Props = $props();
</script>

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
  <div class="row-2">
    <label>
      <span>Copy threads (source → tmp parallelism)</span>
      <input type="number" min="1" max="64" bind:value={cfg.backup.copy_threads} />
    </label>
    <label>
      <span>Upload threads (tmp → S3 parallelism)</span>
      <input type="number" min="1" max="64" bind:value={cfg.backup.upload_threads} />
    </label>
  </div>
  <label>
    <span>Pipeline queue depth (staged groups awaiting upload, 0 = auto)</span>
    <input type="number" min="0" bind:value={cfg.backup.pipeline_queue} />
  </label>
  <p class="muted">Peak tmp disk usage scales with <code>pipeline_queue + upload_threads</code>.</p>
  <label class="checkbox">
    <input type="checkbox" bind:checked={cfg.backup.enable_zip_index} />
    <span>Upload <code>.index.txt</code> sidecar next to each zip (STANDARD tier; lets you list zip contents without a Glacier restore)</span>
  </label>
  <label class="checkbox">
    <input type="checkbox" bind:checked={cfg.backup.retry_failed} />
    <span>Auto-retry failed files on the next run</span>
  </label>
</div>
