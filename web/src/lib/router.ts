// Tiny hash router — no dependencies. Backed by window.location.hash so
// refreshes work without server-side SPA fallback.
import { writable, type Writable } from 'svelte/store';

function read(): string {
  const h = window.location.hash.replace(/^#\/?/, '');
  return h || 'dashboard';
}

export const route: Writable<string> = writable(read());

window.addEventListener('hashchange', () => {
  route.set(read());
});

export function go(to: string) {
  window.location.hash = '#/' + to;
}
