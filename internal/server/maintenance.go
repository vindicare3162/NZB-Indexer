package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/maintenance"
	"github.com/vindicare/goindex/internal/reassemble"
	"github.com/vindicare/goindex/internal/store"
	"github.com/vindicare/goindex/internal/yencverify"
)

// jobRecorderAdapter adapts *store.Store to maintenance.JobRecorder, whose
// CreateJob returns an untyped job so the maintenance package need not import
// the store.
type jobRecorderAdapter struct{ st *store.Store }

func (a jobRecorderAdapter) CreateJob(ctx context.Context, id, jobType, target string) (any, error) {
	return a.st.CreateJob(ctx, id, jobType, target)
}
func (a jobRecorderAdapter) StartJob(ctx context.Context, id string) error {
	return a.st.StartJob(ctx, id)
}
func (a jobRecorderAdapter) FinishJob(ctx context.Context, id, state, errMsg string) error {
	return a.st.FinishJob(ctx, id, state, errMsg)
}

// buildMaintenanceTasks assembles the enabled housekeeping tasks from config
// (#130). Retention pruning is included here when retention is enabled, unifying
// it with the other scheduled tasks; its window/batch limits come from
// RetentionConfig.
func buildMaintenanceTasks(st *store.Store, cfg config.Config, fetch yencverify.Fetcher, log *slog.Logger) []maintenance.Task {
	m := cfg.Maintenance
	var tasks []maintenance.Task

	// yEnc verification (#178). Articles whose Subject carries no "(n/m)"
	// counter can only be told apart from genuine single-part posts by their
	// yEnc header, so they are held out of assembly until checked here. Each
	// check reads only the control line rather than the ~750KB body, which
	// trades wall-clock (a fresh connection per article, since the abandoned
	// response makes it unusable) for roughly 250x less transfer.
	//
	// Both passes draw on the same small NNTP connection budget as scanning and
	// post-processing, so they are not sized equally: repairing already-released
	// stubs is the finite, high-value job and gets the larger share, while the
	// far larger pool of never-assembled ambiguous parts trickles along behind
	// it and can be left running indefinitely.
	if fetch != nil {
		repair := yencverify.New(fetch, st, log, yencverify.Options{
			BatchLimit:  500,
			Concurrency: 4,
		})
		// Sized so a pass finishes inside its interval: the scheduler does not
		// skip an overlapping run, and overlapping passes multiply connection
		// demand on a budget already shared with scanning and post-processing —
		// which is what previously exhausted the provider's connection limit and
		// failed whole batches at once. At ~2.5s per article (a fresh connection
		// each, since the header read abandons the response) three in parallel
		// clear ~150 in well under two minutes.
		backlog := yencverify.New(fetch, st, log, yencverify.Options{
			BatchLimit:  150,
			Concurrency: 3,
		})
		tasks = append(tasks,
			maintenance.Task{
				Name: "yenc-repair", Interval: time.Minute, Enabled: true,
				Run: func(ctx context.Context) (string, error) {
					r, err := repair.RunRepair(ctx)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("rechecked %d stub releases: %d genuinely complete, %d discarded as fragments, %d failed",
						r.Checked, r.Standalone, r.Fragments, r.Failed), nil
				},
			},
			maintenance.Task{
				Name: "yenc-verify", Interval: 2 * time.Minute, Enabled: true,
				Run: func(ctx context.Context) (string, error) {
					r, err := backlog.Run(ctx)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("checked %d articles: %d standalone, %d fragments, %d failed",
						r.Checked, r.Standalone, r.Fragments, r.Failed), nil
				},
			},
		)
	}

	if cfg.Retention.Enabled && cfg.Retention.Days > 0 {
		interval := cfg.Retention.Interval
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		olderThan := time.Duration(cfg.Retention.Days) * 24 * time.Hour
		bs, mb := cfg.Retention.BatchSize, cfg.Retention.MaxBatchesPerRun
		tasks = append(tasks, maintenance.Task{
			Name: "retention", Interval: interval, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				deleted, err := st.PruneRetainedPartsAll(ctx, olderThan, bs, mb)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("pruned %d raw parts older than %d days", deleted, cfg.Retention.Days), nil
			},
		})
	}

	// Re-assembly of pre-fix parts (#196). Off unless explicitly enabled: it
	// rewrites parts, deletes binaries, and removes the releases built from
	// them. Releases that have already been post-processed are protected inside
	// the store layer, so a client cannot lose something it has grabbed.
	if m.Reassemble.Enabled {
		interval := m.Reassemble.Interval
		if interval <= 0 {
			interval = time.Minute
		}
		ra := reassemble.New(st, log, reassemble.Options{})
		tasks = append(tasks, maintenance.Task{
			Name: "reassemble", Interval: interval, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				r, err := ra.Run(ctx)
				if err != nil {
					return "", err
				}
				if r.Done {
					return fmt.Sprintf("re-parse complete at cursor %d; nothing left to scan", r.Cursor), nil
				}
				return fmt.Sprintf(
					"scanned %d parts in %d batches: %d regrouped, %d binaries rebuilt, %d releases removed, %d protected (cursor %d)",
					r.PartsScanned, r.BatchesRun, r.PartsUpdated, r.BinariesDeleted,
					r.ReleasesDeleted, r.BinariesProtected, r.Cursor), nil
			},
		})
	}

	if m.RetryFailed.Enabled {
		tasks = append(tasks, maintenance.Task{
			Name: "retry-failed", Interval: m.RetryFailed.Interval, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				n, err := st.RequeueFailedReleases(ctx)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("requeued %d failed releases", n), nil
			},
		})
	}

	if m.Analyze.Enabled {
		tasks = append(tasks, maintenance.Task{
			Name: "analyze", Interval: m.Analyze.Interval, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				if err := st.AnalyzeStatistics(ctx); err != nil {
					return "", err
				}
				return "refreshed planner statistics", nil
			},
		})
	}

	if m.JobCleanup.Enabled {
		retain := m.JobRetention
		if retain <= 0 {
			retain = 7 * 24 * time.Hour
		}
		tasks = append(tasks, maintenance.Task{
			Name: "job-cleanup", Interval: m.JobCleanup.Interval, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				n, err := st.CleanupOldJobs(ctx, retain)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("removed %d old jobs", n), nil
			},
		})
	}

	if m.BackupVerify.Enabled {
		tasks = append(tasks, maintenance.Task{
			Name: "backup-verify", Interval: m.BackupVerify.Interval, Enabled: true,
			Run: func(ctx context.Context) (string, error) {
				br, err := st.VerifyBackupReadiness(ctx)
				if err != nil {
					return "", err
				}
				if !br.OK {
					return "", fmt.Errorf("backup readiness check failed: only %d/%d tables reachable", br.Tables, len(store.CapacityTableNames()))
				}
				return fmt.Sprintf("backup readiness ok (%d tables, db %d bytes)", br.Tables, br.DatabaseBytes), nil
			},
		})
	}

	return tasks
}

// newMaintenanceScheduler builds the scheduler from config, wiring job history
// and notifications (#130).
func newMaintenanceScheduler(st *store.Store, cfg config.Config, fetch yencverify.Fetcher, notifier maintenance.Notifier, log *slog.Logger) *maintenance.Scheduler {
	return maintenance.New(buildMaintenanceTasks(st, cfg, fetch, log), jobRecorderAdapter{st: st}, notifier, log)
}
