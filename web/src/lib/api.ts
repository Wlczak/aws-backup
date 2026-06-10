// Typed wrappers around the aws-backup HTTP API. Kept free of UI concerns
// so pages + components can import without pulling in Svelte.
import { recordClientLog } from './client-logs';

export type RunStatus = 'running' | 'completed' | 'failed' | 'cancelled';
export type FileStatus = 'pending' | 'zipped' | 'uploaded' | 'failed' | 'cloud_only' | 'missing';
export type RestoreStatus = '' | 'in_progress' | 'restored';
export type RestoreTier = 'bulk' | 'standard';

type EmptyPayload = Record<string, never>;

export interface ScanProgressPayload {
  seen: number;
  bytes: number;
  new: number;
  changed: number;
}

export interface ScanCompletePayload {
  seen: number;
  bytes: number;
  new: number;
  changed: number;
  unchanged: number;
  missing: number;
  paused?: boolean;
}

export interface UploadPlanPayload {
  total_files: number;
  total_groups: number;
  total_bytes: number;
}

export interface CopyProgressPayload {
  key: string;
  size: number;
  bytes_copied: number;
  percent: number;
}

export interface UploadStartPayload {
  key: string;
  size: number;
  files?: number;
}

export interface UploadProgressPayload {
  key: string;
  bytes_uploaded: number;
  size: number;
  percent: number;
}

export interface UploadCompletePayload {
  key: string;
  size: number;
  etag?: string;
  checksum_sha256?: string;
  files?: number;
  skipped?: boolean;
}

export interface UploadFailedPayload {
  key: string;
  error: string;
}

export interface RunStartPayload {
  files_scanned: number;
  bytes_scanned: number;
  files_uploaded: number;
  bytes_uploaded: number;
}

export interface RunLogPayload {
  level?: 'info' | 'warn' | 'error';
  message?: string;
}

export interface RunCompletePayload {
  status: string;
  files_scanned: number;
  bytes_scanned: number;
  files_uploaded: number;
  bytes_uploaded: number;
  error_message?: string;
}

export interface DBSyncStartPayload {
  reason?: 'complete' | 'stop' | 'cancel';
  size?: number;
}

export interface DBSyncProgressPayload {
  reason?: 'complete' | 'stop' | 'cancel';
  bytes?: number;
  total?: number;
  percent?: number;
}

export interface DBSyncCompletePayload {
  reason?: 'complete' | 'stop' | 'cancel';
  size?: number;
}

export interface DBSyncFailedPayload {
  reason?: 'complete' | 'stop' | 'cancel';
  error?: string;
}

export interface RestoreManifestStartPayload {
  stage: string;
  processed: number;
  total: number;
  manifest_key?: string;
}

export interface RestoreManifestProgressPayload {
  stage: string;
  processed: number;
  total: number;
  manifest_key?: string;
  data_key?: string;
  keys?: number;
}

export interface RestoreManifestCompletePayload {
  stage: string;
  processed: number;
  total: number;
  manifest_key?: string;
  keys?: number;
}

export interface RestoreManifestFailedPayload {
  stage: string;
  error: string;
  manifest_key?: string;
  data_key?: string;
}

export interface RestoreScanStartPayload {
  mode: string;
  total: number;
}

export interface RestoreScanProgressPayload {
  mode: string;
  scanned: number;
  updated: number;
  errors: number;
  total: number;
}

export interface RestoreScanCompletePayload {
  mode: string;
  scanned: number;
  updated: number;
  errors: number;
}

export interface RestoreScanFailedPayload {
  mode: string;
  error: string;
}

export interface RestoreRequestStartPayload {
  total: number;
}

export interface RestoreRequestProgressPayload {
  processed: number;
  total: number;
  keys_requested?: number;
  keys_already_thawed?: number;
  errors?: number;
}

export interface RestoreRequestCompletePayload {
  total: number;
  keys_requested: number;
  keys_already_in_progress: number;
  keys_already_available: number;
  files_affected: number;
  bytes_affected: number;
  errors?: number;
}

export interface RestoreRequestFailedPayload {
  error: string;
  processed?: number;
  total?: number;
}

export interface RestoreDownloadStartPayload {
  total: number;
  total_bytes: number;
}

export interface RestoreDownloadProgressPayload {
  path: string;
  processed: number;
  total: number;
  total_bytes: number;
  files_written: number;
  bytes_written: number;
  errors: number;
  current_path?: string;
  current_bytes?: number;
  current_total_bytes?: number;
  current_percent?: number;
  file_status?: 'active' | 'done' | 'failed';
  error?: string;
}

export interface RestoreDownloadCompletePayload {
  files_written: number;
  bytes_written: number;
  total_bytes: number;
  errors: number;
}

export interface RestoreDownloadFailedPayload {
  files_written: number;
  bytes_written: number;
  total_bytes: number;
  errors: number;
  error?: string;
}

export interface DownloadMirrorScanStartPayload {
  total: number;
  download_after: boolean;
}

export interface DownloadMirrorScanProgressPayload {
  scanned: number;
  present: number;
  missing: number;
  total: number;
  download_after: boolean;
}

