// Pure formatting helpers for per-group scan progress / error reporting (#114).
// Kept dependency-free so they can be unit-tested with Vitest.

// groupLag returns how many articles behind the server head a group is on its
// forward watermark, or null when the server head has not been observed yet.
export function groupLag(group) {
  if (!group) return null;
  const high = Number(group.server_high || 0);
  if (high <= 0) return null;
  const seen = Number(group.last_scanned_high || 0);
  const lag = high - seen;
  return lag > 0 ? lag : 0;
}

// formatLag renders the lag as a short label. Returns '' when unknown.
export function formatLag(group) {
  const lag = groupLag(group);
  if (lag == null) return '';
  if (lag === 0) return 'up to date';
  return `${lag.toLocaleString()} behind`;
}

// lastScanLabel summarises a group's most recent pass: relative time + type +
// counts, or 'never' when it has not been scanned. `nowMs` is injectable for
// deterministic tests (defaults to Date.now()).
export function lastScanLabel(group, nowMs = Date.now()) {
  if (!group || !group.last_scan_at) return 'never';
  const rel = relativeTime(group.last_scan_at, nowMs);
  const kind = group.last_scan_backfill ? 'backfill' : 'scan';
  const arts = Number(group.last_scan_articles || 0);
  return `${rel} · ${kind} · ${arts.toLocaleString()} art`;
}

// hasScanError reports whether the group's most recent pass errored.
export function hasScanError(group) {
  return !!(group && group.last_scan_error);
}

// healthLevel returns the server-derived health level for a group (#127),
// defaulting to 'unknown' when the field is absent.
export function healthLevel(group) {
  return (group && group.health && group.health.level) || 'unknown';
}

// healthLabel maps a health level to a short display label.
export function healthLabel(group) {
  switch (healthLevel(group)) {
    case 'ok': return 'OK';
    case 'warn': return 'Warning';
    case 'error': return 'Error';
    default: return 'Unknown';
  }
}

// healthReasons returns the list of reasons behind a group's health level.
export function healthReasons(group) {
  return (group && group.health && group.health.reasons) || [];
}

// formatThroughput renders a group's ingest rate as "N art/s", or '' when the
// rate is unknown (never measured).
export function formatThroughput(group) {
  const rate = Number((group && group.throughput_arts_per_sec) || 0);
  if (!(rate > 0)) return '';
  if (rate >= 100) return `${Math.round(rate).toLocaleString()} art/s`;
  return `${rate.toFixed(1)} art/s`;
}

// formatBytes renders a byte count as a compact human size (e.g. "1.5 GB").
export function formatBytes(n) {
  const bytes = Number(n) || 0;
  if (bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let v = bytes;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${i === 0 ? v : v.toFixed(1)} ${units[i]}`;
}

// relativeTime returns a compact "Ns/Nm/Nh/Nd ago" label for an ISO timestamp.
// Exported for reuse/testing.
export function relativeTime(iso, nowMs = Date.now()) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return '';
  const secs = Math.max(0, Math.round((nowMs - t) / 1000));
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs}h ago`;
  const days = Math.round(hrs / 24);
  return `${days}d ago`;
}
