import { describe, it, expect } from 'vitest';
import { groupLag, formatLag, lastScanLabel, hasScanError, relativeTime } from './groupscan.js';

describe('groupLag / formatLag', () => {
  it('returns null when server head unknown', () => {
    expect(groupLag({ server_high: 0, last_scanned_high: 10 })).toBeNull();
    expect(formatLag({ server_high: 0 })).toBe('');
  });
  it('computes lag from server head and watermark', () => {
    expect(groupLag({ server_high: 5000, last_scanned_high: 4900 })).toBe(100);
    expect(formatLag({ server_high: 5000, last_scanned_high: 4900 })).toBe('100 behind');
  });
  it('clamps negative lag to zero (watermark ahead)', () => {
    expect(groupLag({ server_high: 5000, last_scanned_high: 5200 })).toBe(0);
    expect(formatLag({ server_high: 5000, last_scanned_high: 5000 })).toBe('up to date');
  });
  it('handles missing group', () => {
    expect(groupLag(null)).toBeNull();
    expect(formatLag(undefined)).toBe('');
  });
});

describe('lastScanLabel', () => {
  const now = Date.parse('2026-01-01T12:00:00Z');
  it('reports never when unscanned', () => {
    expect(lastScanLabel({}, now)).toBe('never');
    expect(lastScanLabel(null, now)).toBe('never');
  });
  it('summarises a forward pass', () => {
    const g = {
      last_scan_at: '2026-01-01T11:59:00Z',
      last_scan_backfill: false,
      last_scan_articles: 1234,
    };
    expect(lastScanLabel(g, now)).toBe('1m ago · scan · 1,234 art');
  });
  it('summarises a backfill pass', () => {
    const g = {
      last_scan_at: '2026-01-01T09:00:00Z',
      last_scan_backfill: true,
      last_scan_articles: 5,
    };
    expect(lastScanLabel(g, now)).toBe('3h ago · backfill · 5 art');
  });
});

describe('hasScanError', () => {
  it('is true only when an error is present', () => {
    expect(hasScanError({ last_scan_error: 'boom' })).toBe(true);
    expect(hasScanError({ last_scan_error: '' })).toBe(false);
    expect(hasScanError({})).toBe(false);
    expect(hasScanError(null)).toBe(false);
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-01-01T12:00:00Z');
  it('renders seconds/minutes/hours/days', () => {
    expect(relativeTime('2026-01-01T11:59:30Z', now)).toBe('30s ago');
    expect(relativeTime('2026-01-01T11:30:00Z', now)).toBe('30m ago');
    expect(relativeTime('2026-01-01T06:00:00Z', now)).toBe('6h ago');
    expect(relativeTime('2025-12-30T12:00:00Z', now)).toBe('2d ago');
  });
  it('returns empty for invalid input', () => {
    expect(relativeTime('nonsense', now)).toBe('');
  });
});
