// Typed wrappers around the aws-backup HTTP API. Kept free of UI concerns
// so pages + components can import without pulling in Svelte.

export type RunStatus = 'running' | 'completed' | 'failed' | 'cancelled';
export type FileStatus = 'pending' | 'zipped' | 'uploaded' | 'failed' | 'missing';

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
  total_count: number;
  total_size: number;
}

export interface Status {
  current?: Run;
  last?: Run;
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
}
export interface ServerConfig { host: string; port: number }
export interface Config {
  source: SourceConfig;
  s3: S3Config;
  backup: BackupConfig;
  server: ServerConfig;
}

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
  deleteFile: (id: number) =>
    request<{ affected: number }>(`/api/files/${id}`, { method: 'DELETE' }),
  deleteFiles: (ids: number[]) =>
    request<{ affected: number }>(`/api/files`, {
      method: 'DELETE',
      body: JSON.stringify({ ids }),
    }),

  settings: () => request<Config>('/api/settings'),
  updateSettings: (cfg: Config) =>
    request<Config>('/api/settings', { method: 'PUT', body: JSON.stringify(cfg) }),

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

  restoreEstimate: (paths: string[]) =>
    request<RestoreEstimate>('/api/restore/estimate', {
      method: 'POST',
      body: JSON.stringify({ paths }),
    }),
  restoreTrigger: (paths: string[]) =>
    request<{ error?: string }>('/api/restore/trigger', {
      method: 'POST',
      body: JSON.stringify({ paths }),
    }),
};

// subscribeEvents opens a live EventSource against /api/events. Returns
// an AbortController-style { close } — callers should invoke it on
// component teardown to unsubscribe.
export function subscribeEvents(onEvent: (type: string, data: unknown) => void): { close: () => void } {
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
    'upload_start', 'upload_complete', 'upload_failed',
    'run_start', 'run_complete',
  ];
  for (const t of types) es.addEventListener(t, handler as EventListener);
  return { close: () => es.close() };
}
