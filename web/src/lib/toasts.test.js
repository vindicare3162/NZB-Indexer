import { describe, it, expect, vi } from 'vitest';
import { createToastStore } from './toasts.js';

function manualTimers() {
  const scheduled = new Map();
  let h = 0;
  return {
    setTimeout: (fn, ms) => {
      const id = ++h;
      scheduled.set(id, { fn, ms });
      return id;
    },
    clearTimeout: (id) => scheduled.delete(id),
    run: (id) => {
      const t = scheduled.get(id);
      if (t) {
        scheduled.delete(id);
        t.fn();
      }
    },
    scheduled,
  };
}

describe('createToastStore', () => {
  it('pushes toasts and notifies subscribers', () => {
    const t = createToastStore();
    const seen = [];
    t.subscribe((list) => seen.push(list.length));
    t.success('saved');
    t.error('boom');
    expect(t.get()).toHaveLength(2);
    expect(t.get()[0]).toMatchObject({ kind: 'success', message: 'saved' });
    expect(t.get()[1]).toMatchObject({ kind: 'error', message: 'boom' });
    // subscriber saw 0 (initial), 1, 2
    expect(seen).toEqual([0, 1, 2]);
  });

  it('defaults unknown kinds to info', () => {
    const t = createToastStore();
    t.push({ kind: 'nonsense', message: 'x' });
    expect(t.get()[0].kind).toBe('info');
  });

  it('auto-dismisses after the per-kind delay', () => {
    const tm = manualTimers();
    const t = createToastStore({ setTimeout: tm.setTimeout, clearTimeout: tm.clearTimeout });
    const id = t.success('saved');
    expect(t.get()).toHaveLength(1);
    // one timer scheduled; fire it
    const [timerId] = [...tm.scheduled.keys()];
    tm.run(timerId);
    expect(t.get()).toHaveLength(0);
    // dismissing an already-gone id is a no-op
    t.dismiss(id);
    expect(t.get()).toHaveLength(0);
  });

  it('sticky toasts (duration 0) do not schedule a timer', () => {
    const tm = manualTimers();
    const t = createToastStore({ setTimeout: tm.setTimeout, clearTimeout: tm.clearTimeout });
    t.push({ kind: 'error', message: 'stick', duration: 0 });
    expect(tm.scheduled.size).toBe(0);
    expect(t.get()).toHaveLength(1);
  });

  it('dismiss clears the pending timer', () => {
    const tm = manualTimers();
    const t = createToastStore({ setTimeout: tm.setTimeout, clearTimeout: tm.clearTimeout });
    const id = t.success('saved');
    expect(tm.scheduled.size).toBe(1);
    t.dismiss(id);
    expect(tm.scheduled.size).toBe(0);
    expect(t.get()).toHaveLength(0);
  });

  it('clear removes all toasts and timers', () => {
    const tm = manualTimers();
    const t = createToastStore({ setTimeout: tm.setTimeout, clearTimeout: tm.clearTimeout });
    t.success('a');
    t.error('b');
    expect(t.get()).toHaveLength(2);
    t.clear();
    expect(t.get()).toHaveLength(0);
    expect(tm.scheduled.size).toBe(0);
  });
});
