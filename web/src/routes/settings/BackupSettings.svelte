<script lang="ts">
  import { type Config } from '../../lib/api';
  import FolderInput from '../../components/FolderInput.svelte';

  type Props = { cfg: Config };
  let { cfg = $bindable() }: Props = $props();
</script>

<div class="card">
  <h2>Backup behavior</h2>
  <div class="folder-field">
    <label for="backup-tmp-dir">Temp directory</label>
    <FolderInput id="backup-tmp-dir" bind:value={cfg.backup.tmp_dir} ariaLabel="Backup temp directory" />
  </div>
  <div class="folder-field">
    <label for="backup-download-dir">Download directory</label>
    <FolderInput id="backup-download-dir" bind:value={cfg.backup.download_dir} ariaLabel="Backup download directory" />
  </div>
  <div class="row-2">
    <label>
      <span>Chunk size (individual files per batch)</span>
      <input type="number" min="1" bind:value={cfg.backup.chunk_size} />
    </label>
    <label>
      <span>Scan batch limit (bytes, soft cap)</span>
      <input type="number" min="1" bind:value={cfg.backup.scan_batch_bytes} />
    </label>
  </div>
  <p class="muted">
    The scan stops after it crosses this byte budget, but only after finishing the current folder subtree. Uploads run between scan batches.
  </p>
  <div class="row-2">
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
  <p class="muted">
    Small sibling folders below the min file count are folded into the parent zip pool, including at the top level, so object-heavy trees can collapse into a few archives instead of thousands of S3 uploads.
  </p>
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
  <div class="row-2">
    <label>
      <span>Log retention (days, 0 = keep forever)</span>
      <input type="number" min="0" bind:value={cfg.backup.log_retention_days} />
    </label>
    <label>
      <span>Max log lines per run (0 = unlimited)</span>
      <input type="number" min="0" bind:value={cfg.backup.log_max_per_run} />
    </label>
  </div>
  <p class="muted">
    Old runs lose their per-line logs after the retention window; the run row + final
    error message stay. The per-run cap deletes lowest-severity oldest lines first
    (info before warn before error). Trim runs after each backup and once at startup.
  </p>
</div>
