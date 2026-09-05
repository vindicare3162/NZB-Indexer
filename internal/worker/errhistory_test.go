package worker

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestRecordStageErrorRetainsHistory verifies the bounded error ring keeps
// multiple recent errors (newest first) with stage/group classification, so a
// single last-error field no longer loses concurrent failures (#133).
func TestRecordStageErrorRetainsHistory(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})

	w.recordStageError("scan", "alt.binaries.a", errors.New("boom a"))
	w.recordStageError("assemble", "", errors.New("assemble failed"))
	w.recordStageError("scan", "alt.binaries.b", errors.New("boom b"))

	got := w.RecentErrors(10)
	if len(got) != 3 {
		t.Fatalf("recent errors = %d, want 3", len(got))
	}
	// Newest first.
	if got[0].Message != "boom b" || got[0].Stage != "scan" || got[0].Group != "alt.binaries.b" {
		t.Errorf("newest error = %+v, want scan/alt.binaries.b/boom b", got[0])
	}
	if got[2].Message != "boom a" {
		t.Errorf("oldest error = %+v, want boom a", got[2])
	}
	// Sequence numbers increase.
	if !(got[0].Seq > got[1].Seq && got[1].Seq > got[2].Seq) {
		t.Errorf("seq not decreasing newest-first: %d %d %d", got[0].Seq, got[1].Seq, got[2].Seq)
	}
	// Last-error summary reflects the most recent.
	if le := w.MetricsSnapshot().LastError; le != "boom b" {
		t.Errorf("last error summary = %q, want boom b", le)
	}
}

// TestErrorHistoryBounded verifies the ring never exceeds its cap and keeps the
// most recent entries.
func TestErrorHistoryBounded(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})
	total := maxErrHistory + 50
	for i := 0; i < total; i++ {
		w.recordStageError("scan", "g", fmt.Errorf("err %d", i))
	}
	got := w.RecentErrors(0) // all retained
	if len(got) != maxErrHistory {
		t.Fatalf("history size = %d, want %d (bounded)", len(got), maxErrHistory)
	}
	// Newest retained is the last recorded.
	if got[0].Message != fmt.Sprintf("err %d", total-1) {
		t.Errorf("newest = %q, want err %d", got[0].Message, total-1)
	}
}

// TestConcurrentFailuresAllRetained verifies that failures recorded
// concurrently by multiple goroutines are all captured (up to the bound) — the
// core #133 guarantee that one error no longer overwrites another.
func TestConcurrentFailuresAllRetained(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})
	const goroutines, per = 8, 10
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				w.recordStageError("scan", fmt.Sprintf("g%d", g), fmt.Errorf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()

	got := w.RecentErrors(0)
	if len(got) != goroutines*per {
		t.Fatalf("retained %d errors, want %d (none lost, under bound)", len(got), goroutines*per)
	}
	// Sequence numbers must be unique (no lost updates under concurrency).
	seen := map[int64]bool{}
	for _, e := range got {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}
