import { writable } from 'svelte/store';

export type ClientLogLevel = 'debug' | 'info' | 'warn' | 'error';

export interface ClientLogContext {
  [key: string]: unknown;
}

export interface ClientLogEntry {
  timestamp: string;
  level: ClientLogLevel;
  source: string;
  message: string;
  route?: string;
  url?: string;
  stack?: string;
  session_id?: string;
  context?: ClientLogContext;
}

export interface InstallClientLoggerOptions {
  onError?: (message: string) => void;
}

const DEBUG_KEY = 'aws-backup.client-logs.debug';
const QUEUE_KEY = 'aws-backup.client-logs.queue';
const SESSION_KEY = 'aws-backup.client-logs.session';
const BATCH_SIZE = 25;
const FLUSH_INTERVAL_MS = 5000;
const MAX_QUEUE = 200;
const DUP_WINDOW_MS = 250;

function readBool(key: string, fallback = false): boolean {
  try {
    return localStorage.getItem(key) === '1';
  } catch {
    return fallback;
  }
}

function readQueue(): ClientLogEntry[] {
  try {
    const raw = localStorage.getItem(QUEUE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? parsed.filter(Boolean) : [];
  } catch {
    return [];
  }
}

function persistQueue(queue: ClientLogEntry[]) {
  try {
    if (queue.length === 0) localStorage.removeItem(QUEUE_KEY);
    else localStorage.setItem(QUEUE_KEY, JSON.stringify(queue));
  } catch {
    // Best-effort only. If localStorage is unavailable or full, we keep the
    // in-memory queue and try again on the next flush.
  }
}

function ensureSessionID(): string {
  try {
    const existing = sessionStorage.getItem(SESSION_KEY);
    if (existing) return existing;
    const id = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(16).slice(2)}`;
    sessionStorage.setItem(SESSION_KEY, id);
    return id;
  } catch {
    return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(16).slice(2)}`;
  }
}

function readRoute(): string {
  if (typeof window === 'undefined') return '';
  return window.location.hash.replace(/^#\/?/, '') || 'dashboard';
}

function toText(v: unknown): string {
  if (v instanceof Error) {
    return `${v.name}: ${v.message}`;
  }
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean' || v == null) return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return Object.prototype.toString.call(v);
  }
}

function formatArgs(args: unknown[]): string {
  return args
    .map((arg) => toText(arg))
    .join(' ')
    .trim()
    .slice(0, 2000);
}

function normalizeErrorMessage(message: string): string {
  return message.trim().replace(/\s+/g, ' ').slice(0, 500);
}

const initialDebug = readBool(DEBUG_KEY, false);
export const clientLogDebug = writable<boolean>(initialDebug);

let debugEnabled = initialDebug;
let queue = readQueue();
let inflight = false;
let installed = false;
let flushTimer: ReturnType<typeof setInterval> | null = null;
let lastSignature = '';
let lastSignatureAt = 0;
let teardownFns: Array<() => void> = [];

clientLogDebug.subscribe((enabled) => {
  debugEnabled = enabled;
  try {
    localStorage.setItem(DEBUG_KEY, enabled ? '1' : '0');
  } catch {
    // ignore
  }
  void flushClientLogs();
});

export function setClientLogDebug(enabled: boolean) {
  clientLogDebug.set(enabled);
}

function shouldRecord(level: ClientLogLevel): boolean {
  return level === 'warn' || level === 'error' || debugEnabled;
}

function dedupeSignature(entry: ClientLogEntry): string {
  return [entry.level, entry.route ?? '', entry.url ?? '', entry.message].join('|');
}

function enqueue(entry: ClientLogEntry) {
  if (!shouldRecord(entry.level)) return;
  const sig = dedupeSignature(entry);
  const now = Date.now();
  if (sig === lastSignature && now - lastSignatureAt < DUP_WINDOW_MS) {
    return;
  }
  lastSignature = sig;
  lastSignatureAt = now;

  queue.push(entry);
  if (queue.length > MAX_QUEUE) {
    queue = queue.slice(-MAX_QUEUE);
  }
  persistQueue(queue);
  void flushClientLogs();
}

export function recordClientLog(
  level: ClientLogLevel,
  source: string,
  message: string,
  context?: ClientLogContext,
): void {
  enqueue({
    timestamp: new Date().toISOString(),
    level,
    source,
    message: normalizeErrorMessage(message),
    route: readRoute(),
    url: typeof window !== 'undefined' ? window.location.href : undefined,
    session_id: ensureSessionID(),
    context: context && Object.keys(context).length > 0 ? context : undefined,
  });
}

function serializeError(err: unknown): { message: string; stack?: string; context?: ClientLogContext } {
  if (err instanceof Error) {
    return {
      message: `${err.name}: ${err.message}`,
      stack: err.stack,
      context: err.cause ? { cause: toText(err.cause) } : undefined,
    };
  }
  return { message: toText(err) };
}

