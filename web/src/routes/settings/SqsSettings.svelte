<script lang="ts">
  import { type Config } from '../../lib/api';

  type Props = { cfg: Config };
  let { cfg = $bindable() }: Props = $props();
</script>

<div class="card">
  <h2>SQS restore notifications</h2>
  <p class="muted">
    Polls an SQS queue subscribed to S3 Glacier <code>ObjectRestore:Completed</code>
    events so the Restore page can mark files restored without manual sync.
    Leave the queue URL empty to disable polling. Credentials are reused from
    the S3 settings.
  </p>
  <label>
    <span>Queue URL</span>
    <input
      type="text"
      bind:value={cfg.sqs.queue_url}
      placeholder="https://sqs.us-east-1.amazonaws.com/123456789012/my-queue"
    />
  </label>
  <label>
    <span>Region (falls back to S3 region when empty)</span>
    <input type="text" bind:value={cfg.sqs.region} placeholder="us-east-1" />
  </label>
  <div class="row-2">
    <label>
      <span>Wait time (seconds, 0–20; long-poll)</span>
      <input type="number" min="0" max="20" bind:value={cfg.sqs.wait_time_seconds} />
    </label>
    <label>
      <span>Max messages per poll (1–10)</span>
      <input type="number" min="1" max="10" bind:value={cfg.sqs.max_messages} />
    </label>
  </div>
  <label>
    <span>Visibility timeout (seconds; 0 = queue default)</span>
    <input type="number" min="0" bind:value={cfg.sqs.visibility_timeout} />
  </label>
</div>
