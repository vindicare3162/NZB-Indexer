package store

import (
	"context"
	"testing"
	"time"
)

// prunableRelease builds a fully prunable release: parts -> assembled binary ->
// released, done, durable-segments release posted `age` ago. Returns the
// release id and binary id.
func prunableRelease(t *testing.T, st *Store, groupID int64, norm string, articleBase int64, parts int, age time.Duration) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	seedStatsParts(t, st, groupID, norm, "poster", articleBase, parts, parts)
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	var binID int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT id FROM binaries WHERE norm_subject = $1`, norm).Scan(&binID); err != nil {
		t.Fatalf("find binary %q: %v", norm, err)
	}
	posted := time.Now().Add(-age)
	rel, _, err := st.CreateRelease(ctx, ReleaseInput{
		GUID: norm, Name: norm, SearchName: norm, GroupID: &groupID, BinaryID: &binID,
		ReleaseHash: norm, PostedAt: &posted,
	})
	if err != nil {
		t.Fatalf("create release %q: %v", norm, err)
	}
	// Mark the binary released and the release fully post-processed.
	if err := st.MarkBinariesReleased(ctx, []int64{binID}); err != nil {
		t.Fatalf("mark released: %v", err)
	}
	if err := st.SetReleasePPStatus(ctx, rel.ID, "done"); err != nil {
		t.Fatalf("set pp done: %v", err)
	}
	return rel.ID, binID
}

func partCount(t *testing.T, st *Store, binaryID int64) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM parts WHERE binary_id = $1`, binaryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRetentionCandidatesAndPrune(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.retention", true)

	// A fully prunable release (old, released, done, durable segments).
	oldRelID, oldBinID := prunableRelease(t, st, g.ID, "Old.Prunable.mkv", 1000, 5, 60*24*time.Hour)

	// A release that is released+done but RECENT (inside the window): retained.
	_, recentBinID := prunableRelease(t, st, g.ID, "Recent.Kept.mkv", 2000, 4, 1*time.Hour)

	// A released+durable release still PENDING post-processing: retained.
	pendRelID, pendBinID := prunableRelease(t, st, g.ID, "Pending.Kept.mkv", 3000, 3, 60*24*time.Hour)
	if err := st.SetReleasePPStatus(ctx, pendRelID, "pending"); err != nil {
		t.Fatal(err)
	}

	// An incomplete, unreleased binary (assembler backlog): retained.
	seedStatsParts(t, st, g.ID, "Incomplete.mkv", "poster", 4000, 2, 4)
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}

	cutoff := 30 * 24 * time.Hour // 30 days

	// Dry-run: only the old prunable release's parts are candidates.
	rep, err := st.RetentionCandidates(ctx, cutoff)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if rep.CandidateParts != 5 {
		t.Errorf("candidate parts = %d, want 5 (only the old prunable release)", rep.CandidateParts)
	}
	if rep.CandidateReleases != 1 {
		t.Errorf("candidate releases = %d, want 1", rep.CandidateReleases)
	}
	if rep.CandidateBytes != 5000 {
		t.Errorf("candidate bytes = %d, want 5000 (5 x 1000)", rep.CandidateBytes)
	}
	if rep.Retained.Unassigned == 0 {
		// The incomplete binary's parts remain unassigned only if AssembleBinaries
		// left them so; incomplete single-file collections stay unassigned.
		t.Logf("note: unassigned retained = %d", rep.Retained.Unassigned)
	}
	if rep.Retained.NotReconstructable < 7 {
		// recent(4) + pending(3) assigned-but-not-prunable parts = 7 minimum.
		t.Errorf("not-reconstructable retained = %d, want >= 7", rep.Retained.NotReconstructable)
	}

	// Dry-run must not delete anything.
	if got := partCount(t, st, oldBinID); got != 5 {
		t.Errorf("dry-run deleted parts: old binary now has %d, want 5", got)
	}

	// Verify the prunable release can still generate its NZB from durable
	// segments BEFORE and AFTER pruning.
	segsBefore, err := st.GetReleaseSegments(ctx, oldRelID)
	if err != nil || len(segsBefore) != 5 {
		t.Fatalf("segments before prune = %d (err=%v), want 5", len(segsBefore), err)
	}

	// Prune (bounded batches), then confirm only the candidates were deleted.
	deleted, err := st.PruneRetainedPartsAll(ctx, cutoff, 2 /*batch*/, 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 5 {
		t.Errorf("pruned %d parts, want 5", deleted)
	}
	if got := partCount(t, st, oldBinID); got != 0 {
		t.Errorf("old binary parts after prune = %d, want 0", got)
	}
	if got := partCount(t, st, recentBinID); got != 4 {
		t.Errorf("recent binary parts = %d, want 4 (retained)", got)
	}
	if got := partCount(t, st, pendBinID); got != 3 {
		t.Errorf("pending binary parts = %d, want 3 (retained)", got)
	}

	// NZB safety: the pruned release still resolves its segments from durable
	// storage.
	segsAfter, err := st.GetReleaseSegments(ctx, oldRelID)
	if err != nil {
		t.Fatalf("segments after prune: %v", err)
	}
	if len(segsAfter) != 5 {
		t.Errorf("segments after prune = %d, want 5 (durable segments intact)", len(segsAfter))
	}

	// A second prune is a no-op (resumable/idempotent once drained).
	deleted2, err := st.PruneRetainedPartsAll(ctx, cutoff, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if deleted2 != 0 {
		t.Errorf("second prune deleted %d, want 0", deleted2)
	}
}

func TestPruneRetainedPartsCancellation(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.retention.cancel", true)
	prunableRelease(t, st, g.ID, "Cancel.mkv", 1000, 10, 60*24*time.Hour)

	// A cancelled context stops the batch loop and reports progress so far
	// (0 here, since it's cancelled before any batch).
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	deleted, err := st.PruneRetainedPartsAll(cctx, 30*24*time.Hour, 2, 0)
	if err != nil {
		t.Fatalf("cancelled prune should not error, got %v", err)
	}
	if deleted != 0 {
		t.Errorf("cancelled-before-start prune deleted %d, want 0", deleted)
	}
}
