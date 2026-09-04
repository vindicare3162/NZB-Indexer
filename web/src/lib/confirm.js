// A dependency-free confirmation-flow helper for destructive admin actions
// (#122). Replaces blocking window.confirm with an accessible in-page prompt:
// request() records what needs confirming (title/message/labels) and returns a
// Promise that resolves true when the user confirms and false when they cancel.
// The Svelte layer renders the pending request and calls confirm()/cancel().
//
// Usage:
//   const c = createConfirmStore();
//   c.subscribe(pending => { /* render dialog when pending != null */ });
//   if (await c.request({ title: 'Delete group?', message: '…', danger: true })) {
//     // proceed
//   }

export function createConfirmStore() {
  let pending = null; // { title, message, confirmLabel, cancelLabel, danger }
  let resolver = null;
  const subscribers = new Set();

  function emit() {
    for (const fn of subscribers) fn(pending);
  }

  function subscribe(fn) {
    subscribers.add(fn);
    fn(pending);
    return () => subscribers.delete(fn);
  }

  // request shows a confirmation prompt and resolves to a boolean. If a prompt
  // is already pending, the previous one is auto-cancelled (resolves false) so
  // there is never more than one outstanding dialog.
  function request(o = {}) {
    if (resolver) {
      const prev = resolver;
      resolver = null;
      prev(false);
    }
    pending = {
      title: o.title ?? 'Are you sure?',
      message: o.message ?? '',
      confirmLabel: o.confirmLabel ?? 'Confirm',
      cancelLabel: o.cancelLabel ?? 'Cancel',
      danger: !!o.danger,
    };
    emit();
    return new Promise((resolve) => {
      resolver = resolve;
    });
  }

  function settle(result) {
    if (!resolver) return;
    const r = resolver;
    resolver = null;
    pending = null;
    emit();
    r(result);
  }

  const confirm = () => settle(true);
  const cancel = () => settle(false);

  return { subscribe, request, confirm, cancel, get: () => pending };
}
