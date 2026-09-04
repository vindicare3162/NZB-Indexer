// Package logbuf provides a bounded in-memory ring buffer that captures recent
// slog records so they can be surfaced in the admin UI. It is a recent-activity
// view, not durable log retention.
package logbuf

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Entry is a captured log record in a form convenient for JSON serialisation.
type Entry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Buffer is a fixed-capacity, concurrency-safe ring of recent log entries. It
// is the shared storage; log capture goes through a Handler (see NewHandler).
// It also supports live subscription (Subscribe) so new entries can be streamed
// (e.g. to the admin UI via Server-Sent Events, #121).
type Buffer struct {
	mu      sync.Mutex
	entries []Entry
	next    int  // index of the next write position
	full    bool // whether the ring has wrapped
	cap     int

	// subs are live subscribers; each new entry is fanned out to them
	// non-blocking (a slow/full subscriber drops entries rather than stalling
	// log writes). Guarded by subMu (separate from mu so a subscriber's channel
	// send never contends with a Recent read).
	subMu sync.Mutex
	subs  map[int]chan Entry
	subID int
}

// New creates a Buffer holding up to capacity entries. A non-positive capacity
// defaults to 1000.
func New(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = 1000
	}
	return &Buffer{entries: make([]Entry, capacity), cap: capacity, subs: map[int]chan Entry{}}
}

// add appends an entry to the ring, evicting the oldest when full, then fans it
// out to any live subscribers (non-blocking).
func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	b.entries[b.next] = e
	b.next = (b.next + 1) % b.cap
	if b.next == 0 {
		b.full = true
	}
	b.mu.Unlock()
	b.publish(e)
}

// publish delivers an entry to every subscriber without blocking: if a
// subscriber's buffered channel is full (a slow consumer), the entry is dropped
// for that subscriber rather than stalling log capture.
func (b *Buffer) publish(e Entry) {
	b.subMu.Lock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // subscriber is behind; drop to keep logging non-blocking
		}
	}
	b.subMu.Unlock()
}

// Subscribe registers a live subscriber and returns a channel of newly-added
// entries plus a cancel function that unregisters and closes the channel. The
// channel is buffered; a consumer that falls behind will miss entries (they are
// dropped) but never blocks log capture. Always call cancel when done.
func (b *Buffer) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 256)
	b.subMu.Lock()
	id := b.subID
	b.subID++
	b.subs[id] = ch
	b.subMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.subMu.Lock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
			b.subMu.Unlock()
		})
	}
	return ch, cancel
}

// Recent returns up to limit most-recent entries, newest first. A non-positive
// limit returns all buffered entries. Entries below minLevel are skipped when
// minLevel is non-nil.
func (b *Buffer) Recent(limit int, minLevel *slog.Level) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Reconstruct chronological order (oldest -> newest).
	var ordered []Entry
	if b.full {
		ordered = append(ordered, b.entries[b.next:]...)
		ordered = append(ordered, b.entries[:b.next]...)
	} else {
		ordered = append(ordered, b.entries[:b.next]...)
	}

	// Walk newest -> oldest, applying the level filter and limit.
	out := make([]Entry, 0, len(ordered))
	for i := len(ordered) - 1; i >= 0; i-- {
		e := ordered[i]
		if minLevel != nil && levelValue(e.Level) < int(*minLevel) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Handler is an slog.Handler that captures records into a Buffer. It correctly
// carries WithAttrs/WithGroup state, and multiple derived handlers all write to
// the same shared Buffer.
//
// Attribute keys are already qualified with their group prefix at the moment
// they are added (via WithAttrs or on a record), matching slog semantics where
// WithGroup only affects attributes added after it.
type Handler struct {
	buf *Buffer
	// preAttrs holds already-group-qualified key/value pairs accumulated via
	// WithAttrs on ancestor handlers.
	preAttrs map[string]string
	// prefix is the current group prefix applied to attrs added from here on.
	prefix string
}

// NewHandler returns an slog.Handler that captures records into buf.
func (b *Buffer) NewHandler() slog.Handler {
	return &Handler{buf: b}
}

// Enabled captures records at all levels; filtering happens at read time.
func (h *Handler) Enabled(context.Context, slog.Level) bool { return true }

// Handle records the entry, applying the current group prefix to record attrs.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	e := Entry{Time: r.Time, Level: r.Level.String(), Message: r.Message}
	if len(h.preAttrs) > 0 || r.NumAttrs() > 0 {
		e.Attrs = make(map[string]string, len(h.preAttrs)+r.NumAttrs())
		for k, v := range h.preAttrs {
			e.Attrs[k] = v
		}
		r.Attrs(func(a slog.Attr) bool {
			e.Attrs[h.prefix+a.Key] = a.Value.String()
			return true
		})
	}
	h.buf.add(e)
	return nil
}

// WithAttrs returns a derived handler carrying the given attrs, qualified with
// the current group prefix.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make(map[string]string, len(h.preAttrs)+len(attrs))
	for k, v := range h.preAttrs {
		merged[k] = v
	}
	for _, a := range attrs {
		merged[h.prefix+a.Key] = a.Value.String()
	}
	return &Handler{buf: h.buf, preAttrs: merged, prefix: h.prefix}
}

// WithGroup returns a derived handler nested under the given group name. Only
// attributes added after this call are prefixed with the group.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{buf: h.buf, preAttrs: h.preAttrs, prefix: h.prefix + name + "."}
}

// MultiHandler fans a record out to several handlers (e.g. stderr + buffer).
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler composes handlers; a record is dispatched to each.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	return &MultiHandler{handlers: handlers}
}

// Enabled reports true if any wrapped handler is enabled for the level.
func (m *MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle dispatches to every wrapped handler that is enabled for the record.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WithAttrs derives all wrapped handlers.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: next}
}

// WithGroup derives all wrapped handlers.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: next}
}

// levelValue maps a slog level string back to its numeric value for filtering.
func levelValue(s string) int {
	switch {
	case hasPrefix(s, "DEBUG"):
		return int(slog.LevelDebug)
	case hasPrefix(s, "WARN"):
		return int(slog.LevelWarn)
	case hasPrefix(s, "ERROR"):
		return int(slog.LevelError)
	default:
		return int(slog.LevelInfo)
	}
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
