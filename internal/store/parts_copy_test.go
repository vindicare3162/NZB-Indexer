package store

import (
	"context"
	"testing"
)

func mkPart(groupID, article int64, msgID, norm string) PartInput {
	return PartInput{
		GroupID: groupID, ArticleNumber: article, MessageID: msgID,
		Subject: norm, Poster: "poster", Bytes: 1000, PartNumber: 1, TotalParts: 1,
		NormSubject: norm,
	}
}

func TestInsertPartsCopyIdempotent(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.copy", true)

	batch := []PartInput{
		mkPart(g.ID, 1, "m1@x", "file.one"),
		mkPart(g.ID, 2, "m2@x", "file.two"),
		mkPart(g.ID, 3, "m3@x", "file.three"),
	}

	// First load inserts all three.
	n, err := st.InsertParts(ctx, batch)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if n != 3 {
		t.Errorf("first insert count = %d, want 3", n)
	}

	// Re-scan of the same batch inserts nothing (idempotent on the natural key).
	n, err = st.InsertParts(ctx, batch)
	if err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if n != 0 {
		t.Errorf("re-insert count = %d, want 0 (idempotent)", n)
	}

	total, _ := st.CountParts(ctx, g.ID)
	if total != 3 {
		t.Errorf("total parts = %d, want 3", total)
	}
}

func TestInsertPartsCopyDedupesWithinBatch(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.copy.dup", true)

	// Two rows share the natural key (group_id, article_number). The second
	// must be collapsed, not error on "ON CONFLICT twice".
	batch := []PartInput{
		mkPart(g.ID, 10, "m10a@x", "dup.a"),
		mkPart(g.ID, 10, "m10b@x", "dup.b"), // same article number
		mkPart(g.ID, 11, "m11@x", "other"),
	}
	n, err := st.InsertParts(ctx, batch)
	if err != nil {
		t.Fatalf("insert with in-batch dup: %v", err)
	}
	if n != 2 {
		t.Errorf("inserted = %d, want 2 (in-batch dup collapsed)", n)
	}
	total, _ := st.CountParts(ctx, g.ID)
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
}

func TestInsertPartsCopyCancellationRollsBack(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.copy.cancel", true)

	// Seed one part so we can confirm the table is otherwise untouched.
	if _, err := st.InsertParts(ctx, []PartInput{mkPart(g.ID, 1, "seed@x", "seed")}); err != nil {
		t.Fatal(err)
	}

	// A cancelled context must abort the load: no new rows, transaction rolled
	// back, watermark-safe (caller must not advance).
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := st.InsertParts(cctx, []PartInput{
		mkPart(g.ID, 2, "m2@x", "two"),
		mkPart(g.ID, 3, "m3@x", "three"),
	})
	if err == nil {
		t.Error("expected error from cancelled InsertParts")
	}

	total, _ := st.CountParts(ctx, g.ID)
	if total != 1 {
		t.Errorf("total parts = %d, want 1 (cancelled batch must not persist)", total)
	}
}

func TestInsertPartsCopyEmpty(t *testing.T) {
	st := freshStore(t)
	n, err := st.InsertParts(context.Background(), nil)
	if err != nil || n != 0 {
		t.Errorf("empty insert = (%d, %v), want (0, nil)", n, err)
	}
}
