// Typed wrappers around the aws-backup HTTP API. Kept free of UI concerns
// so pages + components can import without pulling in Svelte.

export type RunStatus = 'running' | 'completed' | 'failed' | 'cancelled';
export type FileStatus = 'pending' | 'zipped' | 'uploaded' | 'failed' | 'missing';
export type RestoreStatus = '' | 'in_progress' | 'restored';

export interface Run {
  id: number;
  started_at: string;
  finished_at?: string;
  status: RunStatus;
  files_scanned: number;
  files_uploaded: number;
  bytes_uploaded: number;
  error_message?: string;
}

export interface LogEntry {
  id: number;
  timestamp: string;
  level: 'info' | 'warn' | 'error';
  message: string;
}

export interface FileRow {
  id: number;
  path: string;
  size: number;
  mtime: string;
  md5?: string;
  status: FileStatus;
  zip_name?: string;
  s3_key?: string;
  uploaded_at?: string;
  last_seen_at: string;
  restore_status?: RestoreStatus;
  restore_expires_at?: string;
}

export interface FilesPage {
  files: FileRow[];
  total: number;
  page: number;
  limit: number;
}

export interface RunsPage {
  runs: Run[];
  total: number;
  page: number;
  limit: number;
}

export interface RunDetail {
  run: Run;
  logs: LogEntry[];
}

export interface FileStats {
  by_status: Record<string, number>;
  by_restore_status: Record<string, number>;
  restore_soonest_expires_at?: string;
  total_count: number;
  total_size: number;
}

export interface Status {
  current?: Run;
  last?: Run;
  stop_requested?: boolean;
}

export interface RestoreEstimate {
  file_count: number;
  total_bytes: number;
  request_fee_usd: number;
  retrieval_fee_usd: number;
  egress_fee_usd: number;
  total_fee_usd: number;
  wait_hours_min: number;
  wait_hours_max: number;
  unknown_paths?: string[];
}

export interface TestResult {
  ok: boolean;
  message?: string;
}

// Mirror of the Go `config.Config` tree exposed by /api/settings.
// Credential-like fields are returned as "***" by the server and, if
// sent back unchanged, are preserved by its mergeSecrets step.
export interface SourceLocalDirConfig { root: string }
export interface SourceSMBConfig {
  host: string; port: number;
  username: string; password: string;
  domain: string; share: string; path: string;
}
export interface SourceConfig {
  type: 'localdir' | 'smb' | '';
  localdir: SourceLocalDirConfig;
  smb: SourceSMBConfig;
}
export interface S3Config {
  endpoint: string;
  use_path_style: boolean;
  bucket: string;
  region: string;
  access_key_id: string;
  secret_access_key: string;
  storage_class: 'DEEP_ARCHIVE' | 'STANDARD' | '';
  key_prefix: string;
  multipart_threshold: number;
  resume_threshold: number;
  part_size: number;
}
export interface BackupConfig {
  chunk_size: number;
  tmp_dir: string;
  schedule: string;
  zip_threshold: number;
  min_zip_dir_files: number;
  zip_max_bytes: number;
  enable_zip_index: boolean;
  retry_failed: boolean;
  copy_threads: number;
  upload_threads: number;
  pipeline_queue: number;
}
export interface ServerConfig { host: string; port: number }
export interface SQSConfig {
  queue_url: string;
  region: string;
  wait_time_seconds: number;
  visibility_timeout: number;
  max_messages: number;
}
export interface Config {
  source: SourceConfig;
  s3: S3Config;
  sqs: SQSConfig;
  backup: BackupConfig;
  server: ServerConfig;
}

// SettingsResponse mirrors the wire shape of GET/PUT /api/settings: a
// full Config plus a `pending_apply` flag that's true when the saved
// config is queued behind an in-flight backup run and will be applied
// once the run finishes.
export type SettingsResponse = Config & { pending_apply: boolean };

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      ...(init?.headers ?? {}),
    },
  });
  if (!resp.ok) {
    let msg = resp.statusText;
    try {
      const body = await resp.json();
      if (body?.error) msg = body.error;
    } catch { /* ignore non-JSON error bodies */ }
    throw new Error(`${resp.status}: ${msg}`);
  }
  if (resp.status === 204) return undefined as T;
  return (await resp.json()) as T;
}

