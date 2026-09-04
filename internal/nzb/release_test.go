package nzb

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

func freshStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping nzb integration test")
	}
	if err := store.MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestGenerateNZBEndToEnd runs the full pipeline slice: parts -> assemble ->
// release -> NZB, then validates the generated document.
func TestGenerateNZBEndToEnd(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.nzb", true)

	// Seed a complete 4-part binary.
	const total = 4
	var parts []store.PartInput
	for i := 1; i <= total; i++ {
		parts = append(parts, store.PartInput{
			GroupID:       g.ID,
			ArticleNumber: 1000 + int64(i),
			MessageID:     fmt.Sprintf("part%d@example.com", i),
			Subject:       fmt.Sprintf(`"Great.Movie.2024.1080p.mkv" yEnc (%d/%d)`, i, total),
			Poster:        "uploader@example.com",
			PostedAt:      ptrTime(time.Unix(1_700_000_000, 0)),
			Bytes:         int64(i) * 1000,
			PartNumber:    i,
			TotalParts:    total,
			NormSubject:   `"Great.Movie.2024.1080p.mkv"`,
		})
	}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}

	b := release.New(st, nil, release.Options{BatchLimit: 100})
	if _, err := b.Build(ctx); err != nil {
		t.Fatal(err)
	}

	// Find the release GUID.
	var guid string
	if err := st.Pool().QueryRow(ctx, `SELECT guid FROM releases LIMIT 1`).Scan(&guid); err != nil {
		t.Fatalf("no release created: %v", err)
	}

	gen := NewGenerator(st)
	data, filename, err := gen.ForGUID(ctx, guid)
	if err != nil {
		t.Fatalf("generate nzb: %v", err)
	}
	if !strings.HasSuffix(filename, ".nzb") {
		t.Errorf("filename = %q, want .nzb suffix", filename)
	}

	// Validate the NZB has all 4 segments in order and the correct group.
	var parsed struct {
		Files []struct {
			Groups struct {
				Group []string `xml:"group"`
			} `xml:"groups"`
			Segments struct {
				Segment []struct {
					Number int    `xml:"number,attr"`
					Bytes  int64  `xml:"bytes,attr"`
					Value  string `xml:",chardata"`
				} `xml:"segment"`
			} `xml:"segments"`
		} `xml:"file"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse generated nzb: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(parsed.Files))
	}
	f := parsed.Files[0]
	if len(f.Segments.Segment) != total {
		t.Fatalf("segments = %d, want %d", len(f.Segments.Segment), total)
	}
	for i, seg := range f.Segments.Segment {
		wantNum := i + 1
		if seg.Number != wantNum {
			t.Errorf("segment %d number = %d, want %d", i, seg.Number, wantNum)
		}
		wantID := fmt.Sprintf("part%d@example.com", wantNum)
		if seg.Value != wantID {
			t.Errorf("segment %d id = %q, want %q", i, seg.Value, wantID)
		}
	}
	if len(f.Groups.Group) != 1 || f.Groups.Group[0] != "alt.binaries.nzb" {
		t.Errorf("groups = %v, want [alt.binaries.nzb]", f.Groups.Group)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestNZBFromDurableSegmentsAfterPartsPruned is the #105 retention guarantee: a
// release must generate its NZB after its backing raw parts are deleted,
// because build time snapshots segments into durable release storage.
func TestNZBFromDurableSegmentsAfterPartsPruned(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.retain", true)

	const total = 3
	var parts []store.PartInput
	for i := 1; i <= total; i++ {
		parts = append(parts, store.PartInput{
			GroupID: g.ID, ArticleNumber: 2000 + int64(i),
			MessageID: fmt.Sprintf("keep%d@example.com", i),
			Subject:   fmt.Sprintf(`"Keep.Me.2024.1080p.mkv" yEnc (%d/%d)`, i, total),
			Poster:    "up@example.com", PostedAt: ptrTime(time.Unix(1_700_000_100, 0)),
			Bytes: int64(i) * 500, PartNumber: i, TotalParts: total,
			NormSubject: `"Keep.Me.2024.1080p.mkv"`,
		})
	}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := release.New(st, nil, release.Options{BatchLimit: 100}).Build(ctx); err != nil {
		t.Fatal(err)
	}

	var guid string
	if err := st.Pool().QueryRow(ctx, `SELECT guid FROM releases LIMIT 1`).Scan(&guid); err != nil {
		t.Fatalf("no release: %v", err)
	}

	// Durable segments must have been snapshotted at build time.
	var segCount int
	if err := st.Pool().QueryRow(ctx,
		`SELECT jsonb_array_length(segments) FROM releases WHERE guid = $1`, guid).Scan(&segCount); err != nil {
		t.Fatal(err)
	}
	if segCount != total {
		t.Fatalf("durable segments = %d, want %d", segCount, total)
	}

	// Now DELETE all raw parts (simulating retention pruning).
	if _, err := st.Pool().Exec(ctx, `DELETE FROM parts`); err != nil {
		t.Fatal(err)
	}

	// NZB generation must still succeed from durable segments.
	data, _, err := NewGenerator(st).ForGUID(ctx, guid)
	if err != nil {
		t.Fatalf("nzb generation after parts pruned: %v", err)
	}
	if got := strings.Count(string(data), "<segment "); got != total {
		t.Errorf("nzb segments after pruning = %d, want %d", got, total)
	}
	for i := 1; i <= total; i++ {
		if !strings.Contains(string(data), fmt.Sprintf("keep%d@example.com", i)) {
			t.Errorf("nzb missing segment keep%d after pruning", i)
		}
	}
}

// TestBackfillReleaseSegmentsRepairsLegacy verifies the backfill tool snapshots
// segments for a legacy release whose segments are empty but whose parts still
// exist, and reports releases it cannot repair.
func TestBackfillReleaseSegmentsRepairsLegacy(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.legacy", true)

	parts := []store.PartInput{{
		GroupID: g.ID, ArticleNumber: 3001, MessageID: "leg1@example.com",
		Subject: `"Legacy.mkv" yEnc (1/1)`, Poster: "p", Bytes: 100, PartNumber: 1, TotalParts: 1,
		NormSubject: `"Legacy.mkv"`,
	}}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := release.New(st, nil, release.Options{BatchLimit: 100}).Build(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy release: clear its durable segments.
	if _, err := st.Pool().Exec(ctx, `UPDATE releases SET segments = '[]'::jsonb`); err != nil {
		t.Fatal(err)
	}

	repaired, unresolved, err := st.BackfillReleaseSegments(ctx, 100)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if repaired != 1 || unresolved != 0 {
		t.Errorf("backfill repaired=%d unresolved=%d, want 1/0", repaired, unresolved)
	}
	var segCount int
	st.Pool().QueryRow(ctx, `SELECT jsonb_array_length(segments) FROM releases LIMIT 1`).Scan(&segCount)
	if segCount != 1 {
		t.Errorf("after backfill durable segments = %d, want 1", segCount)
	}
}
