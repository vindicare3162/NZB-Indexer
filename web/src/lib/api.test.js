import { describe, it, expect, vi, beforeAll, beforeEach } from 'vitest';

// The api module uses localStorage and fetch.  Vitest in node has neither,
// so we provide stubs before any module is imported.
let storage = {};
beforeAll(() => {
  vi.stubGlobal('localStorage', {
    getItem: (key) => storage[key] ?? null,
    setItem: (key, value) => { storage[key] = value; },
    removeItem: (key) => { delete storage[key]; },
    clear: () => { storage = {}; },
  });
});

let fetchMock;
beforeEach(() => {
  storage = {};
  fetchMock = vi.fn();
  globalThis.fetch = fetchMock;
});

describe('api.login', () => {
  it('returns token/username/role on 200', async () => {
    fetchMock.mockResolvedValueOnce(new Response(
      JSON.stringify({ token: 'jwt...', username: 'admin', role: 'admin' }),
      { status: 200, headers: { 'Content-Type': 'application/json' } }
    ));

    const { api } = await import('./api.js');
    const res = await api.login('admin', 'pass');

    expect(res.token).toBe('jwt...');
    expect(res.username).toBe('admin');
    expect(res.role).toBe('admin');

    // Verify the request was POST /api/v1/login with correct body.
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toContain('/api/v1/login');
    expect(opts.method).toBe('POST');
    expect(JSON.parse(opts.body)).toEqual({ username: 'admin', password: 'pass' });
  });

  it('throws with server error message on 401 with JSON body', async () => {
    fetchMock.mockResolvedValueOnce(new Response(
      JSON.stringify({ error: 'Invalid credentials' }),
      { status: 401, headers: { 'Content-Type': 'application/json' } }
    ));

    const { api } = await import('./api.js');
    let err;
    try { await api.login('admin', 'wrong'); } catch (e) { err = e; }

    expect(err).toBeTruthy();
    expect(err.message).toBe('Invalid credentials');
    expect(err.status).toBe(401);
    // The token should have been cleared on 401.
    expect(localStorage.getItem('goindex_token')).toBeFalsy();
  });

  it('throws "Not authenticated" on 401 with no JSON body', async () => {
    fetchMock.mockResolvedValueOnce(new Response(
      'Unauthorized',
      { status: 401 }
    ));

    const { api } = await import('./api.js');
    let err;
    try { await api.login('admin', 'wrong'); } catch (e) { err = e; }

    expect(err).toBeTruthy();
    expect(err.message).toBe('Not authenticated');
    expect(err.status).toBe(401);
  });

  it('throws on network error with the error message', async () => {
    fetchMock.mockRejectedValueOnce(new Error('NetworkError'));

    const { api } = await import('./api.js');
    let err;
    try { await api.login('admin', 'pass'); } catch (e) { err = e; }

    expect(err).toBeTruthy();
    // Network errors are not caught by the 401 branch, they bubble as ApiError
    // from the !res.ok branch — but a network rejection never reaches res.ok.
    // Vitest catches the rejection but we want to see the actual error.
    // Actually a fetch rejection throws a TypeError, which should be caught
    // by the catch in api.js → let's verify the promise rejects.
    expect(err.message).toMatch(/NetworkError/);
  });
});

describe('api.me', () => {
  it('sends stored Bearer token', async () => {
    localStorage.setItem('goindex_token', 'test-jwt');
    fetchMock.mockResolvedValueOnce(new Response(
      JSON.stringify({ username: 'admin', role: 'admin', user_id: 1 }),
      { status: 200, headers: { 'Content-Type': 'application/json' } }
    ));

    const { api } = await import('./api.js');
    const res = await api.me();

    expect(res.username).toBe('admin');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(opts.headers['Authorization']).toBe('Bearer test-jwt');
  });
});

describe('api.setupStatus', () => {
  it('returns setup_required from server', async () => {
    fetchMock.mockResolvedValueOnce(new Response(
      JSON.stringify({ setup_required: false }),
      { status: 200, headers: { 'Content-Type': 'application/json' } }
    ));

    const { api } = await import('./api.js');
    const res = await api.setupStatus();

    expect(res.setup_required).toBe(false);
  });
});

describe('api.search', () => {
  it('sends query params', async () => {
    fetchMock.mockResolvedValueOnce(new Response(
      JSON.stringify({ releases: [], total: 0 }),
      { status: 200, headers: { 'Content-Type': 'application/json' } }
    ));

    const { api } = await import('./api.js');
    await api.search({ q: 'test', cat: '2000', limit: 25 });

    const [url] = fetchMock.mock.calls[0];
    expect(url).toContain('q=test');
    expect(url).toContain('cat=2000');
    expect(url).toContain('limit=25');
  });
});