package server

// Resource budgeting (#117).
//
// The pipeline draws on two independent, differently-sized pools:
//
//   - the NNTP connection pool (bounded by the provider's connection limit), and
//   - the PostgreSQL connection pool (bounded by database.max_conns).
//
// NNTP-heavy stages (scan, post-process) also perform database work, and the
// HTTP API + admin control plane contend for the SAME PostgreSQL pool. If we
// size pipeline concurrency purely off the NNTP budget, a small database pool
// can be fully consumed by pipeline workers, leaving searches and admin
// requests to block on connection acquisition and the service to appear hung.
//
// resourceBudget derives worker counts that respect BOTH pools at once and
// always reserve headroom on the database pool for API/control-plane traffic.

// ResourceBudget is the computed concurrency plan for the NNTP-bound pipeline
// stages, derived from both the effective NNTP capacity and the database pool
// capacity minus reserved API headroom.
type ResourceBudget struct {
	// NNTPMaxConns is the effective NNTP connection budget the plan was sized
	// against.
	NNTPMaxConns int
	// DBMaxConns is the total PostgreSQL pool size.
	DBMaxConns int
	// ReservedAPIConns is the number of database connections held back for the
	// HTTP API and admin control plane so pipeline load cannot starve them.
	ReservedAPIConns int
	// DBPipelineBudget is the number of database connections the pipeline may
	// use concurrently (DBMaxConns - ReservedAPIConns, floored at 1).
	DBPipelineBudget int
	// ScanWorkers and PostProcessWorkers are the per-stage worker counts. Both
	// stages draw NNTP AND database connections, so their combined footprint is
	// held within DBPipelineBudget.
	ScanWorkers        int
	PostProcessWorkers int
	// Overcommit is true when an explicit operator override was honoured even
	// though the combined worker count exceeds the database pipeline budget
	// (potentially unsafe; logged so operators can see it).
	Overcommit bool
}

// defaultReservedAPIConns picks how many DB connections to hold back for the
// API/control plane, given the pool size: about a quarter of the pool, bounded
// to [1, 4]. Small pools still reserve at least one connection.
func defaultReservedAPIConns(dbMaxConns int) int {
	if dbMaxConns <= 1 {
		return 0 // nothing sensible to reserve from a 1-conn pool
	}
	n := dbMaxConns / 4
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	// Never reserve the whole pool.
	if n >= dbMaxConns {
		n = dbMaxConns - 1
	}
	return n
}

// computeBudget derives the pipeline concurrency plan.
//
//   - nntpMaxConns: effective NNTP connection budget (from #104).
//   - dbMaxConns: PostgreSQL pool size (database.max_conns).
//   - reservedAPIConns: DB connections to hold back for API/control plane; a
//     negative value means "auto" (defaultReservedAPIConns).
//   - scanOverride/ppOverride: operator overrides (>0 honoured verbatim, even if
//     unsafe; <=0 means auto-derive).
//
// Auto-derivation splits the smaller of the NNTP budget and the DB pipeline
// budget between scanning and post-processing, matching the historical
// heuristic (scan ~1/2 of NNTP capped [1,8]; pp ~1/2 capped [1,4]) but then
// clamps the COMBINED worker count so it never exceeds the DB pipeline budget.
func computeBudget(nntpMaxConns, dbMaxConns, reservedAPIConns, scanOverride, ppOverride int) ResourceBudget {
	if nntpMaxConns < 1 {
		nntpMaxConns = 1
	}
	if dbMaxConns < 1 {
		dbMaxConns = 1
	}
	if reservedAPIConns < 0 {
		reservedAPIConns = defaultReservedAPIConns(dbMaxConns)
	}
	if reservedAPIConns > dbMaxConns-1 {
		// Always leave at least one connection for the pipeline.
		reservedAPIConns = dbMaxConns - 1
	}
	if reservedAPIConns < 0 {
		reservedAPIConns = 0
	}
	dbPipeline := dbMaxConns - reservedAPIConns
	if dbPipeline < 1 {
		dbPipeline = 1
	}

	b := ResourceBudget{
		NNTPMaxConns:     nntpMaxConns,
		DBMaxConns:       dbMaxConns,
		ReservedAPIConns: reservedAPIConns,
		DBPipelineBudget: dbPipeline,
	}

	// Auto-derived defaults from the NNTP budget (historical heuristic).
	autoScan := clamp(nntpMaxConns/2, 1, 8)
	autoPP := clamp(nntpMaxConns/2, 1, 4)

	scan := scanOverride
	if scan <= 0 {
		scan = autoScan
	}
	pp := ppOverride
	if pp <= 0 {
		pp = autoPP
	}

	explicit := scanOverride > 0 || ppOverride > 0

	if explicit {
		// Honour operator overrides verbatim, but flag when their combined
		// footprint exceeds the DB pipeline budget (potentially unsafe).
		b.ScanWorkers = scan
		b.PostProcessWorkers = pp
		if scan+pp > dbPipeline {
			b.Overcommit = true
		}
		return b
	}

	// Auto mode: keep the combined pipeline DB footprint within budget by
	// scaling the two stages down proportionally when they would exceed it.
	scan, pp = fitWithinBudget(scan, pp, dbPipeline)
	b.ScanWorkers = scan
	b.PostProcessWorkers = pp
	return b
}

// fitWithinBudget reduces scan/pp worker counts so scan+pp <= budget while
// keeping each at least 1 and preserving their relative sizes as far as
// possible. budget is assumed >= 1.
func fitWithinBudget(scan, pp, budget int) (int, int) {
	if scan < 1 {
		scan = 1
	}
	if pp < 1 {
		pp = 1
	}
	if scan+pp <= budget {
		return scan, pp
	}
	if budget <= 1 {
		return 1, 1 // both stages need at least one; NNTP pool still serialises
	}
	if budget == 2 {
		return 1, 1
	}
	// Give scanning the larger share (it fans out across groups), leaving the
	// rest to post-processing, both at least 1.
	scanShare := budget * scan / (scan + pp)
	if scanShare < 1 {
		scanShare = 1
	}
	ppShare := budget - scanShare
	if ppShare < 1 {
		ppShare = 1
		scanShare = budget - 1
	}
	return scanShare, ppShare
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
