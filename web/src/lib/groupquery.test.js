import { describe, it, expect } from 'vitest';
import {
  defaultGroupQuery,
  buildGroupsQuery,
  clampPageSize,
  totalPages,
  pageRangeLabel,
  nextSort,
} from './groupquery.js';

describe('buildGroupsQuery', () => {
  it('omits defaults, sets limit/offset from page', () => {
    expect(buildGroupsQuery(defaultGroupQuery())).toBe('limit=50&offset=0');
  });
  it('serialises search/status/errors/sort/order and paging', () => {
    const qs = buildGroupsQuery({
      q: 'alt', status: 'active', errorsOnly: true, sort: 'lag', desc: true, page: 3, pageSize: 25,
    });
    const p = new URLSearchParams(qs);
    expect(p.get('q')).toBe('alt');
    expect(p.get('status')).toBe('active');
    expect(p.get('errors')).toBe('true');
    expect(p.get('sort')).toBe('lag');
    expect(p.get('order')).toBe('desc');
    expect(p.get('limit')).toBe('25');
    expect(p.get('offset')).toBe('50'); // (3-1)*25
  });
  it('does not emit sort=name (the default)', () => {
    const p = new URLSearchParams(buildGroupsQuery({ sort: 'name' }));
    expect(p.has('sort')).toBe(false);
  });
});

describe('clampPageSize', () => {
  it('bounds to [1,500] and defaults to 50 for falsy/invalid', () => {
    expect(clampPageSize(0)).toBe(50); // 0 is falsy -> default
    expect(clampPageSize(-5)).toBe(1); // negative clamps to 1
    expect(clampPageSize(9999)).toBe(500);
    expect(clampPageSize(NaN)).toBe(50);
    expect(clampPageSize(25)).toBe(25);
  });
});

describe('totalPages', () => {
  it('ceils and floors at 1', () => {
    expect(totalPages(0, 50)).toBe(1);
    expect(totalPages(50, 50)).toBe(1);
    expect(totalPages(51, 50)).toBe(2);
    expect(totalPages(120, 50)).toBe(3);
  });
});

describe('pageRangeLabel', () => {
  it('reports the visible range', () => {
    expect(pageRangeLabel(1, 50, 120)).toBe('1-50 of 120');
    expect(pageRangeLabel(3, 50, 120)).toBe('101-120 of 120');
    expect(pageRangeLabel(1, 50, 0)).toBe('0 of 0');
  });
});

describe('nextSort', () => {
  it('toggles direction on the active column', () => {
    expect(nextSort({ sort: 'name', desc: false }, 'name')).toEqual({ sort: 'name', desc: true });
    expect(nextSort({ sort: 'name', desc: true }, 'name')).toEqual({ sort: 'name', desc: false });
  });
  it('selects a new column ascending', () => {
    expect(nextSort({ sort: 'name', desc: true }, 'lag')).toEqual({ sort: 'lag', desc: false });
  });
});
