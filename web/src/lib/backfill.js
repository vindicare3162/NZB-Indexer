// Validation and payload helpers for per-group backfill target configuration.
//
// Semantics (mirrors the REST contract in internal/api/rest/admin.go):
//   - A field left blank means "no override" -> null in the payload, so the
//     global default applies.
//   - An explicit 0 means "no bound" (unlimited days / unlimited articles).
//   - A positive integer is the explicit target.
// These are kept as pure functions so they can be unit-tested without a DOM.

// parseBackfillField turns a raw string field value into { value, error }.
// `value` is null (blank/no override), a non-negative integer, or undefined on
// error. `error` is a human-readable message or ''.
export function parseBackfillField(raw, label) {
  const s = (raw ?? '').toString().trim();
  if (s === '') {
    return { value: null, error: '' }; // blank -> clear override (use default)
  }
  // Only base-10 integers; reject decimals, signs handled below, and junk.
  if (!/^-?\d+$/.test(s)) {
    return { value: undefined, error: `${label} must be a whole number` };
  }
  const n = parseInt(s, 10);
  if (Number.isNaN(n)) {
    return { value: undefined, error: `${label} must be a whole number` };
  }
  if (n < 0) {
    return { value: undefined, error: `${label} must not be negative` };
  }
  return { value: n, error: '' };
}

// buildBackfillPayload validates both fields and returns
// { payload: { days, articles }, error }. On any validation error, `payload`
// is null and `error` is the first problem found. `days`/`articles` are null
// (clear override), 0 (unlimited), or a positive integer.
export function buildBackfillPayload(daysRaw, articlesRaw) {
  const days = parseBackfillField(daysRaw, 'Days');
  if (days.error) return { payload: null, error: days.error };
  const articles = parseBackfillField(articlesRaw, 'Article limit');
  if (articles.error) return { payload: null, error: articles.error };
  return { payload: { days: days.value, articles: articles.value }, error: '' };
}

// describeBackfillField renders how a stored override value will behave, for
// inline help text.
export function describeBackfillField(value, unit) {
  if (value == null) return 'using global default';
  if (value === 0) return `unlimited ${unit}`;
  return `${value} ${unit}`;
}
