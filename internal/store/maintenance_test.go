package store

import (
	"context"
	"testing"
)

// TestAnalyzeStatistics verifies ANALYZE runs over the allow-listed tables
// without error on a fresh (empty) schema.
func TestAnalyzeStatistics(t *testing.T) {
	st := freshStore(t)
	if err := st.AnalyzeStatistics(context.Background()); err != nil {
		t.Fatalf("analyze statistics: %v", err)
	}
}

// TestRecentReleaseErrorsAndCount covers the diagnostics failed-release queries
// (#133): a failed release is surfaced with its last error, and the counts
// distinguish permanent from retryable.
func TestRecentReleaseErrorsAndCount(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	mk := func(hash, guid, name string) int64 {
		r, _, err := st.CreateRelease(ctx, ReleaseInput{
			GUID: guid, Name: name, SearchName: name, ReleaseHash: hash,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	// Two failed releases: one retryable, one permanent.
	r1 := mk("h1", "g1", "Retryable Release")
	r2 := mk("h2", "g2", "Permanent Release")
	if _, err := st.RecordPPFailure(ctx, r1, false, "article missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordPPFailure(ctx, r2, true, "auth rejected"); err != nil {
		t.Fatal(err)
	}

	errs, err := st.RecentReleaseErrors(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 2 {
		t.Fatalf("recent release errors = %d, want 2", len(errs))
	}
	// Each carries its last error and permanence.
	byGUID := map[string]ReleaseError{}
	for _, e := range errs {
		byGUID[e.GUID] = e
	}
	if byGUID["g1"].LastError != "article missing" || byGUID["g1"].Permanent {
		t.Errorf("g1 = %+v, want retryable with 'article missing'", byGUID["g1"])
	}
	if !byGUID["g2"].Permanent {
		t.Errorf("g2 should be permanent, got %+v", byGUID["g2"])
	}

	total, perm, err := st.CountFailedReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || perm != 1 {
		t.Errorf("counts = %d total / %d permanent, want 2/1", total, perm)
	}
}

// TestVerifyBackupReadiness verifies the read-only backup-readiness probe finds
// all key tables reachable and a positive database size.
func TestVerifyBackupReadiness(t *testing.T) {
	st := freshStore(t)
	br, err := st.VerifyBackupReadiness(context.Background())
	if err != nil {
		t.Fatalf("verify backup readiness: %v", err)
	}
	if !br.OK {
		t.Errorf("backup readiness OK = false, want true (tables=%d)", br.Tables)
	}
	if br.Tables != len(capacityTables) {
		t.Errorf("tables reachable = %d, want %d", br.Tables, len(capacityTables))
	}
	if br.DatabaseBytes <= 0 {
		t.Errorf("database bytes = %d, want > 0", br.DatabaseBytes)
	}
}
