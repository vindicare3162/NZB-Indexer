package server

import "testing"

func TestComputeBudgetReservesAPIHeadroom(t *testing.T) {
	// Large NNTP budget but a small DB pool: pipeline workers must be clamped so
	// the combined footprint leaves reserved headroom for the API.
	b := computeBudget(20 /*nntp*/, 10 /*db*/, -1 /*auto reserve*/, 0, 0)

	if b.ReservedAPIConns < 1 {
		t.Errorf("expected reserved API headroom on a 10-conn pool, got %d", b.ReservedAPIConns)
	}
	if b.DBPipelineBudget != b.DBMaxConns-b.ReservedAPIConns {
		t.Errorf("pipeline budget %d != db(%d)-reserved(%d)", b.DBPipelineBudget, b.DBMaxConns, b.ReservedAPIConns)
	}
	if b.ScanWorkers+b.PostProcessWorkers > b.DBPipelineBudget {
		t.Errorf("combined workers %d+%d exceed db pipeline budget %d",
			b.ScanWorkers, b.PostProcessWorkers, b.DBPipelineBudget)
	}
	if b.ScanWorkers < 1 || b.PostProcessWorkers < 1 {
		t.Errorf("each stage needs at least one worker, got scan=%d pp=%d", b.ScanWorkers, b.PostProcessWorkers)
	}
	if b.Overcommit {
		t.Error("auto sizing should never overcommit")
	}
}

func TestComputeBudgetTinyDBPool(t *testing.T) {
	// A 2-connection DB pool: reserve 1 for the API, leaving 1 for the whole
	// pipeline. Workers must still be >= 1 (they serialise on the pool) and not
	// deadlock the API out entirely.
	b := computeBudget(10, 2, -1, 0, 0)
	if b.ReservedAPIConns < 1 {
		t.Errorf("2-conn pool should still reserve 1 for API, got %d", b.ReservedAPIConns)
	}
	if b.DBPipelineBudget < 1 {
		t.Fatalf("pipeline budget must be >= 1, got %d", b.DBPipelineBudget)
	}
	if b.ScanWorkers < 1 || b.PostProcessWorkers < 1 {
		t.Errorf("workers must be >= 1, got scan=%d pp=%d", b.ScanWorkers, b.PostProcessWorkers)
	}
}

func TestComputeBudgetLargeDBPoolUsesNNTPHeuristic(t *testing.T) {
	// When the DB pool is ample, sizing follows the NNTP-derived heuristic
	// (scan ~1/2 capped [1,8], pp ~1/2 capped [1,4]).
	b := computeBudget(8, 50, -1, 0, 0)
	if b.ScanWorkers != 4 {
		t.Errorf("scan workers = %d, want 4 (8/2)", b.ScanWorkers)
	}
	if b.PostProcessWorkers != 4 {
		t.Errorf("pp workers = %d, want 4 (8/2 capped at 4)", b.PostProcessWorkers)
	}
	if b.Overcommit {
		t.Error("ample pool should not overcommit")
	}
}

func TestComputeBudgetHonoursExplicitOverrideAndFlagsOvercommit(t *testing.T) {
	// Explicit operator overrides are honoured verbatim even when they exceed
	// the DB pipeline budget, but Overcommit is set so it can be logged.
	b := computeBudget(20, 6, 2 /*reserve*/, 10 /*scan override*/, 5 /*pp override*/)
	if b.ScanWorkers != 10 || b.PostProcessWorkers != 5 {
		t.Errorf("overrides not honoured: scan=%d pp=%d", b.ScanWorkers, b.PostProcessWorkers)
	}
	if b.DBPipelineBudget != 4 {
		t.Errorf("pipeline budget = %d, want 4 (6-2)", b.DBPipelineBudget)
	}
	if !b.Overcommit {
		t.Error("expected Overcommit=true when overrides exceed the DB pipeline budget")
	}
}

func TestComputeBudgetExplicitOverrideWithinBudgetNoOvercommit(t *testing.T) {
	b := computeBudget(20, 20, 4, 3, 2)
	if b.Overcommit {
		t.Error("overrides within budget should not flag overcommit")
	}
	if b.ScanWorkers != 3 || b.PostProcessWorkers != 2 {
		t.Errorf("overrides not honoured: scan=%d pp=%d", b.ScanWorkers, b.PostProcessWorkers)
	}
}

func TestDefaultReservedAPIConns(t *testing.T) {
	cases := []struct{ pool, want int }{
		{1, 0},
		{2, 1},
		{4, 1},
		{8, 2},
		{16, 4},
		{100, 4},
	}
	for _, c := range cases {
		if got := defaultReservedAPIConns(c.pool); got != c.want {
			t.Errorf("defaultReservedAPIConns(%d) = %d, want %d", c.pool, got, c.want)
		}
	}
}

func TestComputeBudgetExplicitReserveCannotStarvePipeline(t *testing.T) {
	// Reserving >= the whole pool is clamped so the pipeline keeps >= 1 conn.
	b := computeBudget(10, 4, 99, 0, 0)
	if b.ReservedAPIConns > b.DBMaxConns-1 {
		t.Errorf("reserved %d must leave at least 1 for pipeline (pool %d)", b.ReservedAPIConns, b.DBMaxConns)
	}
	if b.DBPipelineBudget < 1 {
		t.Errorf("pipeline budget must stay >= 1, got %d", b.DBPipelineBudget)
	}
}
