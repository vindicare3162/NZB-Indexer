import { describe, it, expect } from 'vitest';
import {
  defaultState, parseSearchState, serializeSearchState, offsetFor, statesEqual,
} from './searchstate.js';

describe('parseSearchState', () => {
  it('returns defaults for an empty query', () => {
    expect(parseSearchState('')).toEqual(defaultState());
    expect(parseSearchState(null)).toEqual(defaultState());
  });

  it('parses all fields', () => {
    const s = parseSearchState('q=some+movie&cat=2000&page=3&limit=25&obf=1');
    expect(s).toEqual({ q: 'some movie', cat: '2000', page: 3, limit: 25, obf: true });
  });

  it('accepts obf=true as well as obf=1', () => {
    expect(parseSearchState('obf=true').obf).toBe(true);
    expect(parseSearchState('obf=0').obf).toBe(false);
    expect(parseSearchState('obf=1').obf).toBe(true);
  });

  it('falls back to defaults for invalid page/limit', () => {
    const s = parseSearchState('page=0&limit=-5');
    expect(s.page).toBe(1);
    expect(s.limit).toBe(50);
    const s2 = parseSearchState('page=abc');
    expect(s2.page).toBe(1);
  });

  it('preserves an explicitly empty query string', () => {
    expect(parseSearchState('q=').q).toBe('');
  });
});

describe('serializeSearchState', () => {
  it('omits default values', () => {
    expect(serializeSearchState(defaultState())).toBe('');
  });

  it('round-trips a populated state', () => {
    const s = { q: 'the matrix', cat: '2040', page: 2, limit: 25, obf: true };
    const q = serializeSearchState(s);
    expect(parseSearchState(q)).toEqual(s);
  });

  it('omits page=1 and default limit but keeps q', () => {
    const q = serializeSearchState({ q: 'x', cat: '', page: 1, limit: 50, obf: false });
    expect(q).toBe('q=x');
  });

  it('encodes obfuscated as obf=1', () => {
    expect(serializeSearchState({ ...defaultState(), obf: true })).toBe('obf=1');
  });
});

describe('offsetFor', () => {
  it('maps 1-based page to zero-based offset', () => {
    expect(offsetFor({ page: 1, limit: 50 })).toBe(0);
    expect(offsetFor({ page: 2, limit: 50 })).toBe(50);
    expect(offsetFor({ page: 3, limit: 25 })).toBe(50);
  });
  it('guards against invalid values', () => {
    expect(offsetFor({ page: 0, limit: 0 })).toBe(0);
  });
});

describe('statesEqual', () => {
  it('treats equivalent states as equal (cat number vs string)', () => {
    expect(statesEqual(
      { q: 'a', cat: '2000', page: 1, limit: 50, obf: false },
      { q: 'a', cat: 2000, page: 1, limit: 50, obf: false },
    )).toBe(true);
  });
  it('detects differences', () => {
    expect(statesEqual(defaultState(), { ...defaultState(), page: 2 })).toBe(false);
    expect(statesEqual(defaultState(), { ...defaultState(), q: 'x' })).toBe(false);
  });
});
