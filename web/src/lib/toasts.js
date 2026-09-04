// A tiny, dependency-free toast/notification queue for the admin UI (#122).
// Each action can push a success or error toast; toasts auto-expire and can be
// dismissed. The store is framework-agnostic so it can be unit-tested with
// Vitest and driven by Svelte via a small subscribe wrapper.
//
// Usage:
//   const toasts = createToastStore();
//   const id = toasts.push({ kind: 'success', message: 'Saved' });
//   toasts.dismiss(id);
//   toasts.subscribe(list => { /* render */ });

let seq = 0;

// Valid toast kinds. 'info' is the default.
const KINDS = new Set(['success', 'error', 'info']);

export function createToastStore(opts = {}) {
  // Default auto-dismiss delays per kind (ms). 0 or negative means sticky.
  const defaults = {
    success: opts.successMs ?? 4000,
    info: opts.infoMs ?? 4000,
    // Errors linger longer so they're not missed.
    error: opts.errorMs ?? 8000,
  };
  // Injectable timer + clock for deterministic tests.
  const setTimer = opts.setTimeout ?? ((fn, ms) => setTimeout(fn, ms));
  const clearTimer = opts.clearTimeout ?? ((h) => clearTimeout(h));

  let toasts = [];
  const subscribers = new Set();
  const timers = new Map();

  function emit() {
    for (const fn of subscribers) fn(toasts);
  }

  function subscribe(fn) {
    subscribers.add(fn);
    fn(toasts);
    return () => subscribers.delete(fn);
  }

  // push adds a toast and returns its id. `duration` overrides the per-kind
  // default; pass 0 for a sticky toast.
  function push({ kind = 'info', message = '', duration } = {}) {
    const k = KINDS.has(kind) ? kind : 'info';
    const id = ++seq;
    const t = { id, kind: k, message: String(message) };
    toasts = [...toasts, t];
    emit();
    const ms = duration != null ? duration : defaults[k];
    if (ms > 0) {
      timers.set(id, setTimer(() => dismiss(id), ms));
    }
    return id;
  }

  // Convenience helpers.
  const success = (message, duration) => push({ kind: 'success', message, duration });
  const error = (message, duration) => push({ kind: 'error', message, duration });
  const info = (message, duration) => push({ kind: 'info', message, duration });

  function dismiss(id) {
    const before = toasts.length;
    toasts = toasts.filter((t) => t.id !== id);
    const h = timers.get(id);
    if (h != null) {
      clearTimer(h);
      timers.delete(id);
    }
    if (toasts.length !== before) emit();
  }

  function clear() {
    for (const h of timers.values()) clearTimer(h);
    timers.clear();
    if (toasts.length > 0) {
      toasts = [];
      emit();
    }
  }

  return { subscribe, push, success, error, info, dismiss, clear, get: () => toasts };
}
