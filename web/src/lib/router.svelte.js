// Tiny hash-based router as reactive state.
//
// The hash may carry a query string for shareable state, e.g.
// "#/?q=movie&cat=2000&page=2". `path` is the part before "?" (so route
// matching like route.path === '/' still works), and `query` is the raw query
// string after "?" (empty when absent) for views that read shareable state.
export const route = $state({ path: currentPath(), query: currentQuery() });

function rawHash() {
  return window.location.hash.replace(/^#/, '');
}

function currentPath() {
  const h = rawHash();
  const i = h.indexOf('?');
  const p = i >= 0 ? h.slice(0, i) : h;
  return p || '/';
}

function currentQuery() {
  const h = rawHash();
  const i = h.indexOf('?');
  return i >= 0 ? h.slice(i + 1) : '';
}

function syncFromLocation() {
  route.path = currentPath();
  route.query = currentQuery();
}

// hashchange covers <a href="#/..."> navigations; popstate covers browser
// back/forward including history entries added by pushQuery.
window.addEventListener('hashchange', syncFromLocation);
window.addEventListener('popstate', syncFromLocation);

// navigate sets the hash (path plus optional "?query"). Passing the same value
// still updates reactive state via the hashchange handler.
export function navigate(path) {
  window.location.hash = path;
}

// replaceQuery updates only the query string of the current path WITHOUT adding
// a new browser history entry (used for live search-state sync so typing
// doesn't flood history). Back/forward still works across explicit navigations.
export function replaceQuery(query) {
  const base = '#' + currentPath();
  const url = query ? `${base}?${query}` : base;
  window.history.replaceState(null, '', url);
  route.query = query;
}

// pushQuery updates the query string and DOES add a history entry, so
// back/forward restores the previous search state.
export function pushQuery(query) {
  const base = '#' + currentPath();
  const url = query ? `${base}?${query}` : base;
  window.history.pushState(null, '', url);
  route.query = query;
}
