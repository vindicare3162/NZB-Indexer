// Server-Sent Events client for the admin live updates stream (#121).
// Connects to GET /api/v1/admin/events (which authenticates via the ?token=
// query parameter since EventSource cannot set an Authorization header) and
// dispatches parsed "status" and "log" events to callbacks. Falls back to
// polling: the caller enables its polling timer when onDown fires and disables
// it when onUp fires.
//
// The EventSource constructor is injectable for unit tests.

// buildEventsUrl returns the SSE endpoint URL with the session token attached.
// Exported for testing.
export function buildEventsUrl(token, base = '/api/v1') {
  const t = token ? `?token=${encodeURIComponent(token)}` : '';
  return `${base}/admin/events${t}`;
}

// parseEventData safely JSON-parses an SSE event's data field; returns null on
// bad/empty input rather than throwing. Exported for testing.
export function parseEventData(data) {
  if (!data) return null;
  try {
    return JSON.parse(data);
  } catch {
    return null;
  }
}

// connectAdminEvents opens the stream and wires callbacks. Returns a controller
// with close(). Options:
//   token        session token (required for auth)
//   onStatus(fn) called with the parsed status snapshot
//   onLog(fn)    called with each parsed log entry
//   onUp()       called when the connection opens (disable polling fallback)
//   onDown()     called when the connection errors/closes (enable polling)
//   EventSourceCtor  injectable constructor (defaults to window.EventSource)
export function connectAdminEvents(opts = {}) {
  const {
    token,
    onStatus,
    onLog,
    onUp,
    onDown,
    EventSourceCtor = typeof EventSource !== 'undefined' ? EventSource : null,
  } = opts;

  if (!EventSourceCtor) {
    // No SSE support in this environment: stay on polling.
    if (onDown) onDown();
    return { close() {}, supported: false };
  }

  let closed = false;
  const es = new EventSourceCtor(buildEventsUrl(token));

  es.addEventListener('open', () => {
    if (!closed && onUp) onUp();
  });
  es.addEventListener('status', (e) => {
    if (closed) return;
    const s = parseEventData(e.data);
    if (s != null && onStatus) onStatus(s);
  });
  es.addEventListener('log', (e) => {
    if (closed) return;
    const entry = parseEventData(e.data);
    if (entry != null && onLog) onLog(entry);
  });
  es.addEventListener('error', () => {
    // EventSource auto-reconnects; surface the down state so the caller can
    // resume polling until 'open' fires again.
    if (!closed && onDown) onDown();
  });

  return {
    supported: true,
    close() {
      closed = true;
      es.close();
    },
  };
}
