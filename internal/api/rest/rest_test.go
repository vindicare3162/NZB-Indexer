package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/auth"
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

	createdGroup  string
	deletedGroup  int64
	setActiveID   int64
	setActiveVal  bool
	createdUser   string
	deletedUser   int64
	createdKeyLbl string
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

type mockNZB struct{}

func (mockNZB) ForGUID(context.Context, string) ([]byte, string, error) {
	return []byte("<nzb/>"), "rel.nzb", nil
}

type mockJobs struct{ scanned, backfilled string }

func (m *mockJobs) TriggerScan(g string) error     { m.scanned = g; return nil }
func (m *mockJobs) TriggerBackfill(g string) error { m.backfilled = g; return nil }
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
	api := New(st, mockNZB{}, svc, svc, jobs, nil)

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

	rec = do(t, env, http.MethodGet, "/api/v1/admin/status", env.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status endpoint = %d", rec.Code)
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
