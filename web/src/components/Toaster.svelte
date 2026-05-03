<script lang="ts">
  import { toasts, dismiss } from '../lib/toast';
</script>

<div class="toaster" aria-live="polite" aria-atomic="false">
  {#each $toasts as t (t.id)}
    <button class="toast {t.kind}" type="button" onclick={() => dismiss(t.id)}>
      {t.message}
    </button>
  {/each}
</div>

<style>
  .toaster {
    position: fixed;
    bottom: 1rem;
    right: 1rem;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    z-index: 1000;
    pointer-events: none;
    max-width: min(28rem, calc(100vw - 2rem));
  }
  .toast {
    pointer-events: auto;
    text-align: left;
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 0.55rem 0.75rem;
    font-family: inherit;
    font-size: 0.9rem;
    line-height: 1.3;
    cursor: pointer;
    white-space: pre-wrap;
    word-break: break-word;
    box-shadow: 0 4px 12px rgba(0, 0, 0, 0.35);
  }
  .toast.success { color: var(--ok); border-color: var(--ok); }
  .toast.error { color: var(--err); border-color: var(--err); }
  .toast.info { color: var(--accent); border-color: var(--accent); }
  .toast:hover { filter: brightness(1.1); }
</style>
