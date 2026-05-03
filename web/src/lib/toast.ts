import { writable } from 'svelte/store';

export type ToastKind = 'success' | 'error' | 'info';
export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
}

const DURATIONS: Record<ToastKind, number> = {
  success: 4000,
  info: 4000,
  error: 8000,
};

export const toasts = writable<Toast[]>([]);

const timers = new Map<number, ReturnType<typeof setTimeout>>();
let nextId = 1;

export function dismiss(id: number) {
  const t = timers.get(id);
  if (t) {
    clearTimeout(t);
    timers.delete(id);
  }
  toasts.update((list) => list.filter((x) => x.id !== id));
}

function push(kind: ToastKind, message: string) {
  const id = nextId++;
  toasts.update((list) => [...list, { id, kind, message }]);
  timers.set(id, setTimeout(() => dismiss(id), DURATIONS[kind]));
  return id;
}

export const toast = {
  success: (message: string) => push('success', message),
  error: (message: string) => push('error', message),
  info: (message: string) => push('info', message),
};
