// Pure helpers for the server-side group listing controls (#123): building the
// query string for GET /admin/groups and computing pagination metadata. Kept
// dependency-free for unit testing.

// defaultGroupQuery is the initial filter/sort/paging state.
export function defaultGroupQuery() {
  return { q: '', status: '', errorsOnly: false, sort: 'name', desc: false, page: 1, pageSize: 50 };
}

// buildGroupsQuery serialises a query state into a URLSearchParams string,
// omitting defaults so the URL stays clean. page is 1-based and converted to a
// 0-based offset for the API.
export function buildGroupsQuery(state = {}) {
  const s = { ...defaultGroupQuery(), ...state };
  const params = new URLSearchParams();
  if (s.q) params.set('q', s.q);
  if (s.status) params.set('status', s.status);
  if (s.errorsOnly) params.set('errors', 'true');
  if (s.sort && s.sort !== 'name') params.set('sort', s.sort);
  if (s.desc) params.set('order', 'desc');
  const pageSize = clampPageSize(s.pageSize);
  params.set('limit', String(pageSize));
  const page = Math.max(1, Math.floor(s.page) || 1);
  params.set('offset', String((page - 1) * pageSize));
  return params.toString();
}

// clampPageSize bounds the page size to the server's accepted range.
export function clampPageSize(n) {
  const v = Math.floor(Number(n)) || 50;
  if (v < 1) return 1;
  if (v > 500) return 500;
  return v;
}

// totalPages returns how many pages cover `total` items at `pageSize`.
export function totalPages(total, pageSize) {
  const ps = clampPageSize(pageSize);
  return Math.max(1, Math.ceil((Number(total) || 0) / ps));
}

// pageRangeLabel returns a human "X-Y of N" label for the current page.
export function pageRangeLabel(page, pageSize, total) {
  const t = Number(total) || 0;
  if (t === 0) return '0 of 0';
  const ps = clampPageSize(pageSize);
  const p = Math.max(1, Math.floor(page) || 1);
  const start = (p - 1) * ps + 1;
  const end = Math.min(p * ps, t);
  return `${start}-${end} of ${t}`;
}

// nextSort returns the sort state when a column header is clicked: clicking the
// active column toggles direction; clicking a new column selects it ascending.
export function nextSort(current, column) {
  if (current.sort === column) {
    return { sort: column, desc: !current.desc };
  }
  return { sort: column, desc: false };
}
