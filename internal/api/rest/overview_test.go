package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/vindicare/goindex/internal/store"
)

// overviewBody is a partial decode of the overview envelope for assertions.
type overviewBody struct {
	GeneratedAt string            `json:"generated_at"`
	Health      json.RawMessage   `json:"health"`
	Status      json.RawMessage   `json:"status"`
	Stats       json.RawMessage   `json:"stats"`
	Groups      []store.Group     `json:"groups"`
	Servers     []struct {
		HasPassword bool   `json:"has_password"`
		Password    string `json:"password"`
	} `json:"servers"`
	Users    []map[string]any  `json:"users"`
	Schedule json.RawMessage   `json:"schedule"`
	Logs     []map[string]any  `json:"logs"`
	Errors   map[string]string `json:"errors"`
}

func TestAdminOverviewShape(t *testing.T) {
	env := setup(t)
	env.store.groups = []store.Group{{ID: 1, Name: "alt.binaries.tv", Active: true}}
	env.store.servers2 = []store.Server{{ID: 1, Name: "primary", Host: "news.example.com", Password: "secret"}}

	rec := do(t, env, http.MethodGet, "/api/v1/admin/overview", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, body=%s", rec.Code, rec.Body)
	}

	var body overviewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode overview: %v", err)
	}

	if body.GeneratedAt == "" {
		t.Error("generated_at should be populated")
	}
	if len(body.Health) == 0 || string(body.Health) == "null" {
		t.Error("health should be present")
	}
	if len(body.Status) == 0 {
		t.Error("status should be present")
	}
	if len(body.Stats) == 0 || string(body.Stats) == "null" {
		t.Error("stats should be present")
	}
	if len(body.Schedule) == 0 || string(body.Schedule) == "null" {
		t.Error("schedule should be present (jobs controller wired)")
	}
	if len(body.Groups) != 1 || body.Groups[0].Name != "alt.binaries.tv" {
		t.Errorf("groups = %+v, want the one group", body.Groups)
	}
	if len(body.Logs) == 0 {
		t.Error("logs should be present")
	}
	if body.Errors != nil {
		t.Errorf("errors should be nil when everything loads, got %v", body.Errors)
	}

	// Servers must never leak the password; only signal that one is set.
	if len(body.Servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(body.Servers))
	}
	if !body.Servers[0].HasPassword {
		t.Error("server has_password should be true")
	}
	if body.Servers[0].Password != "" {
		t.Errorf("server password must not be serialised, got %q", body.Servers[0].Password)
	}
	// Raw-body safety net: the secret string must not appear anywhere.
	if got := rec.Body.String(); containsSecret(got, "secret") {
		t.Error("overview response leaked the server password")
	}
}

func containsSecret(body, secret string) bool {
	// The password value "secret" should never appear. (Field names/other words
	// won't match this exact token in the fixtures used here.)
	return len(secret) > 0 && (indexOf(body, secret) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestAdminOverviewRequiresAdmin(t *testing.T) {
	env := setup(t)

	// Unauthenticated.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/overview", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec.Code)
	}

	// Non-admin user.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/overview", env.userTok, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user status = %d, want 403", rec.Code)
	}
}

func TestAdminOverviewPartialFailure(t *testing.T) {
	env := setup(t)
	env.store.groups = []store.Group{{ID: 1, Name: "alt.binaries.tv"}}
	// Make only the pipeline statistics subsystem fail.
	env.store.statsErr = errors.New("stats query failed")

	rec := do(t, env, http.MethodGet, "/api/v1/admin/overview", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200 despite partial failure; body=%s", rec.Code, rec.Body)
	}

	var body overviewBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The failed section is reported in errors...
	if body.Errors == nil || body.Errors["stats"] == "" {
		t.Errorf("expected a stats error entry, got errors=%v", body.Errors)
	}
	// ...and its data is null, but unrelated sections still loaded.
	if string(body.Stats) != "null" && len(body.Stats) != 0 {
		t.Errorf("stats should be null on failure, got %s", body.Stats)
	}
	if len(body.Groups) != 1 {
		t.Errorf("unrelated groups section should still load, got %+v", body.Groups)
	}
	if len(body.Health) == 0 || string(body.Health) == "null" {
		t.Error("health should still be present on partial failure")
	}
}