export const api = {
  status: () => request<Status>('/api/status'),
  runs: (page = 1, limit = 20) => request<RunsPage>(`/api/runs?page=${page}&limit=${limit}`),
  run: (id: number) => request<RunDetail>(`/api/runs/${id}`),
  triggerRun: (opts?: { mode?: 'full' | 'scan' | 'upload'; paths?: string[] }) =>
    request<{ run_id: number }>('/api/runs', {
      method: 'POST',
      body: opts ? JSON.stringify(opts) : undefined,
    }),
  cancelRun: (id: number) => request<{ status: string }>(`/api/runs/${id}/cancel`, { method: 'POST' }),
  stopRun: (id: number) => request<{ status: string }>(`/api/runs/${id}/stop`, { method: 'POST' }),
  continueRun: (id: number) => request<{ status: string }>(`/api/runs/${id}/continue`, { method: 'POST' }),

  files: (opts: { page?: number; limit?: number; status?: string; search?: string; all?: boolean } = {}) => {
    const qs = new URLSearchParams();
    if (opts.page) qs.set('page', String(opts.page));
    if (opts.limit) qs.set('limit', String(opts.limit));
    if (opts.status) qs.set('status', opts.status);
    if (opts.search) qs.set('search', opts.search);
    if (opts.all) qs.set('all', 'true');
    return request<FilesPage>(`/api/files?${qs.toString()}`);
  },
  fileStats: () => request<FileStats>('/api/files/stats'),
  retryFile: (id: number) =>
    request<{ affected: number }>(`/api/files/${id}/retry`, { method: 'POST' }),
  retryFiles: (ids: number[]) =>
    request<{ affected: number }>(`/api/files/retry`, {
      method: 'POST',
      body: JSON.stringify({ ids }),
    }),
  retryAllFailed: () =>
    request<{ affected: number }>(`/api/files/retry`, {
      method: 'POST',
      body: JSON.stringify({ all_failed: true }),
    }),
  retryByPaths: (paths: string[]) =>
    request<{ affected: number }>(`/api/files/retry`, {
      method: 'POST',
      body: JSON.stringify({ paths }),
    }),
  deleteFile: (id: number) =>
    request<{ affected: number }>(`/api/files/${id}`, { method: 'DELETE' }),
  deleteFiles: (ids: number[]) =>
    request<{ affected: number }>(`/api/files`, {
      method: 'DELETE',
      body: JSON.stringify({ ids }),
    }),

  settings: () => request<SettingsResponse>('/api/settings'),
  updateSettings: (cfg: Config) =>
    request<SettingsResponse>('/api/settings', { method: 'PUT', body: JSON.stringify(cfg) }),

  testSource: () => request<TestResult>('/api/smb/test'),
  testStorage: () => request<TestResult>('/api/s3/test'),

  sync: () =>
    request<{
      zip_names_in_db: number;
      individual_keys_in_db: number;
      keys_in_s3: number;
      missing_zips: number;
      missing_individual: number;
      files_reset: number;
    }>('/api/sync', { method: 'POST' }),

  syncFull: () =>
    request<{
      zip_names_in_db: number;
      individual_keys_in_db: number;
      keys_in_s3: number;
      missing_zips: number;
      missing_individual: number;
      files_reset: number;
      cloud_file_count: number;
      local_file_count: number;
      zip_indexes_consumed: number;
      local_missing_count: number;
      local_missing_from_cloud?: string[];
      cloud_missing_count: number;
      cloud_missing_from_local?: string[];
    }>('/api/sync/full', { method: 'POST' }),

  deleteCloudPaths: (paths: string[]) =>
    request<{
      deleted_standalone: number;
      deleted_zips: number;
      skipped_partial_zip: number;
      errors?: string[];
    }>('/api/sync/delete-cloud-paths', {
      method: 'POST',
      body: JSON.stringify({ paths }),
    }),

  restoreEstimate: (paths: string[]) =>
    request<RestoreEstimate>('/api/restore/estimate', {
      method: 'POST',
      body: JSON.stringify({ paths }),
    }),
  restoreTrigger: (paths: string[], targetDir: string) =>
    request<{
      files_written: number;
      bytes_written: number;
      skipped?: string[];
      errors?: string[];
    }>('/api/restore/trigger', {
      method: 'POST',
      body: JSON.stringify({ paths, target_dir: targetDir }),
    }),
  restoreSyncStatus: () =>
    request<{ processed: number }>('/api/restore/sync-status', {
      method: 'POST',
    }),

  restoreScanFull: () =>
    request<RestoreScanResult>('/api/restore/scan/full', { method: 'POST' }),
  restoreScanPending: () =>
    request<RestoreScanResult>('/api/restore/scan/pending', { method: 'POST' }),

  inventoryGet: () => request<InventoryStatus>('/api/restore/inventory'),
  inventoryPut: (frequency: 'daily' | 'weekly') =>
    request<InventoryStatus>('/api/restore/inventory', {
      method: 'PUT',
      body: JSON.stringify({ frequency }),
    }),
  inventoryDelete: () =>
    request<{ enabled: false }>('/api/restore/inventory', {
      method: 'DELETE',
    }),
  inventorySync: () =>
    request<RestoreScanResult>('/api/restore/inventory/sync', {
      method: 'POST',
    }),
};

