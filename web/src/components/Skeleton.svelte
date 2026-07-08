<script lang="ts">
  type Props = {
    lines?: number;
    widths?: Array<string | number>;
    height?: string;
    class?: string;
  };

  let {
    lines = 1,
    widths = [],
    height = '1rem',
    class: className = '',
  }: Props = $props();

  function toCssSize(value: string | number | undefined, fallback: string): string {
    if (typeof value === 'number') return `${value}px`;
    if (typeof value === 'string' && value.trim() !== '') return value;
    return fallback;
  }
</script>

<div class={`skeleton-stack ${className}`.trim()} aria-hidden="true">
  {#each Array.from({ length: lines }) as _, i}
    <div
      class="skeleton skeleton-line"
      style={`width: ${toCssSize(widths[i], '100%')}; height: ${height};`}
    ></div>
  {/each}
</div>
