package store

import (
	"context"
	"testing"
	"time"
)

func TestJobLifecycle(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	j, err := st.CreateJob(ctx, "job-1", "scan", "alt.binaries.test")
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if j.State != JobQueued || j.Type != "scan" || j.Target != "alt.binaries.test" {
		t.Fatalf("unexpected new job: %+v", j)
	}
	if j.StartedAt != nil || j.FinishedAt != nil {
		t.Error("new job should have no start/finish timestamps")
	}

	// Start -> running with started_at.
	if err := st.StartJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	// Progress update.
	if err := st.UpdateJobProgress(ctx, "job-1", 3, 10, "scanning"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetJob(ctx, "job-1")
	if got.State != JobRunning || got.StartedAt == nil {
		t.Errorf("running job: %+v", got)
	}
	if got.ProgressCurrent != 3 || got.ProgressTotal != 10 || got.Message != "scanning" {
		t.Errorf("progress not recorded: %+v", got)
	}

	// Complete.
	if err := st.FinishJob(ctx, "job-1", JobCompleted, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetJob(ctx, "job-1")
	if got.State != JobCompleted || got.FinishedAt == nil {
		t.Errorf("completed job: %+v", got)
	}
}

func TestJobCancellation(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	st.CreateJob(ctx, "cancel-me", "backfill", "")
	if req, _ := st.IsJobCancelRequested(ctx, "cancel-me"); req {
		t.Error("cancel should not be requested initially")
	}
	if err := st.RequestJobCancel(ctx, "cancel-me"); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	if req, err := st.IsJobCancelRequested(ctx, "cancel-me"); err != nil || !req {
		t.Errorf("cancel should be requested, got req=%v err=%v", req, err)
	}

	// Cancelling a terminal job is not eligible.
	st.FinishJob(ctx, "cancel-me", JobCancelled, "")
	if err := st.RequestJobCancel(ctx, "cancel-me"); err != ErrNotFound {
		t.Errorf("cancelling terminal job = %v, want ErrNotFound", err)
	}
	// Unknown job.
	if err := st.RequestJobCancel(ctx, "nope"); err != ErrNotFound {
		t.Errorf("cancel unknown = %v, want ErrNotFound", err)
	}
	if _, err := st.GetJob(ctx, "nope"); err != ErrNotFound {
		t.Errorf("get unknown = %v, want ErrNotFound", err)
	}
}

func TestJobListingAndInterrupt(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	st.CreateJob(ctx, "j1", "scan", "")
	st.CreateJob(ctx, "j2", "assemble", "")
	st.StartJob(ctx, "j2")
	st.CreateJob(ctx, "j3", "build", "")
	st.FinishJob(ctx, "j3", JobCompleted, "")

	jobs, err := st.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("listed %d jobs, want 3", len(jobs))
	}
	// Newest first: j3 created last.
	if jobs[0].ID != "j3" {
		t.Errorf("first job = %s, want j3 (newest first)", jobs[0].ID)
	}

	// Restart recovery: queued/running jobs become interrupted; terminal stay.
	n, err := st.MarkInterruptedJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("marked %d interrupted, want 2 (j1 queued + j2 running)", n)
	}
	j1, _ := st.GetJob(ctx, "j1")
	if j1.State != JobInterrupted || j1.Error == "" {
		t.Errorf("j1 should be interrupted with an error: %+v", j1)
	}
	j3, _ := st.GetJob(ctx, "j3")
	if j3.State != JobCompleted {
		t.Errorf("j3 should stay completed, got %s", j3.State)
	}
}

func TestJobCleanup(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	st.CreateJob(ctx, "old", "scan", "")
	st.FinishJob(ctx, "old", JobCompleted, "")
	// Backdate its finished_at beyond the cleanup window.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE jobs SET finished_at = now() - interval '30 days' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	st.CreateJob(ctx, "recent", "scan", "")
	st.FinishJob(ctx, "recent", JobCompleted, "")
	st.CreateJob(ctx, "active", "scan", "") // queued: never cleaned

	n, err := st.CleanupOldJobs(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleaned %d, want 1 (only the 30-day-old terminal job)", n)
	}
	if _, err := st.GetJob(ctx, "old"); err != ErrNotFound {
		t.Error("old job should have been cleaned up")
	}
	if _, err := st.GetJob(ctx, "recent"); err != nil {
		t.Error("recent terminal job should be retained")
	}
	if _, err := st.GetJob(ctx, "active"); err != nil {
		t.Error("active (queued) job must never be cleaned up")
	}
}
