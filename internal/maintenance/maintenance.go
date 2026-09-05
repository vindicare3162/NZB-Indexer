// Package maintenance runs routine housekeeping (retention pruning, failed-work
// retries, statistics maintenance, old-job cleanup, and backup-readiness
// verification) as observable, scheduled jobs (#130). Each task has its own
// cadence and enablement; a run is wrapped in a persistent pipeline job (so it
// shows up in job history) and publishes a notification on completion or
// failure. A failing task never stops the others, and every task stops promptly
// on context cancellation.
package maintenance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vindicare/goindex/internal/notify"
)

// TaskFunc performs one maintenance run. It returns a short human-readable
// summary (recorded on the job/notification) and an error on failure. It must
// honour ctx for cancellation.
type TaskFunc func(ctx context.Context) (summary string, err error)

// Task is one scheduled maintenance job.
type Task struct {
	// Name is a stable identifier used as the job type and in logs.
	Name string
	// Interval is how often the task runs. Must be > 0 when enabled.
	Interval time.Duration
	// Enabled gates the task; a disabled task never runs.
	Enabled bool
	// Run performs the work.
	Run TaskFunc
}

// JobRecorder records a maintenance run as a persistent pipeline job (#113) so
// it appears in job history. The store.Store satisfies it. Optional: a nil
// recorder skips job bookkeeping (the task still runs).
type JobRecorder interface {
	CreateJob(ctx context.Context, id, jobType, target string) (job any, err error)
	StartJob(ctx context.Context, id string) error
	FinishJob(ctx context.Context, id, state, errMsg string) error
}

// Notifier publishes maintenance events (#137). Optional; nil skips
// notifications.
type Notifier interface {
	Emit(e notify.Event)
}

// Scheduler runs a set of maintenance tasks, each on its own cadence.
type Scheduler struct {
	tasks    []Task
	jobs     JobRecorder
	notifier Notifier
	log      *slog.Logger

	// now is injectable for deterministic tests.
	now func() time.Time
}

// New builds a Scheduler. jobs and notifier may be nil.
func New(tasks []Task, jobs JobRecorder, notifier Notifier, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{tasks: tasks, jobs: jobs, notifier: notifier, log: log, now: time.Now}
}

// Run starts one goroutine per enabled task and blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range s.tasks {
		if !t.Enabled || t.Interval <= 0 || t.Run == nil {
			continue
		}
		wg.Add(1)
		go func(task Task) {
			defer wg.Done()
			s.loop(ctx, task)
		}(t)
	}
	wg.Wait()
}

// loop runs one task on its cadence until ctx is cancelled. An initial run
// happens shortly after start.
func (s *Scheduler) loop(ctx context.Context, t Task) {
	s.log.Info("maintenance task scheduled", "task", t.Name, "interval", t.Interval)
	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	s.execute(ctx, t) // initial run
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.execute(ctx, t)
		}
	}
}

// execute runs one pass of a task, wrapping it in a persistent job and emitting
// a notification on completion/failure. A panic or error is contained to this
// task.
func (s *Scheduler) execute(ctx context.Context, t Task) {
	if ctx.Err() != nil {
		return
	}
	jobID := uuid.NewString()
	s.startJob(ctx, jobID, t.Name)

	summary, err := t.Run(ctx)

	if err != nil {
		s.finishJob(ctx, jobID, "failed", err.Error())
		s.log.Warn("maintenance task failed", "task", t.Name, "err", err)
		s.emit(notify.Event{
			Type:    notify.EventBackupOutcome, // overridden per task below
			Title:   "Maintenance task failed: " + t.Name,
			Message: err.Error(),
			Fields:  map[string]string{"task": t.Name, "job_id": jobID},
		})
		return
	}
	s.finishJob(ctx, jobID, "completed", "")
	if summary != "" {
		s.log.Info("maintenance task completed", "task", t.Name, "summary", summary)
	}
	s.emit(notify.Event{
		Type:    eventTypeFor(t.Name),
		Title:   "Maintenance task completed: " + t.Name,
		Message: summary,
		Fields:  map[string]string{"task": t.Name, "job_id": jobID},
	})
}

// eventTypeFor maps a task name to the most specific notification event type.
func eventTypeFor(name string) notify.EventType {
	switch name {
	case "retention":
		return notify.EventRetentionCompleted
	case "backup-verify":
		return notify.EventBackupOutcome
	default:
		return notify.EventJobCompleted
	}
}

func (s *Scheduler) startJob(ctx context.Context, id, name string) {
	if s.jobs == nil {
		return
	}
	if _, err := s.jobs.CreateJob(ctx, id, "maintenance:"+name, name); err != nil {
		s.log.Debug("maintenance job create failed", "task", name, "err", err)
		return
	}
	_ = s.jobs.StartJob(ctx, id)
}

func (s *Scheduler) finishJob(ctx context.Context, id, state, errMsg string) {
	if s.jobs == nil {
		return
	}
	// Detach from cancellation so a just-finished run still records its terminal
	// state even as the parent context is shutting down.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.jobs.FinishJob(fctx, id, state, errMsg)
}

func (s *Scheduler) emit(e notify.Event) {
	if s.notifier != nil {
		s.notifier.Emit(e)
	}
}
