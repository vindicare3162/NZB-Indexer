import { describe, it, expect } from 'vitest';
import { createConfirmStore } from './confirm.js';

describe('createConfirmStore', () => {
  it('resolves true on confirm', async () => {
    const c = createConfirmStore();
    const p = c.request({ title: 'Delete?', message: 'gone', danger: true });
    expect(c.get()).toMatchObject({ title: 'Delete?', message: 'gone', danger: true });
    c.confirm();
    await expect(p).resolves.toBe(true);
    expect(c.get()).toBeNull();
  });

  it('resolves false on cancel', async () => {
    const c = createConfirmStore();
    const p = c.request({ title: 'Delete?' });
    c.cancel();
    await expect(p).resolves.toBe(false);
    expect(c.get()).toBeNull();
  });

  it('applies sensible defaults', () => {
    const c = createConfirmStore();
    c.request({});
    expect(c.get()).toMatchObject({
      title: 'Are you sure?',
      confirmLabel: 'Confirm',
      cancelLabel: 'Cancel',
      danger: false,
    });
  });

  it('auto-cancels a previous pending request when a new one arrives', async () => {
    const c = createConfirmStore();
    const first = c.request({ title: 'first' });
    const second = c.request({ title: 'second' });
    await expect(first).resolves.toBe(false);
    expect(c.get()).toMatchObject({ title: 'second' });
    c.confirm();
    await expect(second).resolves.toBe(true);
  });

  it('notifies subscribers of pending changes', async () => {
    const c = createConfirmStore();
    const seen = [];
    c.subscribe((p) => seen.push(p ? p.title : null));
    const p = c.request({ title: 'x' });
    c.confirm();
    await p;
    expect(seen).toEqual([null, 'x', null]);
  });

  it('confirm/cancel with nothing pending is a no-op', () => {
    const c = createConfirmStore();
    expect(() => c.confirm()).not.toThrow();
    expect(() => c.cancel()).not.toThrow();
    expect(c.get()).toBeNull();
  });
});