export interface DownloadMirrorScanCompletePayload {
  scanned: number;
  present: number;
  missing: number;
  total: number;
  download_after: boolean;
}

export interface DownloadMirrorScanFailedPayload {
  error: string;
  download_after: boolean;
}

export interface DownloadMirrorStartPayload {
  total: number;
}

export interface DownloadMirrorProgressPayload {
  processed: number;
  total: number;
  path?: string;
  files_written: number;
  bytes_written: number;
  errors: number;
  error?: string;
}

export interface DownloadMirrorCompletePayload {
  files_written: number;
  bytes_written: number;
  errors: number;
}

export interface DownloadMirrorFailedPayload {
  files_written: number;
  bytes_written: number;
  errors: number;
  error?: string;
}

export interface DownloadMirrorCancelledPayload {
  files_written: number;
  bytes_written: number;
  errors: number;
  error?: string;
}

export type SseEvent =
  | { type: 'scan_start'; data: EmptyPayload }
  | { type: 'scan_progress'; data: ScanProgressPayload }
  | { type: 'scan_complete'; data: ScanCompletePayload }
  | { type: 'upload_plan'; data: UploadPlanPayload }
  | { type: 'copy_progress'; data: CopyProgressPayload }
  | { type: 'upload_start'; data: UploadStartPayload }
  | { type: 'upload_progress'; data: UploadProgressPayload }
  | { type: 'upload_complete'; data: UploadCompletePayload }
  | { type: 'upload_failed'; data: UploadFailedPayload }
  | { type: 'run_start'; data: RunStartPayload }
  | { type: 'run_log'; data: RunLogPayload }
  | { type: 'run_complete'; data: RunCompletePayload }
  | { type: 'db_sync_start'; data: DBSyncStartPayload }
  | { type: 'db_sync_progress'; data: DBSyncProgressPayload }
  | { type: 'db_sync_complete'; data: DBSyncCompletePayload }
  | { type: 'db_sync_failed'; data: DBSyncFailedPayload }
  | { type: 'restore_manifest_start'; data: RestoreManifestStartPayload }
  | { type: 'restore_manifest_progress'; data: RestoreManifestProgressPayload }
  | { type: 'restore_manifest_complete'; data: RestoreManifestCompletePayload }
  | { type: 'restore_manifest_failed'; data: RestoreManifestFailedPayload }
  | { type: 'restore_scan_start'; data: RestoreScanStartPayload }
  | { type: 'restore_scan_progress'; data: RestoreScanProgressPayload }
  | { type: 'restore_scan_complete'; data: RestoreScanCompletePayload }
  | { type: 'restore_scan_failed'; data: RestoreScanFailedPayload }
  | { type: 'restore_request_start'; data: RestoreRequestStartPayload }
  | { type: 'restore_request_progress'; data: RestoreRequestProgressPayload }
  | { type: 'restore_request_complete'; data: RestoreRequestCompletePayload }
  | { type: 'restore_request_failed'; data: RestoreRequestFailedPayload }
  | { type: 'restore_download_start'; data: RestoreDownloadStartPayload }
  | { type: 'restore_download_progress'; data: RestoreDownloadProgressPayload }
  | { type: 'restore_download_complete'; data: RestoreDownloadCompletePayload }
  | { type: 'restore_download_failed'; data: RestoreDownloadFailedPayload }
  | { type: 'download_mirror_scan_start'; data: DownloadMirrorScanStartPayload }
  | { type: 'download_mirror_scan_progress'; data: DownloadMirrorScanProgressPayload }
  | { type: 'download_mirror_scan_complete'; data: DownloadMirrorScanCompletePayload }
  | { type: 'download_mirror_scan_failed'; data: DownloadMirrorScanFailedPayload }
  | { type: 'download_mirror_start'; data: DownloadMirrorStartPayload }
  | { type: 'download_mirror_progress'; data: DownloadMirrorProgressPayload }
  | { type: 'download_mirror_complete'; data: DownloadMirrorCompletePayload }
  | { type: 'download_mirror_failed'; data: DownloadMirrorFailedPayload }
  | { type: 'download_mirror_cancelled'; data: DownloadMirrorCancelledPayload };

