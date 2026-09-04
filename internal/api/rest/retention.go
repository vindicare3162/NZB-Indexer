package rest

import (
	"net/http"
	"strconv"
	"time"
)

// retentionDaysFromRequest resolves the retention window for a request: an
// explicit ?days= query parameter overrides the configured default. Returns
// (days, ok); ok is false when neither is set (so the endpoint can 400).
func (a *API) retentionDaysFromRequest(r *http.Request) (int, bool) {
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n, true
		}
		return 0, false
	}
	if a.retentionDays > 0 {
		return a.retentionDays, true
	}
	return 0, false
}

// handleRetentionPreview returns a dry-run report of the raw parts a retention
// pass would prune, without deleting anything. Admin-only.
func (a *API) handleRetentionPreview(w http.ResponseWriter, r *http.Request) {
	days, ok := a.retentionDaysFromRequest(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "retention window not set; pass ?days=N (or configure retention.days)")
		return
	}
	report, err := a.store.RetentionCandidates(r.Context(), time.Duration(days)*24*time.Hour)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "retention preview failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":   days,
		"report": report,
	})
}

// handleRetentionPrune deletes raw parts for reconstructable, released,
// fully-processed releases older than the retention window, in bounded batches.
// It reports how many parts were deleted. Admin-only. The deletion is bounded
// and resumable (batches), and honours the request context for cancellation.
func (a *API) handleRetentionPrune(w http.ResponseWriter, r *http.Request) {
	days, ok := a.retentionDaysFromRequest(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "retention window not set; pass ?days=N (or configure retention.days)")
		return
	}
	batchSize := a.retentionBatchSize
	if v := r.URL.Query().Get("batch_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			batchSize = n
		}
	}
	// A manual prune drains fully by default; callers can bound it with
	// ?max_batches=N.
	maxBatches := 0
	if v := r.URL.Query().Get("max_batches"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxBatches = n
		}
	}

	deleted, err := a.store.PruneRetainedPartsAll(r.Context(),
		time.Duration(days)*24*time.Hour, batchSize, maxBatches)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "retention prune failed")
		return
	}
	a.log.Info("retention prune completed", "days", days, "parts_deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{
		"days":          days,
		"parts_deleted": deleted,
	})
}
