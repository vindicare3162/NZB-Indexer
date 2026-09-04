import { describe, it, expect } from 'vitest';
import { parseBackfillField, buildBackfillPayload, describeBackfillField } from './backfill.js';

describe('parseBackfillField', () => {
  it('treats blank as no override (null)', () => {
    expect(parseBackfillField('', 'Days')).toEqual({ value: null, error: '' });
    expect(parseBackfillField('   ', 'Days')).toEqual({ value: null, error: '' });
    expect(parseBackfillField(null, 'Days')).toEqual({ value: null, error: '' });
    expect(parseBackfillField(undefined, 'Days')).toEqual({ value: null, error: '' });
  });

  it('accepts zero as an explicit unlimited value', () => {
    expect(parseBackfillField('0', 'Days')).toEqual({ value: 0, error: '' });
  });

  it('accepts positive integers', () => {
    expect(parseBackfillField('30', 'Days')).toEqual({ value: 30, error: '' });
    expect(parseBackfillField(' 500 ', 'Article limit')).toEqual({ value: 500, error: '' });
  });

  it('rejects negative numbers', () => {
    const r = parseBackfillField('-5', 'Days');
    expect(r.value).toBeUndefined();
    expect(r.error).toMatch(/negative/);
  });

  it('rejects non-integers and junk', () => {
    for (const bad of ['abc', '3.5', '1e3', '12x', '--1']) {
      const r = parseBackfillField(bad, 'Days');
      expect(r.value).toBeUndefined();
      expect(r.error).toMatch(/whole number/);
    }
  });
});

describe('buildBackfillPayload', () => {
  it('builds a payload with both overrides', () => {
    expect(buildBackfillPayload('30', '50000')).toEqual({
      payload: { days: 30, articles: 50000 },
      error: '',
    });
  });

  it('maps blank fields to null (clear override) independently', () => {
    expect(buildBackfillPayload('', '1000')).toEqual({
      payload: { days: null, articles: 1000 },
      error: '',
    });
    expect(buildBackfillPayload('7', '')).toEqual({
      payload: { days: 7, articles: null },
      error: '',
    });
    expect(buildBackfillPayload('', '')).toEqual({
      payload: { days: null, articles: null },
      error: '',
    });
  });

  it('preserves explicit zero (unlimited) for each field', () => {
    expect(buildBackfillPayload('0', '0')).toEqual({
      payload: { days: 0, articles: 0 },
      error: '',
    });
  });

  it('fails with the days error first', () => {
    const r = buildBackfillPayload('-1', 'abc');
    expect(r.payload).toBeNull();
    expect(r.error).toMatch(/Days/);
  });

  it('reports an article-limit error when days is valid', () => {
    const r = buildBackfillPayload('7', 'abc');
    expect(r.payload).toBeNull();
    expect(r.error).toMatch(/Article limit/);
  });
});

describe('describeBackfillField', () => {
  it('describes default, unlimited, and explicit values', () => {
    expect(describeBackfillField(null, 'days')).toBe('using global default');
    expect(describeBackfillField(0, 'days')).toBe('unlimited days');
    expect(describeBackfillField(30, 'days')).toBe('30 days');
  });
});