export interface RestoreScanResult {
  mode: 'full' | 'pending' | 'inventory';
  scanned: number;
  updated: number;
  errors: number;
  duration_ns: number;
}

export interface InventoryStatus {
  enabled: boolean;
  id?: string;
  frequency?: 'Daily' | 'Weekly';
  destination?: string;
  format?: string;
}

// subscribeEvents opens a live EventSource against /api/events. Returns
// an AbortController-style { close } — callers should invoke it on
// component teardown to unsubscribe. The optional onStatus callback fires
// with 'open' once the stream is connected and 'error' on disconnect, so
// the UI can surface a "live updates disconnected" banner instead of
// freezing silently when the connection drops. The browser auto-reconnects
// on transient network errors; onStatus('open') will fire again once it's back.
export function subscribeEvents(
  onEvent: (type: string, data: unknown) => void,
  onStatus?: (status: 'open' | 'error') => void,
): { close: () => void } {
  const es = new EventSource('/api/events');
  const handler = (ev: MessageEvent) => {
    try {
      onEvent(ev.type, JSON.parse(ev.data));
    } catch {
      onEvent(ev.type, ev.data);
    }
  };
  const types = [
    'scan_progress', 'scan_complete',
    'upload_plan',
    'copy_progress',
    'upload_start', 'upload_progress', 'upload_complete', 'upload_failed',
    'run_start', 'run_log', 'run_complete',
    'db_sync_start', 'db_sync_progress', 'db_sync_complete', 'db_sync_failed',
    'restore_scan_start', 'restore_scan_progress', 'restore_scan_complete', 'restore_scan_failed',
  ];
  for (const t of types) es.addEventListener(t, handler as EventListener);
  // Fallback for default-typed (`event: message`) frames: forward them so
  // new server event types aren't silently dropped while the typed list
  // catches up. In dev, surface them so missing entries are easy to spot. (#201)
  es.onmessage = (ev) => {
    if ((import.meta as any).env?.DEV) {
      console.warn('[subscribeEvents] untyped SSE message — add to types list:', ev.data);
    }
    handler(ev);
  };
  if (onStatus) {
    es.addEventListener('open', () => onStatus('open'));
    es.addEventListener('error', () => onStatus('error'));
  }
  return { close: () => es.close() };
}
