import { writable } from 'svelte/store';
import type { UpdateStatus } from './api';

export const updateStatus = writable<UpdateStatus | null>(null);