function installConsoleCapture() {
  const methods: Array<ClientLogLevel & ('debug' | 'info' | 'warn' | 'error')> = ['debug', 'info', 'warn', 'error'];
  const originals = new Map<string, (...args: unknown[]) => void>();
  const consoleObj = console as typeof console & Record<'debug' | 'info' | 'warn' | 'error', (...args: unknown[]) => void>;
  for (const method of methods) {
    const orig = consoleObj[method];
    if (typeof orig !== 'function') continue;
    originals.set(method, orig.bind(console));
    consoleObj[method] = ((...args: unknown[]) => {
      originals.get(method)?.(...args);
      const level: ClientLogLevel = method === 'debug' ? 'debug' : method === 'info' ? 'info' : method === 'warn' ? 'warn' : 'error';
      enqueue({
        timestamp: new Date().toISOString(),
        level,
        source: 'console',
        message: formatArgs(args),
        route: readRoute(),
        url: typeof window !== 'undefined' ? window.location.href : undefined,
        session_id: ensureSessionID(),
        context: debugEnabled ? { args: args.map((arg) => toText(arg)) } : undefined,
      });
    }) as (...args: unknown[]) => void;
  }
  teardownFns.push(() => {
    for (const [method, orig] of originals) {
      consoleObj[method as 'debug' | 'info' | 'warn' | 'error'] = orig;
    }
  });
}

async function postBatch(entries: ClientLogEntry[]): Promise<boolean> {
  if (entries.length === 0) return true;
  if (typeof fetch !== 'function') return false;
  try {
    const resp = await fetch('/api/client-logs', {
      method: 'POST',
      credentials: 'same-origin',
      keepalive: true,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ entries }),
    });
    return resp.ok;
  } catch {
    return false;
  }
}

export async function flushClientLogs(): Promise<void> {
  if (inflight || queue.length === 0) return;
  inflight = true;
  try {
    while (queue.length > 0) {
      const batch = queue.slice(0, BATCH_SIZE);
      const ok = await postBatch(batch);
      if (!ok) return;
      queue = queue.slice(batch.length);
      persistQueue(queue);
    }
  } finally {
    inflight = false;
  }
}

function handleWindowError(ev: ErrorEvent) {
  const err = ev.error;
  const parsed = serializeError(err);
  const message = ev.message || parsed.message || 'Unhandled frontend error';
  enqueue({
    timestamp: new Date().toISOString(),
    level: 'error',
    source: 'window.error',
    message: normalizeErrorMessage(message),
    route: readRoute(),
    url: typeof window !== 'undefined' ? window.location.href : undefined,
    stack: parsed.stack ?? ev.error?.stack ?? `${ev.filename}:${ev.lineno}:${ev.colno}`,
    session_id: ensureSessionID(),
    context: {
      filename: ev.filename,
      lineno: ev.lineno,
      colno: ev.colno,
    },
  });
  return message;
}

function handleUnhandledRejection(ev: PromiseRejectionEvent) {
  const parsed = serializeError(ev.reason);
  enqueue({
    timestamp: new Date().toISOString(),
    level: 'error',
    source: 'unhandledrejection',
    message: normalizeErrorMessage(parsed.message || 'Unhandled promise rejection'),
    route: readRoute(),
    url: typeof window !== 'undefined' ? window.location.href : undefined,
    stack: parsed.stack,
    session_id: ensureSessionID(),
    context: parsed.context,
  });
}

function handlePageHide() {
  void flushClientLogs();
}

export function installClientLogging(handlers?: InstallClientLoggerOptions): () => void {
  if (installed || typeof window === 'undefined') {
    return () => {};
  }
  installed = true;

  const onError = (ev: ErrorEvent) => {
    const message = handleWindowError(ev);
    handlers?.onError?.(message);
  };
  const onUnhandledRejection = (ev: PromiseRejectionEvent) => {
    handleUnhandledRejection(ev);
    handlers?.onError?.('Unhandled promise rejection');
  };

  window.addEventListener('error', onError);
  window.addEventListener('unhandledrejection', onUnhandledRejection);
  window.addEventListener('pagehide', handlePageHide);
  window.addEventListener('online', flushClientLogs);
  const onVisibilityChange = () => {
    if (document.visibilityState === 'hidden') {
      void flushClientLogs();
    }
  };
  document.addEventListener('visibilitychange', onVisibilityChange);

  installConsoleCapture();
  void flushClientLogs();
  flushTimer = setInterval(() => {
    void flushClientLogs();
  }, FLUSH_INTERVAL_MS);

  return () => {
    window.removeEventListener('error', onError);
    window.removeEventListener('unhandledrejection', onUnhandledRejection);
    window.removeEventListener('pagehide', handlePageHide);
    window.removeEventListener('online', flushClientLogs);
    document.removeEventListener('visibilitychange', onVisibilityChange);
    if (flushTimer) {
      clearInterval(flushTimer);
      flushTimer = null;
    }
    for (const fn of teardownFns.splice(0)) fn();
    installed = false;
  };
}

export function clientLogInfo(source: string, message: string, context?: ClientLogContext) {
  recordClientLog('info', source, message, context);
}

export function clientLogWarn(source: string, message: string, context?: ClientLogContext) {
  recordClientLog('warn', source, message, context);
}

export function clientLogError(source: string, message: string, context?: ClientLogContext) {
  recordClientLog('error', source, message, context);
}
