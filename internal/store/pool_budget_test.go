package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestConstrainedPoolQueuesWithoutDeadlock proves the core premise of the
// resource-budgeting work (#117): with a small PostgreSQL pool, work that
// exceeds the pool size QUEUES on connection acquisition and is served in turn,
// rather than deadlocking or permanently starving a late "API" request.
//
// It opens a 3-connection pool, saturates it with 3 long-held transactions
// (standing in for pipeline workers), then issues an additional short query
// (standing in for an API/control-plane request). The extra query must block
// only until a worker releases its connection, then succeed within a bounded
// time — demonstrating queuing, not deadlock.
func TestConstrainedPoolQueuesWithoutDeadlock(t *testing.T) {
	dsn := testDSN(t)
	if err := MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	const poolSize = 3
	ctx := context.Background()
	st, err := Open(ctx, dsn, poolSize)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	pool := st.Pool()

	// Saturate the pool: acquire every connection and hold it briefly.
	held := make([]interface{ Release() }, 0, poolSize)
	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		held = append(held, c)
	}

	// The pool is now empty. Fire the "API" query; it must block on acquisition.
	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		qctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var n int
		done <- st.pool.QueryRow(qctx, "SELECT 1").Scan(&n)
	}()

	// It should not have completed yet (pool is saturated).
	select {
	case err := <-done:
		t.Fatalf("query unexpectedly completed while pool saturated (err=%v); acquisition did not queue", err)
	case <-time.After(150 * time.Millisecond):
		// Good: it is queued, waiting for a connection.
	}

	// Release one worker's connection; the queued query should now proceed.
	held[0].Release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued query failed after a connection freed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued query did not complete after a connection was freed: possible deadlock/starvation")
	}

	// Release the rest.
	for _, c := range held[1:] {
		c.Release()
	}
	wg.Wait()
}
