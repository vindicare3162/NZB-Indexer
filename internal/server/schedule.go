package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/vindicare/goindex/internal/api/rest"
	"github.com/vindicare/goindex/internal/store"
	"github.com/vindicare/goindex/internal/worker"
)

// settings keys for the persisted schedule (mirrors the constants in the rest
// package; duplicated here to avoid an import cycle and keep the mapping local
// to startup loading).
const (
	settingScanInterval     = "schedule.scan_interval"
	settingDownstream       = "schedule.downstream_interval"
	settingBuildInterval    = "schedule.build_interval"
	settingPostProcInterval = "schedule.postprocess_interval"
)

// scheduleAdapter adapts *worker.Worker to rest.JobController: the trigger and
// status methods pass through via the embedded worker, and the schedule methods
// translate between rest.Schedule and worker.Schedule (which are structurally
// identical but live in different packages to keep rest free of a worker
// import).
type scheduleAdapter struct {
	*worker.Worker
}

func (s scheduleAdapter) CurrentSchedule() rest.Schedule {
	w := s.Worker.CurrentSchedule()
	return rest.Schedule{
		ScanInterval:        w.ScanInterval,
		DownstreamInterval:  w.DownstreamInterval,
		BuildInterval:       w.BuildInterval,
		PostProcessInterval: w.PostProcessInterval,
	}
}

func (s scheduleAdapter) Reconfigure(r rest.Schedule) {
	s.Worker.Reconfigure(worker.Schedule{
		ScanInterval:        r.ScanInterval,
		DownstreamInterval:  r.DownstreamInterval,
		BuildInterval:       r.BuildInterval,
		PostProcessInterval: r.PostProcessInterval,
	})
}

// applyPersistedSchedule overrides the worker options with any schedule
// intervals persisted in the settings table, so a value set from the admin UI
// survives a restart. Missing/invalid values leave the config default in place.
func applyPersistedSchedule(ctx context.Context, st *store.Store, opts *worker.Options, log *slog.Logger) {
	settings, err := st.GetSettings(ctx)
	if err != nil {
		log.Warn("could not load persisted schedule; using config defaults", "err", err)
		return
	}
	apply := func(key string, dst *time.Duration) {
		raw, ok := settings[key]
		if !ok {
			return
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			log.Warn("ignoring invalid persisted schedule value", "key", key, "value", raw)
			return
		}
		*dst = d
	}
	apply(settingScanInterval, &opts.ScanInterval)
	apply(settingDownstream, &opts.DownstreamInterval)
	apply(settingBuildInterval, &opts.BuildInterval)
	apply(settingPostProcInterval, &opts.PostProcessInterval)
}
