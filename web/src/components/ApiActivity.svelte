<script lang="ts">
  import { activeApiRequests } from '../lib/api-activity';

  let visible = $state(false);
  let busy = $derived($activeApiRequests > 0);

  $effect(() => {
    if (!busy) {
      visible = false;
      return;
    }
    const timer = window.setTimeout(() => {
      visible = true;
    }, 200);
    return () => window.clearTimeout(timer);
  });
</script>

{#if visible}
  <div class="api-activity" role="status" aria-live="polite">
    <span class="spinner" aria-hidden="true"></span>
    <span>Working</span>
  </div>
{/if}

<style>
  .api-activity {
    position: fixed;
    z-index: 1100;
    top: 0.75rem;
    left: 50%;
    display: inline-flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.35rem 0.65rem;
    border: 1px solid color-mix(in srgb, var(--accent) 65%, var(--border));
    border-radius: 999px;
    background: color-mix(in srgb, var(--surface) 92%, transparent);
    box-shadow: 0 8px 28px rgba(0, 0, 0, 0.28);
    color: var(--text);
    font-size: 0.78rem;
    font-weight: 600;
    letter-spacing: 0.04em;
    text-transform: uppercase;
    transform: translateX(-50%);
    backdrop-filter: blur(8px);
    animation: activity-in 140ms ease-out;
  }

  .spinner {
    width: 0.8rem;
    height: 0.8rem;
    border: 2px solid color-mix(in srgb, var(--accent) 30%, transparent);
    border-top-color: var(--accent);
    border-radius: 50%;
    animation: spin 700ms linear infinite;
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }

  @keyframes activity-in {
    from { opacity: 0; transform: translate(-50%, -0.35rem); }
  }

  @media (prefers-reduced-motion: reduce) {
    .api-activity { animation: none; }
    .spinner { animation-duration: 1.4s; }
  }
</style>
