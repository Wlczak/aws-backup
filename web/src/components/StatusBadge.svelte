<script lang="ts">
  import { statusBadge, statusLabel } from '../lib/format';

  type Props = { status: string };
  let { status }: Props = $props();
</script>

<span class="badge {statusBadge(status)}">{statusLabel(status)}</span>
