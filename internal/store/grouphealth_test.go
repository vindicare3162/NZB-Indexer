package store

import (
	"context"
	"testing"
	"time"
)

// TestRecordGroupScanHealthSignals covers the #127 retained signals: success
// timestamps (overall/forward/backfill), consecutive-failure counting and
// recovery, and throughput EMA updates.
func TestRecordGroupScanHealthSignals(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g, err := st.UpsertGroup(ctx, "alt.binaries.health", true)
	if err != nil {
		t.Fatal(err)
	}
	if g.ConsecutiveFailures != 0 || g.ThroughputArtsPerSec != 0 ||
		g.LastSuccessAt != nil || g.LastForwardAt != nil || g.LastBackfillAt != nil {
		t.Errorf("fresh group should have empty health signals, got %+v", g)
	}

	// Successful forward pass: 300 articles in 1s -> 300 arts/sec seeds the EMA.
	if err := st.RecordGroupScan(ctx, g.ID, GroupScanOutcome{
		Articles: 300, Parts: 250, ServerHigh: 9000, DurationMS: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetGroupByName(ctx, g.Name)
	if got.LastSuccessAt == nil || got.LastForwardAt == nil {
		t.Errorf("forward success should set last_success_at and last_forward_at, got %+v", got)
	}
	if got.LastBackfillAt != nil {
		t.Error("forward pass should not set last_backfill_at")
	}
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutive_failures = %d, want 0", got.ConsecutiveFailures)
	}
	if got.ThroughputArtsPerSec != 300 {
		t.Errorf("throughput seed = %v, want 300", got.ThroughputArtsPerSec)
	}

	// Successful backfill pass: 600 arts in 1s = 600 sample. EMA (weight 0.3):
	// 300 + 0.3*(600-300) = 390.
	if err := st.RecordGroupScan(ctx, g.ID, GroupScanOutcome{
		Backfill: true, Articles: 600, Parts: 500, ServerHigh: 9500, DurationMS: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetGroupByName(ctx, g.Name)
	if got.LastBackfillAt == nil {
		t.Error("backfill success should set last_backfill_at")
	}
	if d := got.ThroughputArtsPerSec - 390; d < -0.001 || d > 0.001 {
		t.Errorf("throughput EMA = %v, want ~390", got.ThroughputArtsPerSec)
	}

	// Two failing passes increment the counter and leave throughput untouched.
	for i := 0; i < 2; i++ {
		if err := st.RecordGroupScan(ctx, g.ID, GroupScanOutcome{
			Articles: 0, DurationMS: 500, Err: "connection reset",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = st.GetGroupByName(ctx, g.Name)
	if got.ConsecutiveFailures != 2 {
		t.Errorf("consecutive_failures = %d, want 2", got.ConsecutiveFailures)
	}
	if d := got.ThroughputArtsPerSec - 390; d < -0.001 || d > 0.001 {
		t.Errorf("failures should not change throughput, got %v", got.ThroughputArtsPerSec)
	}

	// Recovery resets the failure counter.
	if err := st.RecordGroupScan(ctx, g.ID, GroupScanOutcome{
		Articles: 10, Parts: 8, ServerHigh: 9600, DurationMS: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetGroupByName(ctx, g.Name)
	if got.ConsecutiveFailures != 0 {
		t.Errorf("recovery should reset consecutive_failures, got %d", got.ConsecutiveFailures)
	}
}

// TestGroupStorageBytes covers the per-group storage estimate.
func TestGroupStorageBytes(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g, err := st.UpsertGroup(ctx, "alt.binaries.storage", true)
	if err != nil {
		t.Fatal(err)
	}
	// No parts yet.
	m, err := st.GroupStorageBytes(ctx, []int64{g.ID})
	if err != nil {
		t.Fatal(err)
	}
	if m[g.ID] != 0 {
		t.Errorf("empty group storage = %d, want 0", m[g.ID])
	}

	// Seed two parts with known byte sizes.
	if _, err := st.InsertParts(ctx, []PartInput{
		{GroupID: g.ID, ArticleNumber: 1, MessageID: "<a@x>", Subject: "s [1/1]", Bytes: 100},
		{GroupID: g.ID, ArticleNumber: 2, MessageID: "<b@x>", Subject: "s2 [1/1]", Bytes: 250},
	}); err != nil {
		t.Fatal(err)
	}
	m, err = st.GroupStorageBytes(ctx, []int64{g.ID})
	if err != nil {
		t.Fatal(err)
	}
	if m[g.ID] != 350 {
		t.Errorf("group storage = %d, want 350", m[g.ID])
	}

	// Empty id list returns an empty map without querying.
	if m, err := st.GroupStorageBytes(ctx, nil); err != nil || len(m) != 0 {
		t.Errorf("empty ids => %v, %v", m, err)
	}
}

// TestGroupHealthStats covers the aggregate group-freshness metrics query
// (#129): active-group count, lag aggregates, failure aggregates, and
// never-scanned count over only active groups.
func TestGroupHealthStats(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	// g1: behind by 100, one recorded success (via a scan).
	g1, _ := st.UpsertGroup(ctx, "alt.binaries.ghs.one", true)
	if err := st.RecordGroupScan(ctx, g1.ID, GroupScanOutcome{
		Articles: 10, Parts: 8, ServerHigh: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	// Advance the server head so g1 shows lag of 100 (head 1100, watermark 1000).
	if _, err := st.Pool().Exec(ctx,
		`UPDATE groups SET server_high = 1100, last_scanned_high = 1000 WHERE id = $1`, g1.ID); err != nil {
		t.Fatal(err)
	}

	// g2: failing (2 consecutive failures), never succeeded.
	g2, _ := st.UpsertGroup(ctx, "alt.binaries.ghs.two", true)
	for i := 0; i < 2; i++ {
		if err := st.RecordGroupScan(ctx, g2.ID, GroupScanOutcome{Err: "boom"}); err != nil {
			t.Fatal(err)
		}
	}

	// g3: inactive — must be excluded from all aggregates.
	g3, _ := st.UpsertGroup(ctx, "alt.binaries.ghs.three", false)
	_ = g3

	s, err := st.GroupHealthStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ActiveGroups != 2 {
		t.Errorf("active groups = %d, want 2 (inactive excluded)", s.ActiveGroups)
	}
	if s.GroupsBehind != 1 || s.MaxLag != 100 || s.TotalLag != 100 {
		t.Errorf("lag aggregates = behind %d max %d total %d, want 1/100/100", s.GroupsBehind, s.MaxLag, s.TotalLag)
	}
	if s.GroupsFailing != 1 || s.MaxConsecutiveFailures != 2 {
		t.Errorf("failure aggregates = failing %d max %d, want 1/2", s.GroupsFailing, s.MaxConsecutiveFailures)
	}
	if s.GroupsNeverScanned != 1 {
		t.Errorf("never-scanned = %d, want 1 (g2)", s.GroupsNeverScanned)
	}
}

// TestClassifyGroupHealth covers the pure health classifier across the
// scenarios in the issue: healthy, lag warn/error, staleness, failure
// escalation, never-scanned, and disabled groups.
func TestClassifyGroupHealth(t *testing.T) {
	th := DefaultGroupHealthThresholds()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-1 * time.Minute)
	old := now.Add(-30 * time.Hour)
	tp := func(t time.Time) *time.Time { return &t }

	cases := []struct {
		name string
		g    Group
		want GroupHealthLevel
	}{
		{
			name: "healthy",
			g:    Group{Active: true, ServerHigh: 1000, LastScannedHigh: 1000, LastSuccessAt: tp(recent)},
			want: HealthOK,
		},
		{
			name: "lag warn",
			g:    Group{Active: true, ServerHigh: 100000, LastScannedHigh: 0, LastSuccessAt: tp(recent)},
			want: HealthWarn,
		},
		{
			name: "lag error",
			g:    Group{Active: true, ServerHigh: 1_000_000, LastScannedHigh: 0, LastSuccessAt: tp(recent)},
			want: HealthError,
		},
		{
			name: "stale error",
			g:    Group{Active: true, ServerHigh: 100, LastScannedHigh: 100, LastSuccessAt: tp(old)},
			want: HealthError,
		},
		{
			name: "failures warn",
			g:    Group{Active: true, ServerHigh: 100, LastScannedHigh: 100, LastSuccessAt: tp(recent), ConsecutiveFailures: 1},
			want: HealthWarn,
		},
		{
			name: "failures error",
			g:    Group{Active: true, ServerHigh: 100, LastScannedHigh: 100, LastSuccessAt: tp(recent), ConsecutiveFailures: 6},
			want: HealthError,
		},
		{
			name: "never scanned is unknown",
			g:    Group{Active: true},
			want: HealthUnknown,
		},
		{
			name: "inactive is unknown",
			g:    Group{Active: false, ServerHigh: 1_000_000, LastScannedHigh: 0},
			want: HealthUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := ClassifyGroupHealth(tc.g, th, now)
			if h.Level != tc.want {
				t.Errorf("level = %q, want %q (reasons: %v)", h.Level, tc.want, h.Reasons)
			}
		})
	}
}
