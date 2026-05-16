// Cross-page selection state: the user multi-selects rows on the Files
// pages, then the Restore / Download pages read the saved paths to
// pre-fill their textareas. A tiny Svelte store keeps the data
// accessible without prop-drilling or router plumbing.
import { writable, get } from 'svelte/store';

export interface SelectedFile {
  id: number;
  path: string;
}

export const selection = writable<SelectedFile[]>([]);

export function toggle(f: SelectedFile) {
  selection.update((cur) => {
    const i = cur.findIndex((x) => x.id === f.id);
    if (i >= 0) return cur.filter((_, j) => j !== i);
    return [...cur, f];
  });
}

export function clear() {
  selection.set([]);
}

export function has(id: number): boolean {
  return get(selection).some((f) => f.id === id);
}

export function paths(): string[] {
  return get(selection).map((f) => f.path);
}

export function ids(): number[] {
  return get(selection).map((f) => f.id);
}
