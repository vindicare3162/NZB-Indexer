// Minimal API client for the goindex REST API. Stores the session token in
// localStorage and attaches it as a Bearer header.

const TOKEN_KEY = 'goindex_token';

export function getToken() {
  return localStorage.getItem(TOKEN_KEY) || '';
}

export function setToken(token) {
  if (token) localStorage.setItem(TOKEN_KEY, token);
  else localStorage.removeItem(TOKEN_KEY);
}

async function request(method, path, body) {
  const headers = {};
  const token = getToken();
  if (token) headers['Authorization'] = `Bearer ${token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  const res = await fetch(`/api/v1${path}`, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  if (res.status === 401) {
    setToken('');
    throw new ApiError('Not authenticated', 401);
  }
  if (!res.ok) {
    let msg = `Request failed (${res.status})`;
    try {
      const data = await res.json();
      if (data && data.error) msg = data.error;
    } catch (_) {
      // ignore parse errors
    }
    throw new ApiError(msg, res.status);
  }
  if (res.status === 204) return null;
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('application/json')) return res.json();
  return res.text();
}

export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

export const api = {
  login: (username, password) => request('POST', '/login', { username, password }),
  me: () => request('GET', '/me'),
  categories: () => request('GET', '/categories'),
  search: ({ q = '', cat = '', limit = 50, offset = 0 } = {}) => {
    const params = new URLSearchParams();
    if (q) params.set('q', q);
    if (cat) params.set('cat', cat);
    params.set('limit', String(limit));
    params.set('offset', String(offset));
    return request('GET', `/releases?${params.toString()}`);
  },
  release: (guid) => request('GET', `/releases/${encodeURIComponent(guid)}`),
  nzbUrl: (guid) => `/api/v1/releases/${encodeURIComponent(guid)}/nzb`,

  myKeys: () => request('GET', '/apikeys'),
  createKey: (label) => request('POST', '/apikeys', { label }),
  deleteKey: (id) => request('DELETE', `/apikeys/${id}`),

  // admin
  servers: () => request('GET', '/admin/servers'),
  createServer: (s) => request('POST', '/admin/servers', s),
  updateServer: (id, s) => request('PUT', `/admin/servers/${id}`, s),
  deleteServer: (id) => request('DELETE', `/admin/servers/${id}`),
  discover: (q = '', limit = 50, offset = 0, refresh = false) => {
    const params = new URLSearchParams();
    if (q) params.set('q', q);
    params.set('limit', String(limit));
    params.set('offset', String(offset));
    if (refresh) params.set('refresh', '1');
    return request('GET', `/admin/discover?${params.toString()}`);
  },
  groups: () => request('GET', '/admin/groups'),
  createGroup: (name) => request('POST', '/admin/groups', { name, active: true }),
  setGroupActive: (id, active) => request('PATCH', `/admin/groups/${id}`, { active }),
  setGroupBackfill: (id, days, articles) => request('PUT', `/admin/groups/${id}/backfill`, { days, articles }),
  deleteGroup: (id) => request('DELETE', `/admin/groups/${id}`),
  users: () => request('GET', '/admin/users'),
  createUser: (username, password, admin) =>
    request('POST', '/admin/users', { username, password, admin }),
  deleteUser: (id) => request('DELETE', `/admin/users/${id}`),
  triggerScan: (group) => request('POST', '/admin/scan', { group }),
  triggerBackfill: (group) => request('POST', '/admin/backfill', { group }),
  status: () => request('GET', '/admin/status'),
  logs: (level = '', limit = 200) => {
    const params = new URLSearchParams();
    if (level) params.set('level', level);
    params.set('limit', String(limit));
    return request('GET', `/admin/logs?${params.toString()}`);
  },
};
