<script lang="ts">
  import { type Config } from '../../lib/api';

  type Props = { cfg: Config; scheduleHuman: string };
  let { cfg = $bindable(), scheduleHuman }: Props = $props();
</script>

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
