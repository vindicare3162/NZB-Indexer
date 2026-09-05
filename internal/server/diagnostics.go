package server

import (
	"github.com/vindicare/goindex/internal/api/rest"
	"github.com/vindicare/goindex/internal/worker"
)

// errorHistorian adapts *worker.Worker to rest.ErrorHistorian, converting the
// worker's in-process pipeline-error history into the REST view type (#133) so
// the REST layer need not import the worker package.
type errorHistorian struct{ w *worker.Worker }

func (e errorHistorian) RecentErrors(limit int) []rest.PipelineError {
	src := e.w.RecentErrors(limit)
	out := make([]rest.PipelineError, len(src))
	for i, pe := range src {
		out[i] = rest.PipelineError{
			Seq:     pe.Seq,
			Stage:   pe.Stage,
			Group:   pe.Group,
			Message: pe.Message,
			At:      pe.At,
		}
	}
	return out
}
