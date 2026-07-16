import { writable } from 'svelte/store';

export const activeApiRequests = writable(0);

export function beginApiRequest() {
  activeApiRequests.update((count) => count + 1);
  let finished = false;
  return () => {
    if (finished) return;
    finished = true;
    activeApiRequests.update((count) => Math.max(0, count - 1));
  };
}
