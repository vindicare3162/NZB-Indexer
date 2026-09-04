package logbuf

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestBufferCaptureAndOrder(t *testing.T) {
	buf := New(10)
	logger := slog.New(buf.NewHandler())

	logger.Info("first")
	logger.Warn("second", "key", "val")
	logger.Error("third")

	entries := buf.Recent(0, nil) // all, newest first
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	if entries[0].Message != "third" || entries[2].Message != "first" {
		t.Errorf("order wrong (want newest first): %q .. %q", entries[0].Message, entries[2].Message)
	}
	if entries[1].Attrs["key"] != "val" {
		t.Errorf("attr not captured: %+v", entries[1].Attrs)
	}
}

func TestBufferRingEviction(t *testing.T) {
	buf := New(3)
	logger := slog.New(buf.NewHandler())
	for i := 0; i < 5; i++ {
		logger.Info("m", "n", i)
	}
	entries := buf.Recent(0, nil)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3 (capacity)", len(entries))
	}
	// Newest first: the last three writes were n=4,3,2.
	if entries[0].Attrs["n"] != "4" || entries[2].Attrs["n"] != "2" {
		t.Errorf("ring eviction wrong: %q .. %q", entries[0].Attrs["n"], entries[2].Attrs["n"])
	}
}

func TestBufferLevelFilter(t *testing.T) {
	buf := New(10)
	logger := slog.New(buf.NewHandler())
	logger.Debug("d")
	logger.Info("i")
	logger.Warn("w")
	logger.Error("e")

	warn := slog.LevelWarn
	entries := buf.Recent(0, &warn)
	if len(entries) != 2 {
		t.Fatalf("warn+ entries = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Message == "d" || e.Message == "i" {
			t.Errorf("level filter leaked %q", e.Message)
		}
	}
}

func TestBufferLimit(t *testing.T) {
	buf := New(100)
	logger := slog.New(buf.NewHandler())
	for i := 0; i < 20; i++ {
		logger.Info("m")
	}
	if got := buf.Recent(5, nil); len(got) != 5 {
		t.Errorf("limited entries = %d, want 5", len(got))
	}
}

func TestMultiHandlerFansOut(t *testing.T) {
	b1 := New(10)
	b2 := New(10)
	multi := NewMultiHandler(b1.NewHandler(), b2.NewHandler())
	logger := slog.New(multi)

	logger.Info("shared")

	if len(b1.Recent(0, nil)) != 1 || len(b2.Recent(0, nil)) != 1 {
		t.Error("multi handler did not fan out to both buffers")
	}
}

func TestWithAttrsAndGroupShareRing(t *testing.T) {
	buf := New(10)
	base := slog.New(buf.NewHandler())
	child := base.With("component", "scanner").WithGroup("scan")

	base.Info("base msg")
	child.Info("child msg", "group", "g1")

	entries := buf.Recent(0, nil)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (derived handlers share the ring)", len(entries))
	}
	// Newest first: child msg with prefixed group attr + component attr.
	c := entries[0]
	if c.Attrs["component"] != "scanner" {
		t.Errorf("WithAttrs attr missing: %+v", c.Attrs)
	}
	if c.Attrs["scan.group"] != "g1" {
		t.Errorf("WithGroup prefix missing: %+v", c.Attrs)
	}
}

// ensure Handler satisfies slog.Handler.
var _ slog.Handler = (*Handler)(nil)
var _ slog.Handler = (*MultiHandler)(nil)

func TestEnabledAlways(t *testing.T) {
	buf := New(1)
	h := buf.NewHandler()
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("buffer handler should capture all levels")
	}
}

func TestSubscribeReceivesNewEntries(t *testing.T) {
	buf := New(10)
	logger := slog.New(buf.NewHandler())

	ch, cancel := buf.Subscribe()
	defer cancel()

	logger.Info("live one")
	logger.Warn("live two")

	got := []string{}
	for i := 0; i < 2; i++ {
		select {
		case e := <-ch:
			got = append(got, e.Message)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live entry %d", i)
		}
	}
	if got[0] != "live one" || got[1] != "live two" {
		t.Errorf("live entries = %v, want [live one live two]", got)
	}
}

func TestSubscribeCancelStopsAndCloses(t *testing.T) {
	buf := New(10)
	logger := slog.New(buf.NewHandler())

	ch, cancel := buf.Subscribe()
	cancel()

	// After cancel the channel is closed; a receive returns the zero value with ok=false.
	if _, ok := <-ch; ok {
		t.Error("expected channel to be closed after cancel")
	}
	// Further log writes must not panic (subscriber already removed).
	logger.Info("after cancel")
	// A second cancel is a no-op.
	cancel()
}

func TestSubscribeDoesNotBlockWhenFull(t *testing.T) {
	buf := New(10)
	logger := slog.New(buf.NewHandler())

	// Subscribe but never drain: the buffered channel (cap 256) fills and then
	// further entries are dropped rather than blocking log capture.
	_, cancel := buf.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			logger.Info("flood")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("log capture blocked on a full subscriber (should drop)")
	}
}

func TestMultipleSubscribersEachReceive(t *testing.T) {
	buf := New(10)
	logger := slog.New(buf.NewHandler())

	a, ca := buf.Subscribe()
	defer ca()
	b, cb := buf.Subscribe()
	defer cb()

	logger.Info("broadcast")

	for _, ch := range []<-chan Entry{a, b} {
		select {
		case e := <-ch:
			if e.Message != "broadcast" {
				t.Errorf("got %q, want broadcast", e.Message)
			}
		case <-time.After(time.Second):
			t.Fatal("a subscriber did not receive the broadcast")
		}
	}
}
