package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Scale and pool-pressure tests for ingestion (#135). These exercise COPY-based
// ingestion across many groups concurrently while read-side "API" queries run
// against the same pool, confirming there is no deadlock, the data is correct,
// and the pipeline stays consistent under contention.

// TestConcurrentIngestionAcrossManyGroups loads parts into 50 groups in
// parallel while continuously running stats/search/group-listing queries, then
// asserts every part landed and assembly still works.
func TestConcurrentIngestionAcrossManyGroups(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	const groups = 50
	const partsPerGroup = 40

	// Pre-create groups so ingestion goroutines only insert parts.
	ids := make([]int64, groups)
	for i := 0; i < groups; i++ {
		g, err := st.UpsertGroup(ctx, fmt.Sprintf("alt.binaries.scale.%03d", i), true)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = g.ID
	}

	// Reader goroutines simulate API/admin traffic during ingestion: they hit
	// the pool with stats, search, and group-listing queries in a tight loop
	// until ingestion completes.
	stop := make(chan struct{})
	var readWG sync.WaitGroup
	for r := 0; r < 4; r++ {
		readWG.Add(1)
		go func() {
			defer readWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = st.PipelineStatistics(ctx)
				_, _, _ = st.SearchReleases(ctx, SearchFilter{Limit: 20})
				_, _ = st.ListGroupsPage(ctx, GroupFilter{Limit: 25})
			}
		}()
	}

	// Ingest concurrently across all groups.
	var ingestWG sync.WaitGroup
	errCh := make(chan error, groups)
	for i := 0; i < groups; i++ {
		ingestWG.Add(1)
		go func(gi int) {
			defer ingestWG.Done()
			batch := make([]PartInput, partsPerGroup)
			for p := 0; p < partsPerGroup; p++ {
				art := int64(gi*10000 + p)
				norm := fmt.Sprintf("scalefile.%03d.%03d", gi, p)
				batch[p] = mkPart(ids[gi], art, fmt.Sprintf("m%d-%d@x", gi, p), norm)
			}
			if _, err := st.InsertParts(ctx, batch); err != nil {
				errCh <- err
			}
		}(i)
	}
	ingestWG.Wait()
	close(stop)
	readWG.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent ingestion error: %v", err)
	}

	// Every part must have landed exactly once.
	var total int64
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM parts`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if want := int64(groups * partsPerGroup); total != want {
		t.Errorf("parts count = %d, want %d", total, want)
	}

	// Assembly over the full backlog still succeeds after concurrent load.
	if _, err := st.AssembleBinaries(ctx, 10000); err != nil {
		t.Fatalf("assemble after concurrent ingestion: %v", err)
	}
}

// TestReIngestionUnderConcurrencyIsIdempotent hammers the same group with
// overlapping COPY loads of an identical batch from multiple goroutines and
// asserts the ON CONFLICT DO NOTHING path keeps the row count stable (no
// duplicate rows, no errors) under contention.
func TestReIngestionUnderConcurrencyIsIdempotent(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, err := st.UpsertGroup(ctx, "alt.binaries.reingest", true)
	if err != nil {
		t.Fatal(err)
	}

	const n = 30
	batch := make([]PartInput, n)
	for i := 0; i < n; i++ {
		batch[i] = mkPart(g.ID, int64(i), fmt.Sprintf("re%d@x", i), fmt.Sprintf("refile.%03d", i))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.InsertParts(ctx, batch); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent re-ingestion error: %v", err)
	}

	var total int64
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM parts WHERE group_id = $1`, g.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != int64(n) {
		t.Errorf("parts after concurrent re-ingestion = %d, want %d (idempotent)", total, n)
	}
}
