package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/store"
)

// mockStore implements the rest.Store interface for handler tests.
type mockStore struct {
	releases   []store.Release
	total      int
	lastFilter store.SearchFilter
	files      []store.ReleaseFile
	cats      []store.Category
	groups    []store.Group
	users     []store.User
	keys      []store.APIKey
	servers2  []store.Server
	userCount int64

	createdGroup  string
	deletedGroup  int64
	setActiveID   int64
	setActiveVal  bool
	createdUser   string
	deletedUser   int64
	createdKeyLbl string
	createdServer    string
	updatedServer    int64
	deletedServer    int64
	backfillGroupID  int64
	backfillDays     *int
	backfillArticles *int64
}

func (m *mockStore) SearchReleases(_ context.Context, f store.SearchFilter) ([]store.Release, int, error) {
	m.lastFilter = f
	return m.releases, m.total, nil
}
func (m *mockStore) GetReleaseByGUID(_ context.Context, guid string) (store.Release, error) {
	for _, r := range m.releases {
		if r.GUID == guid {
			return r, nil
		}
	}
	return store.Release{}, store.ErrNotFound
}
func (m *mockStore) GetReleaseFiles(context.Context, int64) ([]store.ReleaseFile, error) {
	return m.files, nil
}
func (m *mockStore) ListCategories(context.Context) ([]store.Category, error) { return m.cats, nil }
func (m *mockStore) PipelineStatistics(context.Context) (store.PipelineStats, error) {
	return store.PipelineStats{
		PartsTotal: 1000, PartsUnassigned: 200,
		BinariesTotal: 50, BinariesComplete: 30, BinariesUnreleased: 5,
		ReleasesTotal: 25, ReleasesByPP: map[string]int64{"pending": 4, "done": 21},
		ReleasesFailedExhausted: 2,
	}, nil
}
func (m *mockStore) ListGroups(context.Context, bool) ([]store.Group, error)  { return m.groups, nil }
func (m *mockStore) UpsertGroup(_ context.Context, name string, active bool) (store.Group, error) {
	m.createdGroup = name
	return store.Group{ID: 1, Name: name, Active: active}, nil
}
func (m *mockStore) SetGroupActive(_ context.Context, id int64, active bool) error {
	m.setActiveID, m.setActiveVal = id, active
	return nil
}
func (m *mockStore) DeleteGroup(_ context.Context, id int64) error { m.deletedGroup = id; return nil }
func (m *mockStore) SetGroupBackfillTarget(_ context.Context, id int64, days *int, articles *int64) error {
	m.backfillGroupID = id
	m.backfillDays = days
	m.backfillArticles = articles
	return nil
}
func (m *mockStore) CountUsers(context.Context) (int64, error) { return m.userCount, nil }
func (m *mockStore) ListUsers(context.Context) ([]store.User, error) { return m.users, nil }
func (m *mockStore) CreateUser(_ context.Context, in store.CreateUserInput) (store.User, error) {
	m.createdUser = in.Username
	return store.User{ID: 2, Username: in.Username, Role: in.Role}, nil
}
func (m *mockStore) DeleteUser(_ context.Context, id int64) error { m.deletedUser = id; return nil }
func (m *mockStore) GetUserByID(_ context.Context, id int64) (store.User, error) {
	return store.User{ID: id}, nil
}
func (m *mockStore) ListAPIKeys(context.Context, int64) ([]store.APIKey, error) { return m.keys, nil }
func (m *mockStore) CreateAPIKey(_ context.Context, userID int64, apiKey, label string) (store.APIKey, error) {
	m.createdKeyLbl = label
	return store.APIKey{ID: 9, UserID: userID, APIKey: apiKey, Label: label}, nil
}
func (m *mockStore) DeleteAPIKey(context.Context, int64, int64) error { return nil }
func (m *mockStore) ListServers(context.Context) ([]store.Server, error) { return m.servers2, nil }
func (m *mockStore) CreateServer(_ context.Context, in store.ServerInput) (store.Server, error) {
	m.createdServer = in.Host
	return store.Server{ID: 1, Name: in.Name, Host: in.Host}, nil
}
func (m *mockStore) UpdateServer(_ context.Context, id int64, in store.ServerInput) (store.Server, error) {
	m.updatedServer = id
	return store.Server{ID: id, Name: in.Name, Host: in.Host}, nil
}
func (m *mockStore) DeleteServer(_ context.Context, id int64) error { m.deletedServer = id; return nil }

