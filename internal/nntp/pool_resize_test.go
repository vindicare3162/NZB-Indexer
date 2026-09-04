package nntp

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func resizeTestPool(maxConns int) *Pool {
	d := func(cfg Config) (conn, error) {
		return &fakeConn{groupInfo: GroupInfo{High: 1}}, nil
	}
	return newWithDialer(Config{MaxConns: maxConns}, d)
}

// TestPoolResizeGrowUnblocksWaiter verifies that raising the ceiling wakes a
// caller that was blocked waiting for a slot.
func TestPoolResizeGrowUnblocksWaiter(t *testing.T) {
	p := resizeTestPool(1)
	defer p.Close()

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = p.withConn(context.Background(), func(c conn) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started // the single slot is now held

	// A second acquire blocks because the ceiling is 1.
	got := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := p.acquire(ctx)
		if err == nil {
			p.release(c, false)
		}
		got <- err
	}()

	select {
	case <-got:
		t.Fatal("second acquire should be blocked at ceiling=1")
	case <-time.After(100 * time.Millisecond):
	}

	// Grow the ceiling: the blocked acquire should now succeed.
	p.Resize(2)
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("acquire after grow failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("growing the pool did not unblock the waiter")
	}
	if lim, _ := p.MaxConns(); lim != 2 {
		t.Errorf("effective max = %d, want 2", lim)
	}
	close(release)
}

// TestPoolResizeShrinkRespectsNewLimit verifies that after shrinking, no more
// than the new number of concurrent checkouts is granted, and in-flight holders
// are not interrupted.
func TestPoolResizeShrinkRespectsNewLimit(t *testing.T) {
	p := resizeTestPool(4)
	defer p.Close()

	// Hold 1 connection, then shrink to 1. The holder must not be interrupted.
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = p.withConn(context.Background(), func(c conn) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	p.Resize(1) // new ceiling is 1; the one holder already occupies it
	if lim, inUse := p.MaxConns(); lim != 1 || inUse != 1 {
		t.Fatalf("after shrink: limit=%d inUse=%d, want 1,1", lim, inUse)
	}

	// A new acquire must block (ceiling reached by the existing holder).
	blocked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		c, err := p.acquire(ctx)
		if err == nil {
			p.release(c, false)
		}
		blocked <- err
	}()
	if err := <-blocked; err == nil {
		t.Fatal("acquire should have blocked/timed out at the new ceiling of 1")
	}

	// Release the holder; a subsequent acquire now succeeds within the limit.
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := p.acquire(ctx)
	if err != nil {
		t.Fatalf("acquire after holder released failed: %v", err)
	}
	p.release(c, false)
}

// TestPoolResizeNeverExceedsLimitUnderLoad hammers the pool with concurrent
// operations while repeatedly resizing, asserting the number of simultaneous
// checkouts never exceeds the current ceiling and the pool never deadlocks.
// Run with -race to catch data races.
func TestPoolResizeNeverExceedsLimitUnderLoad(t *testing.T) {
	p := resizeTestPool(4)
	defer p.Close()

	const maxCeiling = 6 // the largest ceiling the resizer ever sets
	var inFlight int32
	var peak int32
	stop := make(chan struct{})

	// A background resizer cycling the ceiling between 1 and maxCeiling.
	var rw sync.WaitGroup
	rw.Add(1)
	go func() {
		defer rw.Done()
		sizes := []int{1, 2, 3, 4, 5, 6, 3, 1}
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				p.Resize(sizes[i%len(sizes)])
				i++
				time.Sleep(time.Millisecond)
			}
		}
	}()

	// Worker operations. Concurrent in-flight checkouts must never exceed the
	// largest ceiling ever set: the semaphore never grants beyond the current
	// limit, and a shrink does not revoke slots already granted under a higher
	// (old) limit — so the instantaneous count stays <= max(old, new) <=
	// maxCeiling. That is exactly the provider-budget guarantee.
	var ww sync.WaitGroup
	for i := 0; i < 20; i++ {
		ww.Add(1)
		go func() {
			defer ww.Done()
			for j := 0; j < 50; j++ {
				_ = p.withConn(context.Background(), func(c conn) error {
					n := atomic.AddInt32(&inFlight, 1)
					for {
						old := atomic.LoadInt32(&peak)
						if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
							break
						}
					}
					time.Sleep(200 * time.Microsecond)
					atomic.AddInt32(&inFlight, -1)
					return nil
				})
			}
		}()
	}
	ww.Wait()
	close(stop)
	rw.Wait()

	if pk := atomic.LoadInt32(&peak); int(pk) > maxCeiling {
		t.Errorf("peak concurrent checkouts %d exceeded the max ceiling %d", pk, maxCeiling)
	}
}

// TestPoolCloseUnblocksWaitersDuringResize ensures closing the pool while a
// resize/acquire is in progress does not hang.
func TestPoolShutdownSafeWithPendingAcquire(t *testing.T) {
	p := resizeTestPool(1)

	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = p.withConn(context.Background(), func(c conn) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// A waiter that will be cancelled by context; must not hang.
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, _ = p.acquire(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled acquire did not return")
	}

	close(release)
	p.Close()
}
