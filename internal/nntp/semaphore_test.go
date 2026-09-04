package nntp

import (
	"context"
	"testing"
	"time"
)

// TestResizableSemNeverGrantsBeyondCurrentLimit asserts the core invariant
// deterministically: acquire never grants a slot that would make inUse exceed
// the limit in force at grant time. A shrink does not revoke slots already
// held, so inUse can be briefly ABOVE the new (smaller) limit — but it can
// never exceed the limit that was in effect when the slot was granted.
func TestResizableSemNeverGrantsBeyondCurrentLimit(t *testing.T) {
	s := newResizableSem(2)

	// Fill to the limit.
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lim, inUse := s.stats(); inUse != 2 || inUse > lim {
		t.Fatalf("after filling: limit=%d inUse=%d", lim, inUse)
	}

	// A third acquire must not be granted (limit reached).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx); err == nil {
		t.Fatal("acquire granted beyond the current limit")
	}

	// Shrinking below the current holders does not revoke them, and still
	// grants nothing new.
	s.resize(1)
	if _, inUse := s.stats(); inUse != 2 {
		t.Errorf("shrink must not revoke in-flight holders; inUse=%d, want 2", inUse)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := s.acquire(ctx2); err == nil {
		t.Fatal("acquire granted while inUse (2) is already at/over the shrunk limit (1)")
	}

	// Releasing one still leaves inUse (1) == limit (1): no new grant.
	s.release()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel3()
	if err := s.acquire(ctx3); err == nil {
		t.Fatal("acquire granted at the shrunk limit of 1 while a holder remains")
	}

	// Releasing the last holder frees the single slot.
	s.release()
	if err := s.acquire(context.Background()); err != nil {
		t.Fatalf("acquire should succeed once below the shrunk limit: %v", err)
	}
	s.release()
}

// TestResizableSemGrowWakesWaiter verifies growing the limit unblocks a waiter.
func TestResizableSemGrowWakesWaiter(t *testing.T) {
	s := newResizableSem(1)
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		got <- s.acquire(ctx)
	}()

	select {
	case <-got:
		t.Fatal("acquire should block at limit=1")
	case <-time.After(100 * time.Millisecond):
	}

	s.resize(2)
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("acquire after grow: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("grow did not wake the waiter")
	}
}

// TestResizableSemAcquireCancels verifies a blocked acquire returns on context
// cancellation rather than hanging.
func TestResizableSemAcquireCancels(t *testing.T) {
	s := newResizableSem(1)
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx); err == nil {
		t.Fatal("expected acquire to fail when limit reached and ctx cancelled")
	}
}
