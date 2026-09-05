package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/maintenance"
	"github.com/vindicare/goindex/internal/store"
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
func buildMaintenanceTasks(st *store.Store, cfg config.Config) []maintenance.Task {
	m := cfg.Maintenance
	var tasks []maintenance.Task

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
func newMaintenanceScheduler(st *store.Store, cfg config.Config, notifier maintenance.Notifier, log *slog.Logger) *maintenance.Scheduler {
	return maintenance.New(buildMaintenanceTasks(st, cfg), jobRecorderAdapter{st: st}, notifier, log)
}
