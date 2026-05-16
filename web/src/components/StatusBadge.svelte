<script lang="ts">
  import { statusBadge, statusLabel } from '../lib/format';

  type Props = { status: string; s3Key?: string };
  let { status, s3Key }: Props = $props();
</script>

<span class="badge {statusBadge(status)}">{statusLabel(status, s3Key)}</span>
