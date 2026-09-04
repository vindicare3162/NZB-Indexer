// Shareable release-search state (#124): serialize/parse the search form to and
// from a URL query string so a search is deep-linkable and browser back/forward
// restores it. Pure functions, unit-tested without a DOM.

// SearchState fields:
//   q     - free-text query (string)
//   cat   - category id (string; "" = all)
//   page  - 1-based page number (number)
//   limit - page size (number)
//   obf   - include obfuscated releases (bool)

// defaultState returns a fresh, empty search state.
export function defaultState() {
  return { q: '', cat: '', page: 1, limit: 50, obf: false };
}

// parseSearchState builds a SearchState from a URL query string (the part after
// "?"), applying defaults for missing/invalid values.
export function parseSearchState(query) {
  const d = defaultState();
  const p = new URLSearchParams(query || '');

  const q = p.get('q');
  const cat = p.get('cat');
  const page = parseInt(p.get('page') || '', 10);
  const limit = parseInt(p.get('limit') || '', 10);
  const obf = p.get('obf');

  return {
    q: q != null ? q : d.q,
    cat: cat != null ? cat : d.cat,
    page: Number.isFinite(page) && page >= 1 ? page : d.page,
    limit: Number.isFinite(limit) && limit >= 1 ? limit : d.limit,
    obf: obf === '1' || obf === 'true',
  };
}

// serializeSearchState renders a SearchState to a compact query string, omitting
// values equal to the defaults so shared URLs stay clean.
export function serializeSearchState(s) {
  const d = defaultState();
  const p = new URLSearchParams();
  if (s.q && s.q !== d.q) p.set('q', s.q);
  if (s.cat && s.cat !== d.cat) p.set('cat', String(s.cat));
  if (s.page && s.page !== d.page) p.set('page', String(s.page));
  if (s.limit && s.limit !== d.limit) p.set('limit', String(s.limit));
  if (s.obf) p.set('obf', '1');
  return p.toString();
}

// offsetFor converts a 1-based page + limit into a zero-based offset.
export function offsetFor(state) {
  const page = state.page >= 1 ? state.page : 1;
  const limit = state.limit >= 1 ? state.limit : 1;
  return (page - 1) * limit;
}

// statesEqual reports whether two search states are equivalent (so we can skip
// redundant work / history entries).
export function statesEqual(a, b) {
  return a.q === b.q && String(a.cat) === String(b.cat) &&
    a.page === b.page && a.limit === b.limit && !!a.obf === !!b.obf;
}
