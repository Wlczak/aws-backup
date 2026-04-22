<script lang="ts">
  import { statusBadge } from '../lib/format';

  type Props = { status: string };
  let { status }: Props = $props();
</script>

<span class="badge {statusBadge(status)}">{status}</span>
