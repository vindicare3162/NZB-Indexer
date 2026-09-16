package reassemble

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vindicare/goindex/internal/store"
)

func bid(v int64) *int64 { return &v }

// TestPlanRegroupsSplitPost is the falsifying test for #196. Before the parser
// fix these subjects produced no collection key, so every file of one post
// became its own binary and its own release: one 43-file post existed as 43
// releases averaging ~10MB. Re-parsing must give them all one key.
func TestPlanRegroupsSplitPost(t *testing.T) {
	const pre = `[37576]-[#a.b.foreign@EFNet]-[ Eurogang.S01E03.Ein.Wagen.voll.Madonnen.GERMAN.FS.DVDRip.xviD-aWake ]-[%02d/43] - "awa-eurogangs01e03.r%02d" yEnc (1/33)`

	var parts []store.PartForReparse
	for i := 1; i <= 43; i++ {
		parts = append(parts, store.PartForReparse{
			ID:       int64(i),
			Subject:  fmt.Sprintf(pre, i, i-1),
			BinaryID: bid(int64(1000 + i)), // each file got its own binary
			// stored as the pre-fix parser left it
			CollectionKey: "",
		})
	}

	updates, dirty := Plan(parts)
	if len(updates) != 43 {
		t.Fatalf("regrouped %d of 43 parts", len(updates))
	}
	key := updates[0].CollectionKey
	if key == "" {
		t.Fatal("no collection key; the post would stay split")
	}
	for _, u := range updates {
		if u.CollectionKey != key {
			t.Errorf("part %d got key %q, want %q — the post is still split", u.ID, u.CollectionKey, key)
		}
		if u.CollectionFiles != 43 {
			t.Errorf("part %d declares %d files, want 43", u.ID, u.CollectionFiles)
		}
	}
	if len(dirty) != 43 {
		t.Errorf("marked %d binaries for teardown, want 43", len(dirty))
	}
}

// Parts whose grouping is already correct must not be rewritten: re-parsing
// 1.8B rows is only affordable if unchanged rows cost nothing.
func TestPlanSkipsUnchanged(t *testing.T) {
	const subj = `[37576]-[#a.b.foreign@EFNet]-[ Eurogang.S01E03.GERMAN.DVDRip.xviD-aWake ]-[01/43] - "awa-eurogangs01e03.nfo" yEnc (1/1)`
	// Seed the stored fields with exactly what the current parser produces.
	seed, _ := Plan([]store.PartForReparse{{ID: 1, Subject: subj}})
	u := seed[0]

	updates, dirty := Plan([]store.PartForReparse{{
		ID: 1, Subject: subj, BinaryID: bid(7),
		CollectionKey:   u.CollectionKey,
		FileNumber:      u.FileNumber,
		CollectionFiles: u.CollectionFiles,
		NormSubject:     u.NormSubject,
	}})
	if len(updates) != 0 || len(dirty) != 0 {
		t.Errorf("already-correct part rewritten: updates=%d dirty=%d", len(updates), len(dirty))
	}
}

// A part with no binary yet still gets its grouping fixed, but contributes no
// teardown — there is nothing to rebuild.
func TestPlanUnassembledPartNeedsNoTeardown(t *testing.T) {
	updates, dirty := Plan([]store.PartForReparse{{
		ID: 1, BinaryID: nil,
		Subject: `"Some Show S01E01" [03/18] - "show.s01e01.part1.rar" yEnc (1/206)`,
	}})
	if len(updates) != 1 {
		t.Fatalf("expected the part to be regrouped, got %d updates", len(updates))
	}
	if len(dirty) != 0 {
		t.Errorf("unassembled part marked %d binaries for teardown", len(dirty))
	}
}

// --- cursor behaviour ---

type fakeRepo struct {
	batches  [][]store.PartForReparse
	calls    int
	settings map[string]string
	applied  int
	failNext bool
}

func (f *fakeRepo) ListPartsForReparse(_ context.Context, after int64, _ int) ([]store.PartForReparse, error) {
	if f.calls >= len(f.batches) {
		return nil, nil
	}
	b := f.batches[f.calls]
	f.calls++
	return b, nil
}

func (f *fakeRepo) ApplyReparse(context.Context, []store.PartReparse, []int64) (store.ReassembleStats, error) {
	if f.failNext {
		return store.ReassembleStats{}, errors.New("boom")
	}
	f.applied++
	return store.ReassembleStats{PartsUpdated: 1}, nil
}

func (f *fakeRepo) GetSetting(_ context.Context, k string) (string, bool, error) {
	v, ok := f.settings[k]
	return v, ok, nil
}

func (f *fakeRepo) SetSetting(_ context.Context, k, v string) error {
	f.settings[k] = v
	return nil
}

// The cursor must be persisted after each batch, so a pass that is interrupted
// resumes where it stopped rather than rescanning from zero.
func TestRunPersistsCursorPerBatch(t *testing.T) {
	f := &fakeRepo{
		settings: map[string]string{},
		batches: [][]store.PartForReparse{
			{{ID: 10, Subject: `"T" [01/18] - "a.part1.rar" yEnc (1/9)`}},
			{{ID: 25, Subject: `"T" [02/18] - "a.part2.rar" yEnc (1/9)`}},
		},
	}
	r := New(f, nil, Options{BatchSize: 1, MaxBatchesPerRun: 5})
	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := f.settings[cursorKey]; got != "25" {
		t.Errorf("cursor = %q, want \"25\"", got)
	}
	if !res.Done {
		t.Errorf("expected Done once the batches were exhausted")
	}
}

// A resumed run must start from the persisted cursor, not from zero.
func TestRunResumesFromCursor(t *testing.T) {
	f := &fakeRepo{settings: map[string]string{cursorKey: "500"}}
	var sawAfter int64 = -1
	fr := &recordingRepo{fakeRepo: f, after: &sawAfter}
	r := New(fr, nil, Options{})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sawAfter != 500 {
		t.Errorf("resumed from %d, want 500", sawAfter)
	}
}

type recordingRepo struct {
	*fakeRepo
	after *int64
}

func (r *recordingRepo) ListPartsForReparse(ctx context.Context, after int64, n int) ([]store.PartForReparse, error) {
	*r.after = after
	return r.fakeRepo.ListPartsForReparse(ctx, after, n)
}

// A failing batch must not advance the cursor, or the failed range is skipped
// silently and never repaired.
func TestRunDoesNotAdvanceCursorOnFailure(t *testing.T) {
	f := &fakeRepo{
		settings: map[string]string{},
		failNext: true,
		batches: [][]store.PartForReparse{
			{{ID: 10, Subject: `"T" [01/18] - "a.part1.rar" yEnc (1/9)`}},
		},
	}
	r := New(f, nil, Options{})
	if _, err := r.Run(context.Background()); err == nil {
		t.Fatal("expected the batch failure to surface")
	}
	if v, ok := f.settings[cursorKey]; ok {
		t.Errorf("cursor advanced to %q despite a failed batch", v)
	}
}
