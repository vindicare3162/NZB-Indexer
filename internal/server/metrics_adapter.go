package server

import (
	"context"

	"github.com/vindicare/goindex/internal/metrics"
	"github.com/vindicare/goindex/internal/store"
	"github.com/vindicare/goindex/internal/worker"
)

// pipelineSnapshot adapts store.PipelineStatistics into the metrics package's
// PipelineSnapshot, keeping the metrics package free of a store import.
func pipelineSnapshot(ctx context.Context, st *store.Store) (metrics.PipelineSnapshot, error) {
	s, err := st.PipelineStatistics(ctx)
	if err != nil {
		return metrics.PipelineSnapshot{}, err
	}
	byPP := make(map[string]float64, len(s.ReleasesByPP))
	for k, v := range s.ReleasesByPP {
		byPP[k] = float64(v)
	}
	return metrics.PipelineSnapshot{
		PartsTotal:              float64(s.PartsTotal),
		PartsUnassigned:         float64(s.PartsUnassigned),
		BinariesTotal:           float64(s.BinariesTotal),
		BinariesComplete:        float64(s.BinariesComplete),
		BinariesUnreleased:      float64(s.BinariesUnreleased),
		ReleasesTotal:           float64(s.ReleasesTotal),
		ReleasesByPPStatus:      byPP,
		ReleasesFailedExhausted: float64(s.ReleasesFailedExhausted),
	}, nil
}

// workerSnapshot adapts worker.Metrics into the metrics package's
// WorkerSnapshot.
func workerSnapshot(m worker.Metrics) metrics.WorkerSnapshot {
	running := 0.0
	if m.Running {
		running = 1
	}
	return metrics.WorkerSnapshot{
		Running:         running,
		Cycles:          float64(m.Cycles),
		ArticlesPulled:  float64(m.ArticlesPulled),
		PartsInserted:   float64(m.PartsInserted),
		BinariesTouched: float64(m.BinariesTouched),
		ReleasesCreated: float64(m.ReleasesCreated),
		ReleasesRenamed: float64(m.ReleasesRenamed),
		NFOsFound:       float64(m.NFOsFound),
	}
}