export interface Run {
  id: number;
  started_at: string;
  finished_at?: string;
  status: RunStatus;
  files_scanned: number;
  bytes_scanned: number;
  files_uploaded: number;
  bytes_uploaded: number;
  files_planned?: number;
  bytes_planned?: number;
  scan_paused?: boolean;
  scan_complete?: boolean;
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
  zip_id?: number;
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

export interface TreeFolderInfo {
  name: string;
  path: string;
  file_count: number;
  total_size: number;
}

export interface TreePage {
  prefix: string;
  folders: TreeFolderInfo[];
  files: FileRow[];
}

export interface SubtreeIDs {
  ids: number[];
  paths: string[];
  total: number;
  truncated: boolean;
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

export interface ClientLog {
  id: number;
  timestamp: string;
  received_at: string;
  level: 'debug' | 'info' | 'warn' | 'error';
  source: string;
  message: string;
  route?: string;
  url?: string;
  stack?: string;
  session_id?: string;
  context?: Record<string, unknown>;
}

export interface ClientLogsPage {
  logs: ClientLog[];
  total: number;
  page: number;
  limit: number;
}

export interface FileStats {
  by_status: Record<string, number>;
  by_restore_status: Record<string, number>;
  by_download_present: Record<string, number>;
  restore_soonest_expires_at?: string;
  total_count: number;
  total_size: number;
}

export interface Status {
  current?: Run;
  last?: Run;
  download_current?: DownloadJobSummary;
  download_last?: DownloadJobSummary;
  download_mirror_snapshot?: DownloadMirrorSnapshot;
  restore_download_current?: RestoreDownloadSummary;
  restore_download_last?: RestoreDownloadSummary;
  restore_job_current?: RestoreJobSummary;
  restore_job_last?: RestoreJobSummary;
  stop_requested?: boolean;
  cancel_requested?: boolean;
}

export interface RestoreJobSummary {
  id: number;
  kind: 'trigger' | 'inventory';
  started_at: string;
  finished_at?: string;
  status: 'running' | 'completed' | 'failed' | 'cancelled';
  phase: 'starting' | 'manifest' | 'request' | 'scan' | 'complete' | 'failed' | 'cancelled';
  total: number;
  processed: number;
  scanned: number;
  updated: number;
  errors: number;
  keys_requested?: number;
  keys_already_in_progress?: number;
  keys_already_available?: number;
  files_affected?: number;
  bytes_affected?: number;
  files_skipped_in_progress?: number;
  bytes_skipped_in_progress?: number;
  files_skipped_restored?: number;
  bytes_skipped_restored?: number;
  unknown_paths?: string[];
  manifest_key?: string;
  error_message?: string;
}

export interface DownloadJobSummary {
  id: number;
  started_at: string;
  finished_at?: string;
  status: 'running' | 'completed' | 'failed' | 'cancelled';
  phase: 'scan' | 'download' | 'complete' | 'failed' | 'cancelled';
  download_dir: string;
  total: number;
  total_bytes: number;
  object_count: number;
  request_fee_usd: number;
  egress_fee_usd: number;
  total_fee_usd: number;
  scanned: number;
  present: number;
  missing: number;
  processed: number;
  files_written: number;
  bytes_written: number;
  errors: number;
  error_message?: string;
}

export interface DownloadFullResponse {
  download_id: number;
}

export interface DownloadMirrorSnapshot {
  download_dir: string;
  scanned_at: string;
  total: number;
  present: number;
  missing: number;
}

export interface RestoreDownloadSummary {
  id: number;
  started_at: string;
  finished_at?: string;
  status: 'running' | 'completed' | 'failed';
  phase: 'download' | 'complete' | 'failed';
  target_dir: string;
  total: number;
  total_bytes: number;
  processed: number;
  files_written: number;
  bytes_written: number;
  errors: number;
  current_path?: string;
  current_bytes?: number;
  current_total_bytes?: number;
  current_percent?: number;
  error_message?: string;
}

export interface RestoreDownloadTriggerResponse {
  restore_download_id: number;
}

export interface RestoreJobStartResponse {
  restore_job_id: number;
  status: string;
  kind: 'trigger' | 'inventory';
  phase: string;
}

export interface RestoreEstimate {
  // Counts of actual S3 objects that will generate fresh
  // RestoreObject calls — zip members collapse to one archive key and
  // already_in_progress / already_restored buckets are excluded.
  file_count: number;
  total_bytes: number;
  request_fee_usd: number;
  retrieval_fee_usd: number;
  storage_fee_usd: number;
  egress_fee_usd: number;
  total_fee_usd: number;
  wait_hours_min: number;
  wait_hours_max: number;
  already_in_progress_count: number;
  already_in_progress_bytes: number;
  already_restored_count: number;
  already_restored_bytes: number;
  unknown_paths?: string[];
}

export interface RestoreDownloadResponse {
  files_written: number;
  bytes_written: number;
  total_bytes: number;
  skipped?: string[];
  errors?: string[];
}

export interface RestoreDownloadEstimate {
  object_count: number;
  total_bytes: number;
  restored_count: number;
  in_progress_count: number;
  not_restoring_count: number;
  request_fee_usd: number;
  egress_fee_usd: number;
  total_fee_usd: number;
  unknown_paths?: string[];
}

export interface SyncResponse {
  zip_names_in_db: number;
  individual_keys_in_db: number;
  keys_in_s3: number;
  missing_zips: number;
  missing_individual: number;
  files_created: number;
  files_reset: number;
}

export interface FullSyncResponse extends SyncResponse {
  cloud_file_count: number;
  local_file_count: number;
  zip_indexes_consumed: number;
  local_missing_count: number;
  local_missing_from_cloud?: string[];
  cloud_missing_count: number;
  cloud_missing_from_local?: string[];
}

export interface TestResult {
  ok: boolean;
  message?: string;
}

export interface ProfileInfo {
  name: string;
  active: boolean;
  bucket?: string;
  config_path: string;
  index_path: string;
}

export interface ProfilesResponse {
  profiles: ProfileInfo[];
  active_profile: string;
  switch_blocked: boolean;
  blocked_reason?: string;
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
  scan_batch_bytes: number;
  tmp_dir: string;
  download_dir: string;
  schedule: string;
  zip_threshold: number;
  min_zip_dir_files: number;
  zip_max_bytes: number;
  enable_zip_index: boolean;
  retry_failed: boolean;
  copy_threads: number;
  upload_threads: number;
  pipeline_queue: number;
  log_retention_days: number;
  log_max_per_run: number;
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

// ApiError distinguishes network, HTTP, and response-parse failures so
// callers can branch on `e.kind` (e.g. show an "offline" UX for network).
// `body` carries the first 200 chars of an unparseable response. (#203)
export class ApiError extends Error {
  kind: 'network' | 'http' | 'parse' | 'abort';
  status?: number;
  body?: string;
  constructor(kind: 'network' | 'http' | 'parse' | 'abort', message: string, opts?: { status?: number; body?: string }) {
    super(message);
    this.name = 'ApiError';
    this.kind = kind;
    this.status = opts?.status;
    this.body = opts?.body;
  }
}

let unauthorizedHandler: (() => void) | null = null;

export function setUnauthorizedHandler(handler: (() => void) | null) {
  unauthorizedHandler = handler;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let resp: Response;
  try {
    resp = await fetch(path, {
      ...init,
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json',
        ...(init?.headers ?? {}),
      },
    });
  } catch (e) {
    // Surface AbortError as its own kind so callers can ignore it
    // silently (intentional abort isn't an error to toast). (#204)
    if (e instanceof DOMException && e.name === 'AbortError') {
      throw new ApiError('abort', 'aborted');
    }
    const err = new ApiError('network', `network error: ${e instanceof Error ? e.message : String(e)}`);
    recordClientLog('error', 'request', err.message, { path });
    throw err;
  }
  if (!resp.ok) {
    if (resp.status === 401) {
      unauthorizedHandler?.();
    }
    let msg = resp.statusText;
    let body: string | undefined;
    try {
      const text = await resp.text();
      body = text.slice(0, 200);
      try {
        const parsed = JSON.parse(text);
        if (parsed?.error) msg = parsed.error;
      } catch { /* not JSON — keep statusText, surface body */ }
    } catch { /* couldn't read body */ }
    const err = new ApiError('http', `${resp.status}: ${msg}`, { status: resp.status, body });
    recordClientLog('error', 'request', err.message, {
      path,
      status: resp.status,
      body,
    });
    throw err;
  }
  if (resp.status === 204) return undefined as T;
  try {
    return (await resp.json()) as T;
  } catch (e) {
    const err = new ApiError('parse', `invalid JSON response: ${e instanceof Error ? e.message : String(e)}`);
    recordClientLog('error', 'request', err.message, { path });
    throw err;
  }
}

export const api = {
  authStatus: () => request<AuthStatus>('/api/auth/status'),
  login: (password: string) =>
    request<AuthStatus>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ password }),
    }),
  logout: () => request<AuthStatus>('/api/auth/logout', { method: 'POST' }),
  status: () => request<Status>('/api/status'),
  runs: (page = 1, limit = 20, signal?: AbortSignal) =>
    request<RunsPage>(`/api/runs?page=${page}&limit=${limit}`, { signal }),
  run: (id: number, signal?: AbortSignal) => request<RunDetail>(`/api/runs/${id}`, { signal }),
  triggerRun: (opts?: { mode?: 'full' | 'scan' | 'upload'; paths?: string[] }) =>
    request<{ run_id: number }>('/api/runs', {
      method: 'POST',
      body: opts ? JSON.stringify(opts) : undefined,
    }),
  cancelRun: (id: number) => request<{ status: string }>(`/api/runs/${id}/cancel`, { method: 'POST' }),
  stopRun: (id: number) => request<{ status: string }>(`/api/runs/${id}/stop`, { method: 'POST' }),
  continueRun: (id: number) => request<{ status: string }>(`/api/runs/${id}/continue`, { method: 'POST' }),
  deleteRunLogs: () =>
    request<{ affected: number }>('/api/run-logs', {
      method: 'DELETE',
    }),
  invalidateScanCache: () =>
    request<{ affected: number }>('/api/scan-cache', {
      method: 'DELETE',
    }),
  clientLogs: (page = 1, limit = 100, signal?: AbortSignal) =>
    request<ClientLogsPage>(`/api/client-logs?page=${page}&limit=${limit}`, { signal }),
  postClientLogs: (entries: Array<Omit<ClientLog, 'id' | 'received_at'>>) =>
    request<{ affected: number }>('/api/client-logs', {
      method: 'POST',
      body: JSON.stringify({ entries }),
    }),
  deleteClientLogs: () =>
    request<{ affected: number }>('/api/client-logs', {
      method: 'DELETE',
    }),

  files: (
    opts: { page?: number; limit?: number; status?: string; search?: string; all?: boolean } = {},
    signal?: AbortSignal,
  ) => {
    const qs = new URLSearchParams();
    if (opts.page) qs.set('page', String(opts.page));
    if (opts.limit) qs.set('limit', String(opts.limit));
    if (opts.status) qs.set('status', opts.status);
    if (opts.search) qs.set('search', opts.search);
    if (opts.all) qs.set('all', 'true');
    return request<FilesPage>(`/api/files?${qs.toString()}`, { signal });
  },
  filesTree: (
    opts: { prefix?: string; status?: string } = {},
    signal?: AbortSignal,
  ) => {
    const qs = new URLSearchParams();
    if (opts.prefix) qs.set('prefix', opts.prefix);
    if (opts.status) qs.set('status', opts.status);
    return request<TreePage>(`/api/files/tree?${qs.toString()}`, { signal });
  },
  filesSubtreeIDs: (
    opts: { prefix: string; status?: string },
    signal?: AbortSignal,
  ) => {
    const qs = new URLSearchParams();
    qs.set('prefix', opts.prefix);
    if (opts.status) qs.set('status', opts.status);
    return request<SubtreeIDs>(`/api/files/subtree-ids?${qs.toString()}`, { signal });
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
  profiles: () => request<ProfilesResponse>('/api/profiles'),
  createProfile: (name: string, cloneActive = true) =>
    request<ProfileInfo>('/api/profiles', {
      method: 'POST',
      body: JSON.stringify({ name, clone_active: cloneActive }),
    }),
  switchProfile: (name: string) =>
    request<ProfileInfo>('/api/profiles/active', {
      method: 'PUT',
      body: JSON.stringify({ name }),
    }),
  renameProfile: (oldName: string, newName: string) =>
    request<ProfileInfo>(`/api/profiles/${encodeURIComponent(oldName)}/rename`, {
      method: 'PUT',
      body: JSON.stringify({ name: newName }),
    }),
  deleteProfile: (name: string) =>
    request<{ status: string }>(`/api/profiles/${encodeURIComponent(name)}`, { method: 'DELETE' }),

  testSource: () => request<TestResult>('/api/smb/test'),
  testStorage: () => request<TestResult>('/api/s3/test'),

  sync: () => request<FullSyncResponse>('/api/sync', { method: 'POST' }),

  syncFull: () => request<FullSyncResponse>('/api/sync/full', { method: 'POST' }),

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

  restoreEstimate: (paths: string[], days: number, tier: RestoreTier) =>
    request<RestoreEstimate>('/api/restore/estimate', {
      method: 'POST',
      body: JSON.stringify({ paths, days, tier }),
    }),
  /**
   * Issue a Glacier restore request for every unique S3 key covering the
   * matched paths. Does NOT download anything — Glacier objects aren't
   * readable until S3 has thawed them (hours to a day). Track completion
   * via restoreSyncStatus / restoreScanFull / restoreScanPending. The
   * affected DB rows immediately move to restore_status='in_progress'
   * so the UI reflects the request.
   */
  restoreTrigger: (paths: string[], days: number, tier: RestoreTier) =>
    request<RestoreJobStartResponse>('/api/restore/trigger', {
      method: 'POST',
      body: JSON.stringify({ paths, days, tier }),
    }),
  restoreDownload: (paths: string[], targetDir: string, verifyChecksum = true) =>
    request<RestoreDownloadTriggerResponse>('/api/restore/download', {
      method: 'POST',
      body: JSON.stringify({ paths, target_dir: targetDir, verify_checksum: verifyChecksum }),
    }),
  downloadFull: () =>
    request<DownloadFullResponse>('/api/download/full', {
      method: 'POST',
    }),
  downloadRescan: () =>
    request<DownloadFullResponse>('/api/download/rescan', {
      method: 'POST',
    }),
  downloadCancel: () =>
    request<{ status: 'cancelling' }>('/api/download/cancel', {
      method: 'POST',
    }),
  restoreDownloadEstimate: (paths: string[]) =>
    request<RestoreDownloadEstimate>('/api/restore/download/estimate', {
      method: 'POST',
      body: JSON.stringify({ paths }),
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
    request<RestoreJobStartResponse>('/api/restore/inventory/sync', {
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

export interface AuthStatus {
  password_set: boolean;
  authenticated: boolean;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function readString(value: unknown): string | null {
  return typeof value === 'string' ? value : null;
}

function readNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

function readBoolean(value: unknown): boolean | null {
  return typeof value === 'boolean' ? value : null;
}

function readOptionalString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function readOptionalNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

function readOptionalBoolean(value: unknown): boolean | undefined {
  return typeof value === 'boolean' ? value : undefined;
}

function parseSseEvent(type: string, data: unknown): SseEvent | null {
  if (!isRecord(data)) {
    return type === 'scan_start' ? { type, data: {} } : null;
  }

  switch (type) {
    case 'scan_start':
      return { type, data: {} };
    case 'scan_progress': {
      const seen = readNumber(data.seen);
      const bytes = readNumber(data.bytes);
      const next = readNumber(data.new);
      const changed = readNumber(data.changed);
      if (seen === null || bytes === null || next === null || changed === null) return null;
      return { type, data: { seen, bytes, new: next, changed } };
    }
    case 'scan_complete': {
      const seen = readNumber(data.seen);
      const bytes = readNumber(data.bytes);
      const next = readNumber(data.new);
      const changed = readNumber(data.changed);
      const unchanged = readNumber(data.unchanged);
      const missing = readNumber(data.missing);
      if (seen === null || bytes === null || next === null || changed === null || unchanged === null || missing === null) return null;
      return { type, data: { seen, bytes, new: next, changed, unchanged, missing, paused: readOptionalBoolean(data.paused) } };
    }
    case 'upload_plan': {
      const total_files = readNumber(data.total_files);
      const total_groups = readNumber(data.total_groups);
      const total_bytes = readNumber(data.total_bytes);
      if (total_files === null || total_groups === null || total_bytes === null) return null;
      return { type, data: { total_files, total_groups, total_bytes } };
    }
    case 'copy_progress': {
      const key = readString(data.key);
      const size = readNumber(data.size);
      const bytes_copied = readNumber(data.bytes_copied);
      const percent = readNumber(data.percent);
      if (key === null || size === null || bytes_copied === null || percent === null) return null;
      return { type, data: { key, size, bytes_copied, percent } };
    }
    case 'upload_start': {
      const key = readString(data.key);
      const size = readNumber(data.size);
      if (key === null || size === null) return null;
      return { type, data: { key, size, files: readOptionalNumber(data.files) } };
    }
    case 'upload_progress': {
      const key = readString(data.key);
      const bytes_uploaded = readNumber(data.bytes_uploaded);
      const size = readNumber(data.size);
      const percent = readNumber(data.percent);
      if (key === null || bytes_uploaded === null || size === null || percent === null) return null;
      return { type, data: { key, bytes_uploaded, size, percent } };
    }
    case 'upload_complete': {
      const key = readString(data.key);
      const size = readNumber(data.size);
      if (key === null || size === null) return null;
      return {
        type,
        data: {
          key,
          size,
          etag: readOptionalString(data.etag),
          checksum_sha256: readOptionalString(data.checksum_sha256),
          files: readOptionalNumber(data.files),
          skipped: readOptionalBoolean(data.skipped),
        },
      };
    }
    case 'upload_failed': {
      const key = readString(data.key);
      const error = readString(data.error);
      if (key === null || error === null) return null;
      return { type, data: { key, error } };
    }
    case 'run_start': {
      const files_scanned = readNumber(data.files_scanned);
      const bytes_scanned = readNumber(data.bytes_scanned);
      const files_uploaded = readNumber(data.files_uploaded);
      const bytes_uploaded = readNumber(data.bytes_uploaded);
      if (files_scanned === null || bytes_scanned === null || files_uploaded === null || bytes_uploaded === null) return null;
      return { type, data: { files_scanned, bytes_scanned, files_uploaded, bytes_uploaded } };
    }
    case 'run_log': {
      const message = readString(data.message);
      if (message === null) return null;
      return { type, data: { level: readOptionalString(data.level) as RunLogPayload['level'], message } };
    }
    case 'run_complete': {
      const status = readString(data.status);
      const files_scanned = readNumber(data.files_scanned);
      const bytes_scanned = readNumber(data.bytes_scanned);
      const files_uploaded = readNumber(data.files_uploaded);
      const bytes_uploaded = readNumber(data.bytes_uploaded);
      if (status === null || files_scanned === null || bytes_scanned === null || files_uploaded === null || bytes_uploaded === null) return null;
      return {
        type,
        data: {
          status,
          files_scanned,
          bytes_scanned,
          files_uploaded,
          bytes_uploaded,
          error_message: readOptionalString(data.error_message),
        },
      };
    }
    case 'db_sync_start':
      return { type, data: { reason: readOptionalString(data.reason) as DBSyncStartPayload['reason'], size: readOptionalNumber(data.size) } };
    case 'db_sync_progress':
      return {
        type,
        data: {
          reason: readOptionalString(data.reason) as DBSyncProgressPayload['reason'],
          bytes: readOptionalNumber(data.bytes),
          total: readOptionalNumber(data.total),
          percent: readOptionalNumber(data.percent),
        },
      };
    case 'db_sync_complete':
      return { type, data: { reason: readOptionalString(data.reason) as DBSyncCompletePayload['reason'], size: readOptionalNumber(data.size) } };
    case 'db_sync_failed':
      return { type, data: { reason: readOptionalString(data.reason) as DBSyncFailedPayload['reason'], error: readOptionalString(data.error) } };
    case 'restore_manifest_start': {
      const stage = readString(data.stage);
      const processed = readNumber(data.processed);
      const total = readNumber(data.total);
      if (stage === null || processed === null || total === null) return null;
      return { type, data: { stage, processed, total, manifest_key: readOptionalString(data.manifest_key) } };
    }
    case 'restore_manifest_progress': {
      const stage = readString(data.stage);
      const processed = readNumber(data.processed);
      const total = readNumber(data.total);
      if (stage === null || processed === null || total === null) return null;
      return {
        type,
        data: {
          stage,
          processed,
          total,
          manifest_key: readOptionalString(data.manifest_key),
          data_key: readOptionalString(data.data_key),
          keys: readOptionalNumber(data.keys),
        },
      };
    }
    case 'restore_manifest_complete': {
      const stage = readString(data.stage);
      const processed = readNumber(data.processed);
      const total = readNumber(data.total);
      if (stage === null || processed === null || total === null) return null;
      return { type, data: { stage, processed, total, manifest_key: readOptionalString(data.manifest_key), keys: readOptionalNumber(data.keys) } };
    }
    case 'restore_manifest_failed': {
      const stage = readString(data.stage);
      const error = readString(data.error);
      if (stage === null || error === null) return null;
      return { type, data: { stage, error, manifest_key: readOptionalString(data.manifest_key), data_key: readOptionalString(data.data_key) } };
    }
    case 'restore_scan_start': {
      const mode = readString(data.mode);
      const total = readNumber(data.total);
      if (mode === null || total === null) return null;
      return { type, data: { mode, total } };
    }
    case 'restore_scan_progress': {
      const mode = readString(data.mode);
      const scanned = readNumber(data.scanned);
      const updated = readNumber(data.updated);
      const errors = readNumber(data.errors);
      const total = readNumber(data.total);
      if (mode === null || scanned === null || updated === null || errors === null || total === null) return null;
      return { type, data: { mode, scanned, updated, errors, total } };
    }
    case 'restore_scan_complete': {
      const mode = readString(data.mode);
      const scanned = readNumber(data.scanned);
      const updated = readNumber(data.updated);
      const errors = readNumber(data.errors);
      if (mode === null || scanned === null || updated === null || errors === null) return null;
      return { type, data: { mode, scanned, updated, errors } };
    }
    case 'restore_scan_failed': {
      const mode = readString(data.mode);
      const error = readString(data.error);
      if (mode === null || error === null) return null;
      return { type, data: { mode, error } };
    }
    case 'restore_request_start': {
      const total = readNumber(data.total);
      if (total === null) return null;
      return { type, data: { total } };
    }
    case 'restore_request_progress': {
      const processed = readNumber(data.processed);
      const total = readNumber(data.total);
      if (processed === null || total === null) return null;
      return {
        type,
        data: {
          processed,
          total,
          keys_requested: readOptionalNumber(data.keys_requested),
          keys_already_thawed: readOptionalNumber(data.keys_already_thawed),
          errors: readOptionalNumber(data.errors),
        },
      };
    }
    case 'restore_request_complete': {
      const total = readNumber(data.total);
      const keys_requested = readNumber(data.keys_requested);
      const keys_already_in_progress = readNumber(data.keys_already_in_progress);
      const keys_already_available = readNumber(data.keys_already_available);
      const files_affected = readNumber(data.files_affected);
      const bytes_affected = readNumber(data.bytes_affected);
      if (
        total === null || keys_requested === null || keys_already_in_progress === null ||
        keys_already_available === null || files_affected === null || bytes_affected === null
      ) return null;
      return {
        type,
        data: {
          total,
          keys_requested,
          keys_already_in_progress,
          keys_already_available,
          files_affected,
          bytes_affected,
          errors: readOptionalNumber(data.errors),
        },
      };
    }
    case 'restore_request_failed': {
      const error = readString(data.error);
      if (error === null) return null;
      return { type, data: { error, processed: readOptionalNumber(data.processed), total: readOptionalNumber(data.total) } };
    }
    case 'restore_download_start': {
      const total = readNumber(data.total);
      const total_bytes = readNumber(data.total_bytes);
      if (total === null || total_bytes === null) return null;
      return { type, data: { total, total_bytes } };
    }
    case 'restore_download_progress': {
      const path = readString(data.path);
      const processed = readNumber(data.processed);
      const total = readNumber(data.total);
      const total_bytes = readNumber(data.total_bytes);
      const files_written = readNumber(data.files_written);
      const bytes_written = readNumber(data.bytes_written);
      const errors = readNumber(data.errors);
      if (
        path === null || processed === null || total === null || total_bytes === null ||
        files_written === null || bytes_written === null || errors === null
      ) return null;
      return {
        type,
        data: {
          path,
          processed,
          total,
          total_bytes,
          files_written,
          bytes_written,
          errors,
          current_path: readOptionalString(data.current_path),
          current_bytes: readOptionalNumber(data.current_bytes),
          current_total_bytes: readOptionalNumber(data.current_total_bytes),
          current_percent: readOptionalNumber(data.current_percent),
          file_status: readOptionalString(data.file_status) as RestoreDownloadProgressPayload['file_status'],
          error: readOptionalString(data.error),
        },
      };
    }
    case 'restore_download_complete':
    case 'restore_download_failed': {
      const files_written = readNumber(data.files_written);
      const bytes_written = readNumber(data.bytes_written);
      const total_bytes = readNumber(data.total_bytes);
      const errors = readNumber(data.errors);
      if (files_written === null || bytes_written === null || total_bytes === null || errors === null) return null;
      const base = { files_written, bytes_written, total_bytes, errors };
      return type === 'restore_download_complete'
        ? { type, data: base }
        : { type, data: { ...base, error: readOptionalString(data.error) } };
    }
    case 'download_mirror_scan_start': {
      const total = readNumber(data.total);
      const download_after = readBoolean(data.download_after);
      if (total === null || download_after === null) return null;
      return { type, data: { total, download_after } };
    }
    case 'download_mirror_scan_progress':
    case 'download_mirror_scan_complete': {
      const scanned = readNumber(data.scanned);
      const present = readNumber(data.present);
      const missing = readNumber(data.missing);
      const total = readNumber(data.total);
      const download_after = readBoolean(data.download_after);
      if (scanned === null || present === null || missing === null || total === null || download_after === null) return null;
      return { type, data: { scanned, present, missing, total, download_after } };
    }
    case 'download_mirror_scan_failed': {
      const error = readString(data.error);
      const download_after = readBoolean(data.download_after);
      if (error === null || download_after === null) return null;
      return { type, data: { error, download_after } };
    }
    case 'download_mirror_start': {
      const total = readNumber(data.total);
      if (total === null) return null;
      return { type, data: { total } };
    }
    case 'download_mirror_progress': {
      const processed = readNumber(data.processed);
      const total = readNumber(data.total);
      const files_written = readNumber(data.files_written);
      const bytes_written = readNumber(data.bytes_written);
      const errors = readNumber(data.errors);
      if (processed === null || total === null || files_written === null || bytes_written === null || errors === null) return null;
      return {
        type,
        data: {
          processed,
          total,
          path: readOptionalString(data.path),
          files_written,
          bytes_written,
          errors,
          error: readOptionalString(data.error),
        },
      };
    }
    case 'download_mirror_complete':
    case 'download_mirror_failed':
    case 'download_mirror_cancelled': {
      const files_written = readNumber(data.files_written);
      const bytes_written = readNumber(data.bytes_written);
      const errors = readNumber(data.errors);
      if (files_written === null || bytes_written === null || errors === null) return null;
      const base = { files_written, bytes_written, errors };
      return type === 'download_mirror_complete'
        ? { type, data: base }
        : { type, data: { ...base, error: readOptionalString(data.error) } };
    }
    default:
      return null;
  }
}

// subscribeEvents opens a live EventSource against /api/events. Returns
// an AbortController-style { close } — callers should invoke it on
// component teardown to unsubscribe. The optional onStatus callback fires
// with 'open' once the stream is connected and 'error' on disconnect, so
// the UI can surface a "live updates disconnected" banner instead of
// freezing silently when the connection drops. The browser auto-reconnects
// on transient network errors; onStatus('open') will fire again once it's back.
export function subscribeEvents(
  onEvent: (event: SseEvent) => void,
  onStatus?: (status: 'open' | 'error') => void,
): { close: () => void } {
  const es = new EventSource('/api/events');
  const handler = (ev: MessageEvent) => {
    let raw: unknown;
    try {
      raw = JSON.parse(ev.data);
    } catch {
      raw = ev.data;
    }
    const parsed = parseSseEvent(
      ev.type,
      isRecord(raw) && 'data' in raw ? raw.data : raw,
    );
    if (!parsed) {
      if (import.meta.env.DEV) {
        console.warn('[subscribeEvents] invalid SSE payload — ignoring:', ev.type, raw);
      }
      return;
    }
    onEvent(parsed);
  };
  const types: SseEvent['type'][] = [
    'scan_start', 'scan_progress', 'scan_complete',
    'upload_plan',
    'copy_progress',
    'upload_start', 'upload_progress', 'upload_complete', 'upload_failed',
    'run_start', 'run_log', 'run_complete',
    'db_sync_start', 'db_sync_progress', 'db_sync_complete', 'db_sync_failed',
    'restore_manifest_start', 'restore_manifest_progress', 'restore_manifest_complete', 'restore_manifest_failed',
    'restore_scan_start', 'restore_scan_progress', 'restore_scan_complete', 'restore_scan_failed',
    'restore_request_start', 'restore_request_progress', 'restore_request_complete', 'restore_request_failed',
    'restore_download_start', 'restore_download_progress', 'restore_download_complete', 'restore_download_failed',
    'download_mirror_scan_start', 'download_mirror_scan_progress', 'download_mirror_scan_complete', 'download_mirror_scan_failed',
    'download_mirror_start', 'download_mirror_progress', 'download_mirror_complete', 'download_mirror_failed', 'download_mirror_cancelled',
  ];
  for (const t of types) es.addEventListener(t, handler as EventListener);
  es.onmessage = (ev) => {
    if (import.meta.env.DEV) {
      console.warn('[subscribeEvents] untyped SSE message — ignoring:', ev.data);
    }
  };
  if (onStatus) {
    // EventSource fires 'error' on every reconnect attempt while the
    // connection is down (every ~3s). Debounce to transitions only so
    // consumers like a "live updates disconnected" banner don't flicker.
    // (#261)
    let lastStatus: 'open' | 'error' | undefined;
    const emit = (s: 'open' | 'error') => {
      if (s === lastStatus) return;
      lastStatus = s;
      onStatus(s);
    };
    es.addEventListener('open', () => emit('open'));
    es.addEventListener('error', () => emit('error'));
  }
  return { close: () => es.close() };
}
