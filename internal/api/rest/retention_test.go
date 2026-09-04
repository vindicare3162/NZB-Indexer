package rest

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestRetentionPreview(t *testing.T) {
	env := setup(t)
	env.api.SetRetention(30, 5000, 0)

	// Uses the configured window (30d) when no ?days= is given.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/retention/preview", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Days   int `json:"days"`
		Report struct {
			CandidateParts int64 `json:"candidate_parts"`
			CandidateBytes int64 `json:"candidate_bytes"`
		} `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Days != 30 {
		t.Errorf("days = %d, want 30 (configured default)", resp.Days)
	}
	if resp.Report.CandidateParts != 42 {
		t.Errorf("candidate parts = %d, want 42 (from mock)", resp.Report.CandidateParts)
	}
	if env.store.retentionPreviewAge != 30*24*time.Hour {
		t.Errorf("preview age = %v, want 720h", env.store.retentionPreviewAge)
	}

	// ?days= overrides the configured default.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/retention/preview?days=7", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview override status = %d", rec.Code)
	}
	if env.store.retentionPreviewAge != 7*24*time.Hour {
		t.Errorf("override age = %v, want 168h", env.store.retentionPreviewAge)
	}
}

func TestRetentionPreviewRequiresWindow(t *testing.T) {
	env := setup(t)
	// No configured window and no ?days= -> 400.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/retention/preview", env.adminTok, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when no window set", rec.Code)
	}
}

func TestRetentionPrune(t *testing.T) {
	env := setup(t)
	env.api.SetRetention(30, 1000, 0)

	rec := do(t, env, http.MethodPost, "/api/v1/admin/retention/prune?batch_size=250", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("prune status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Days         int   `json:"days"`
		PartsDeleted int64 `json:"parts_deleted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PartsDeleted != 17 {
		t.Errorf("parts_deleted = %d, want 17 (from mock)", resp.PartsDeleted)
	}
	if env.store.retentionPruneBatch != 250 {
		t.Errorf("batch size = %d, want 250 (from query)", env.store.retentionPruneBatch)
	}
	if env.store.retentionPruneAge != 30*24*time.Hour {
		t.Errorf("prune age = %v, want 720h", env.store.retentionPruneAge)
	}
}

func TestRetentionEndpointsRequireAdmin(t *testing.T) {
	env := setup(t)
	env.api.SetRetention(30, 5000, 0)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/admin/retention/preview"},
		{http.MethodPost, "/api/v1/admin/retention/prune"},
	} {
		// Unauthenticated.
		if rec := do(t, env, tc.method, tc.path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauth = %d, want 401", tc.method, tc.path, rec.Code)
		}
		// Non-admin.
		if rec := do(t, env, tc.method, tc.path, env.userTok, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s user = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
}