type mockDiscoverer struct{}

func (mockDiscoverer) SearchGroups(_ context.Context, query string, limit, offset int, refresh bool) ([]DiscoveredGroup, int, time.Time, error) {
	all := []DiscoveredGroup{
		{Name: "alt.binaries.movies", EstimatedCount: 5000, Status: "y"},
		{Name: "alt.binaries.tv", EstimatedCount: 3000, Status: "y"},
		{Name: "comp.lang.go", EstimatedCount: 100, Status: "y"},
	}
	var matched []DiscoveredGroup
	for _, g := range all {
		if query == "" || strings.Contains(g.Name, query) {
			matched = append(matched, g)
		}
	}
	return matched, len(matched), time.Unix(1700000000, 0), nil
}

type mockLogs struct{}

func (mockLogs) Recent(limit int, minLevel *slog.Level) []logbuf.Entry {
	all := []logbuf.Entry{
		{Level: "INFO", Message: "info one"},
		{Level: "WARN", Message: "warn one"},
		{Level: "ERROR", Message: "error one"},
	}
	var out []logbuf.Entry
	for _, e := range all {
		if minLevel != nil {
			lv := slog.LevelInfo
			switch e.Level {
			case "WARN":
				lv = slog.LevelWarn
			case "ERROR":
				lv = slog.LevelError
			}
			if lv < *minLevel {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

type mockNZB struct{}

func (mockNZB) ForGUID(context.Context, string) ([]byte, string, error) {
	return []byte("<nzb/>"), "rel.nzb", nil
}

type mockJobs struct {
	scanned, backfilled string
	postProcessed       int
}

func (m *mockJobs) TriggerScan(g string) error     { m.scanned = g; return nil }
func (m *mockJobs) TriggerBackfill(g string) error { m.backfilled = g; return nil }
func (m *mockJobs) TriggerPostProcess() error      { m.postProcessed++; return nil }
func (m *mockJobs) Status() any                    { return map[string]string{"state": "idle"} }

// testSetup wires an API with a real auth service and returns it plus the
// mocks and pre-minted tokens.
type testEnv struct {
	api       *API
	store     *mockStore
	jobs      *mockJobs
	adminTok  string
	userTok   string
}

func setup(t *testing.T) testEnv {
	t.Helper()
	ti, err := auth.NewTokenIssuer("secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Auth service with a repo that knows two users for login.
	adminHash, _ := auth.HashPassword("password123")
	userHash, _ := auth.HashPassword("password123")
	authRepo := &authRepoStub{users: map[string]store.User{
		"admin": {ID: 1, Username: "admin", PasswordHash: adminHash, Role: store.RoleAdmin, Active: true},
		"bob":   {ID: 2, Username: "bob", PasswordHash: userHash, Role: store.RoleUser, Active: true},
	}}
	svc := auth.NewService(authRepo, ti, auth.NewRateLimiter(time.Hour), 100)

	st := &mockStore{}
	jobs := &mockJobs{}
	api := New(st, mockNZB{}, svc, svc, jobs, nil, mockLogs{}, mockDiscoverer{}, nil)

	adminTok, _, _ := svc.Login(context.Background(), "admin", "password123")
	userTok, _, _ := svc.Login(context.Background(), "bob", "password123")
	return testEnv{api: api, store: st, jobs: jobs, adminTok: adminTok, userTok: userTok}
}

// authRepoStub satisfies auth.Repo for the service.
type authRepoStub struct{ users map[string]store.User }

func (a *authRepoStub) GetUserByUsername(_ context.Context, u string) (store.User, error) {
	if user, ok := a.users[u]; ok {
		return user, nil
	}
	return store.User{}, store.ErrNotFound
}
func (a *authRepoStub) GetAPIKeyWithUser(context.Context, string) (store.APIKeyUser, error) {
	return store.APIKeyUser{}, store.ErrNotFound
}
func (a *authRepoStub) TouchAPIKey(context.Context, int64) error { return nil }

func do(t *testing.T, env testEnv, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	env.api.Routes().ServeHTTP(rec, req)
	return rec
}

func TestLogin(t *testing.T) {
	env := setup(t)
	rec := do(t, env, http.MethodPost, "/api/v1/login", "", loginRequest{Username: "admin", Password: "password123"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp loginResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Token == "" || resp.Role != store.RoleAdmin {
		t.Errorf("login response = %+v", resp)
	}

	// Bad credentials.
	rec = do(t, env, http.MethodPost, "/api/v1/login", "", loginRequest{Username: "admin", Password: "nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad login status = %d, want 401", rec.Code)
	}
}

func TestSetupStatus(t *testing.T) {
	env := setup(t)

	// With no users, setup is required.
	env.store.userCount = 0
	rec := do(t, env, http.MethodGet, "/api/v1/setup/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]bool
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp["setup_required"] {
		t.Error("expected setup_required=true when no users")
	}

	// With users, setup is not required.
	env.store.userCount = 1
	rec = do(t, env, http.MethodGet, "/api/v1/setup/status", "", nil)
	resp = nil
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["setup_required"] {
		t.Error("expected setup_required=false when users exist")
	}
}

func TestSetupCreatesFirstAdmin(t *testing.T) {
	env := setup(t)
	env.store.userCount = 0 // fresh instance

	rec := do(t, env, http.MethodPost, "/api/v1/setup", "",
		loginRequest{Username: "firstadmin", Password: "password123"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body=%s", rec.Code, rec.Body)
	}
	if env.store.createdUser != "firstadmin" {
		t.Errorf("created user = %q, want firstadmin", env.store.createdUser)
	}
}

func TestSetupRejectedOnceUsersExist(t *testing.T) {
	env := setup(t)
	env.store.userCount = 1 // already set up

	rec := do(t, env, http.MethodPost, "/api/v1/setup", "",
		loginRequest{Username: "sneaky", Password: "password123"})
	if rec.Code != http.StatusConflict {
		t.Errorf("setup status = %d, want 409 when users already exist", rec.Code)
	}
	if env.store.createdUser == "sneaky" {
		t.Error("must not create a user when setup already completed (privilege escalation)")
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	env := setup(t)
	env.store.userCount = 0
	rec := do(t, env, http.MethodPost, "/api/v1/setup", "",
		loginRequest{Username: "admin", Password: "short"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for short password", rec.Code)
	}
}

func TestSearchRequiresAuth(t *testing.T) {
	env := setup(t)
	// No token -> 401.
	rec := do(t, env, http.MethodGet, "/api/v1/releases?q=x", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth search status = %d, want 401", rec.Code)
	}
}

func TestSearchWithFiltersAndPagination(t *testing.T) {
	env := setup(t)
	env.store.releases = []store.Release{{GUID: "g1", Name: "R1"}}
	env.store.total = 42

	rec := do(t, env, http.MethodGet, "/api/v1/releases?q=movie&cat=2000&limit=10&offset=20", env.userTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body)
	}
	var resp searchResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 42 || resp.Limit != 10 || resp.Offset != 20 {
		t.Errorf("pagination = %+v", resp)
	}
	if env.store.lastFilter.Query != "movie" || len(env.store.lastFilter.Categories) != 1 || env.store.lastFilter.Categories[0] != 2000 {
		t.Errorf("filter = %+v", env.store.lastFilter)
	}
}

func TestReleaseDetailAndDownload(t *testing.T) {
	env := setup(t)
	env.store.releases = []store.Release{{ID: 1, GUID: "g1", Name: "R1"}}

	rec := do(t, env, http.MethodGet, "/api/v1/releases/g1", env.userTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d", rec.Code)
	}

	rec = do(t, env, http.MethodGet, "/api/v1/releases/g1/nzb", env.userTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-nzb" {
		t.Errorf("content-type = %q", ct)
	}

	// Unknown release.
	rec = do(t, env, http.MethodGet, "/api/v1/releases/missing", env.userTok, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing detail status = %d, want 404", rec.Code)
	}
}

func TestAdminEndpointsRequireAdmin(t *testing.T) {
	env := setup(t)

	// A normal user is forbidden from admin routes.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/groups", env.userTok, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user->admin groups status = %d, want 403", rec.Code)
	}

	// Admin can list groups.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/groups", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("admin groups status = %d, want 200", rec.Code)
	}
}

func TestAdminGroupCRUD(t *testing.T) {
	env := setup(t)

	// Create.
	rec := do(t, env, http.MethodPost, "/api/v1/admin/groups", env.adminTok, createGroupRequest{Name: "alt.binaries.x"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create group status = %d, body=%s", rec.Code, rec.Body)
	}
	if env.store.createdGroup != "alt.binaries.x" {
		t.Errorf("created group = %q", env.store.createdGroup)
	}

	// Update (deactivate).
	active := false
	rec = do(t, env, http.MethodPatch, "/api/v1/admin/groups/1", env.adminTok, updateGroupRequest{Active: &active})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update group status = %d", rec.Code)
	}
	if env.store.setActiveID != 1 || env.store.setActiveVal != false {
		t.Errorf("setActive = (%d,%v)", env.store.setActiveID, env.store.setActiveVal)
	}

	// Delete.
	rec = do(t, env, http.MethodDelete, "/api/v1/admin/groups/1", env.adminTok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete group status = %d", rec.Code)
	}
	if env.store.deletedGroup != 1 {
		t.Errorf("deleted group = %d", env.store.deletedGroup)
	}
}

func TestAdminSetGroupBackfillTarget(t *testing.T) {
	env := setup(t)

	days := 30
	arts := int64(50000)
	rec := do(t, env, http.MethodPut, "/api/v1/admin/groups/7/backfill", env.adminTok,
		backfillTargetRequest{Days: &days, Articles: &arts})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set backfill status = %d, body=%s", rec.Code, rec.Body)
	}
	if env.store.backfillGroupID != 7 {
		t.Errorf("group id = %d, want 7", env.store.backfillGroupID)
	}
	if env.store.backfillDays == nil || *env.store.backfillDays != 30 {
		t.Errorf("days = %v, want 30", env.store.backfillDays)
	}
	if env.store.backfillArticles == nil || *env.store.backfillArticles != 50000 {
		t.Errorf("articles = %v, want 50000", env.store.backfillArticles)
	}

	// Negative rejected.
	neg := -5
	rec = do(t, env, http.MethodPut, "/api/v1/admin/groups/7/backfill", env.adminTok,
		backfillTargetRequest{Days: &neg})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("negative days status = %d, want 400", rec.Code)
	}

	// Non-admin forbidden.
	rec = do(t, env, http.MethodPut, "/api/v1/admin/groups/7/backfill", env.userTok,
		backfillTargetRequest{Days: &days})
	if rec.Code != http.StatusForbidden {
		t.Errorf("user status = %d, want 403", rec.Code)
	}
}

func TestAdminTriggersAndStatus(t *testing.T) {
	env := setup(t)

	rec := do(t, env, http.MethodPost, "/api/v1/admin/scan", env.adminTok, triggerRequest{Group: "alt.binaries.x"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scan trigger status = %d", rec.Code)
	}
	if env.jobs.scanned != "alt.binaries.x" {
		t.Errorf("scanned = %q", env.jobs.scanned)
	}

	rec = do(t, env, http.MethodPost, "/api/v1/admin/backfill", env.adminTok, triggerRequest{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("backfill trigger status = %d", rec.Code)
	}

	rec = do(t, env, http.MethodPost, "/api/v1/admin/postprocess", env.adminTok, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("postprocess trigger status = %d", rec.Code)
	}
	if env.jobs.postProcessed != 1 {
		t.Errorf("postProcessed = %d, want 1", env.jobs.postProcessed)
	}

	rec = do(t, env, http.MethodGet, "/api/v1/admin/status", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d", rec.Code)
	}

	rec = do(t, env, http.MethodGet, "/api/v1/admin/stats", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats endpoint = %d", rec.Code)
	}
	var stats store.PipelineStats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.PartsUnassigned != 200 || stats.BinariesUnreleased != 5 || stats.ReleasesByPP["pending"] != 4 {
		t.Errorf("stats = %+v", stats)
	}
	if stats.ReleasesFailedExhausted != 2 {
		t.Errorf("releases_failed_exhausted = %d, want 2", stats.ReleasesFailedExhausted)
	}
}

func TestAdminServerCRUD(t *testing.T) {
	env := setup(t)
	env.store.servers2 = []store.Server{
		{ID: 1, Name: "primary", Host: "news.example.com", Port: 563, TLS: true, Username: "u", Password: "secret"},
	}

	// List must never leak the password, but should flag that one is set.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/servers", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list servers status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret") {
		t.Errorf("server list leaked password:\n%s", body)
	}
	if !strings.Contains(body, `"has_password":true`) {
		t.Errorf("expected has_password flag:\n%s", body)
	}

	// Create.
	pw := "newpass"
	rec = do(t, env, http.MethodPost, "/api/v1/admin/servers", env.adminTok, serverRequest{
		Name: "block", Host: "block.example.com", Port: 563, TLS: true, Username: "u2", Password: &pw, Enabled: true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create server status = %d, body=%s", rec.Code, rec.Body)
	}
	if env.store.createdServer != "block.example.com" {
		t.Errorf("created server host = %q", env.store.createdServer)
	}

	// Update.
	rec = do(t, env, http.MethodPut, "/api/v1/admin/servers/1", env.adminTok, serverRequest{
		Name: "primary", Host: "news2.example.com", Port: 563, TLS: true, Enabled: true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update server status = %d", rec.Code)
	}
	if env.store.updatedServer != 1 {
		t.Errorf("updated server id = %d", env.store.updatedServer)
	}

	// Delete.
	rec = do(t, env, http.MethodDelete, "/api/v1/admin/servers/1", env.adminTok, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete server status = %d", rec.Code)
	}
	if env.store.deletedServer != 1 {
		t.Errorf("deleted server id = %d", env.store.deletedServer)
	}

	// A non-admin cannot manage servers.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/servers", env.userTok, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user->servers status = %d, want 403", rec.Code)
	}
}

func TestAdminDiscover(t *testing.T) {
	env := setup(t)

	// No filter: all groups.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/discover", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status = %d", rec.Code)
	}
	var resp discoverResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 3 || len(resp.Groups) != 3 {
		t.Errorf("discover total/groups = %d/%d, want 3/3", resp.Total, len(resp.Groups))
	}
	if resp.CachedAt == "" {
		t.Error("expected cached_at timestamp")
	}

	// Filtered query.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/discover?q=binaries", env.adminTok, nil)
	resp = discoverResponse{}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 2 {
		t.Errorf("filtered total = %d, want 2", resp.Total)
	}

	// Admin-only.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/discover", env.userTok, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user->discover status = %d, want 403", rec.Code)
	}
}

func TestAdminLogs(t *testing.T) {
	env := setup(t)

	// All levels.
	rec := do(t, env, http.MethodGet, "/api/v1/admin/logs", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs status = %d", rec.Code)
	}
	var entries []logbuf.Entry
	json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) != 3 {
		t.Errorf("entries = %d, want 3", len(entries))
	}

	// Level filter (warn+): drops the INFO entry.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/logs?level=warn", env.adminTok, nil)
	entries = nil
	json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) != 2 {
		t.Errorf("warn-filtered entries = %d, want 2", len(entries))
	}

	// Admin-only.
	rec = do(t, env, http.MethodGet, "/api/v1/admin/logs", env.userTok, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("user->logs status = %d, want 403", rec.Code)
	}
}

func TestSelfServiceAPIKeys(t *testing.T) {
	env := setup(t)

	rec := do(t, env, http.MethodPost, "/api/v1/apikeys", env.userTok, createKeyRequest{Label: "sonarr"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key status = %d", rec.Code)
	}
	if env.store.createdKeyLbl != "sonarr" {
		t.Errorf("created key label = %q", env.store.createdKeyLbl)
	}
}
