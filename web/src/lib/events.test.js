import { describe, it, expect, vi } from 'vitest';
import { buildEventsUrl, parseEventData, connectAdminEvents } from './events.js';

// FakeEventSource lets tests drive the SSE lifecycle without a real connection.
class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.listeners = {};
    FakeEventSource.last = this;
    this.closed = false;
  }
  addEventListener(type, fn) {
    (this.listeners[type] ||= []).push(fn);
  }
  emit(type, data) {
    for (const fn of this.listeners[type] || []) fn({ data });
  }
  close() {
    this.closed = true;
  }
}

describe('buildEventsUrl', () => {
  it('attaches the token as a query param', () => {
    expect(buildEventsUrl('abc')).toBe('/api/v1/admin/events?token=abc');
  });
  it('url-encodes the token', () => {
    expect(buildEventsUrl('a b/c')).toBe('/api/v1/admin/events?token=a%20b%2Fc');
  });
  it('omits the query when no token', () => {
    expect(buildEventsUrl('')).toBe('/api/v1/admin/events');
  });
});

describe('parseEventData', () => {
  it('parses valid JSON', () => {
    expect(parseEventData('{"a":1}')).toEqual({ a: 1 });
  });
  it('returns null for empty or invalid', () => {
    expect(parseEventData('')).toBeNull();
    expect(parseEventData('not json')).toBeNull();
    expect(parseEventData(undefined)).toBeNull();
  });
});

describe('connectAdminEvents', () => {
  it('dispatches status and log events and open/error transitions', () => {
    const onStatus = vi.fn();
    const onLog = vi.fn();
    const onUp = vi.fn();
    const onDown = vi.fn();

    const ctrl = connectAdminEvents({
      token: 'tok',
      onStatus,
      onLog,
      onUp,
      onDown,
      EventSourceCtor: FakeEventSource,
    });
    const es = FakeEventSource.last;
    expect(ctrl.supported).toBe(true);
    expect(es.url).toBe('/api/v1/admin/events?token=tok');

    es.emit('open');
    expect(onUp).toHaveBeenCalledTimes(1);

    es.emit('status', '{"state":"idle"}');
    expect(onStatus).toHaveBeenCalledWith({ state: 'idle' });

    es.emit('log', '{"level":"WARN","message":"x"}');
    expect(onLog).toHaveBeenCalledWith({ level: 'WARN', message: 'x' });

    es.emit('error');
    expect(onDown).toHaveBeenCalledTimes(1);
  });

  it('ignores malformed event data without throwing', () => {
    const onStatus = vi.fn();
    connectAdminEvents({ token: 't', onStatus, EventSourceCtor: FakeEventSource });
    const es = FakeEventSource.last;
    es.emit('status', 'garbage');
    expect(onStatus).not.toHaveBeenCalled();
  });

  it('close() stops further dispatch', () => {
    const onStatus = vi.fn();
    const ctrl = connectAdminEvents({ token: 't', onStatus, EventSourceCtor: FakeEventSource });
    const es = FakeEventSource.last;
    ctrl.close();
    expect(es.closed).toBe(true);
    es.emit('status', '{"state":"running"}');
    expect(onStatus).not.toHaveBeenCalled();
  });

  it('falls back (onDown) when EventSource is unavailable', () => {
    const onDown = vi.fn();
    const ctrl = connectAdminEvents({ token: 't', onDown, EventSourceCtor: null });
    expect(ctrl.supported).toBe(false);
    expect(onDown).toHaveBeenCalledTimes(1);
  });
});
