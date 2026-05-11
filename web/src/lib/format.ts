// Small formatting helpers shared across pages.

export function bytes(n: number): string {
  if (n === 0) return '0 B';
  // Base-2 (KiB / MiB / GiB) to match `du`, Linux conventions, and the
  // StorageSettings page that already shows MiB. Previously this was
  // base-10 (KB / MB), which displayed inconsistently with config UI. (#208)
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log(Math.abs(n)) / Math.log(1024)));
  const v = n / Math.pow(1024, i);
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
}

export function relativeTime(iso?: string): string {
  if (!iso) return '—';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return iso;
  const diff = Date.now() - t;
  const abs = Math.abs(diff);
  const sec = Math.round(abs / 1000);
  const suffix = diff >= 0 ? 'ago' : 'from now';
  if (sec < 60) return `${sec}s ${suffix}`;
  const min = Math.round(sec / 60);
  if (min < 60) return `${min}m ${suffix}`;
  const hr = Math.round(min / 60);
  if (hr < 48) return `${hr}h ${suffix}`;
  const day = Math.round(hr / 24);
  return `${day}d ${suffix}`;
}

export function formatDate(iso?: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

export function statusBadge(status: string): 'ok' | 'warn' | 'err' | 'running' {
  switch (status) {
    case 'completed':
    case 'uploaded':
    case 'restored':
      return 'ok';
    case 'failed':
    case 'missing':
      return 'err';
    case 'running':
    case 'in_progress':
      return 'running';
    default:
      return 'warn';
  }
}

// statusLabel maps the raw db status to a human-readable badge label.
// The DB still stores 'missing' for files gone from source but present in S3;
// the user-facing term for that state is "cloud only". Rows that are
// missing locally but have no recorded S3 key are shown as plain "missing"
// so never-uploaded files are not mislabeled as cloud-backed.
export function statusLabel(status: string, s3Key?: string): string {
  if (status === 'missing') return s3Key ? 'cloud only' : 'missing';
  return status;
}

// restoreLabel maps the raw db enum to a human-readable badge label.
export function restoreLabel(s?: string): string {
  switch (s) {
    case 'in_progress': return 'restoring';
    case 'restored': return 'restored';
    default: return '';
  }
}

// expiresIn renders a Glacier restored-copy expiry as "in 3d", "in 4h",
// or "expired" once it slips into the past.
export function expiresIn(iso?: string): string {
  if (!iso) return '';
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return '';
  const diff = t - Date.now();
  if (diff <= 0) return 'expired';
  const min = Math.round(diff / 60000);
  if (min < 60) return `in ${min}m`;
  const hr = Math.round(min / 60);
  if (hr < 48) return `in ${hr}h`;
  const day = Math.round(hr / 24);
  return `in ${day}d`;
}
