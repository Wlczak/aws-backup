<script lang="ts">
  type Props = { value: number; max: number; label?: string };
  let { value, max, label }: Props = $props();

  let pct = $derived(max > 0 ? Math.min(100, Math.round((value / max) * 100)) : 0);
</script>

<div class="wrap">
  {#if label}<div class="label">{label}</div>{/if}
  <div class="bar"><div class="fill" style="width: {pct}%"></div></div>
  <div class="numbers">{value.toLocaleString()} / {max.toLocaleString()} ({pct}%)</div>
</div>

<style>
  .wrap { display: grid; gap: 0.25rem; }
  .label { font-size: 0.85rem; color: var(--muted); }
  .bar {
    height: 8px;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 4px;
    overflow: hidden;
  }
  .fill {
    height: 100%;
    background: var(--accent);
    transition: width 0.3s ease;
  }
  .numbers {
    font-size: 0.8rem;
    color: var(--muted);
    font-family: ui-monospace, monospace;
  }
</style>
