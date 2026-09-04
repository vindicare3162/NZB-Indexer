// Package rest implements the JSON HTTP API that backs the goindex SPA. It
// exposes public authentication, session-protected search and downloads, and
// admin-protected configuration and job control.
package rest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/store"
)

// Store is the subset of persistence the REST API needs.
type Store interface {
	// Ping verifies the database is reachable (readiness probe).
	Ping(ctx context.Context) error

	// Settings (runtime-configurable options, e.g. the pipeline schedule).
	SetSettings(ctx context.Context, kv map[string]string) error

	// Search / details
	SearchReleases(ctx context.Context, f store.SearchFilter) ([]store.Release, int, error)
	GetReleaseByGUID(ctx context.Context, guid string) (store.Release, error)
	GetReleaseFiles(ctx context.Context, releaseID int64) ([]store.ReleaseFile, error)
	GetReleaseMetadata(ctx context.Context, releaseID int64) (store.ReleaseMetadata, error)
	GetReleaseIdentifiers(ctx context.Context, releaseID int64) ([]store.ReleaseIdentifier, error)
	ListCategories(ctx context.Context) ([]store.Category, error)

	// Pipeline health (admin)
	PipelineStatistics(ctx context.Context) (store.PipelineStats, error)
	// DatabaseHealth reports DB size, cache hit ratio, and pool utilisation.
	DatabaseHealth(ctx context.Context) (store.DBHealth, error)
	// RequeueFailedReleases resets failed post-processing releases to pending.
	RequeueFailedReleases(ctx context.Context) (int64, error)
	// BackfillReleaseSegments snapshots durable segments for legacy releases
	// that lack them (retention prerequisite). Returns repaired + unresolved.
	BackfillReleaseSegments(ctx context.Context, limit int) (repaired, unresolved int, err error)

	// Groups (admin)
	ListGroups(ctx context.Context, activeOnly bool) ([]store.Group, error)
	GetGroupByName(ctx context.Context, name string) (store.Group, error)
	UpsertGroup(ctx context.Context, name string, active bool) (store.Group, error)
	SetGroupActive(ctx context.Context, id int64, active bool) error
	SetGroupBackfillTarget(ctx context.Context, id int64, days *int, articles *int64) error
	DeleteGroup(ctx context.Context, id int64) error

	// News servers (admin)
	ListServers(ctx context.Context) ([]store.Server, error)
	CreateServer(ctx context.Context, in store.ServerInput) (store.Server, error)
	UpdateServer(ctx context.Context, id int64, in store.ServerInput) (store.Server, error)
	DeleteServer(ctx context.Context, id int64) error

	// Users & API keys (admin / self)
	CountUsers(ctx context.Context) (int64, error)
	ListUsers(ctx context.Context) ([]store.User, error)
	CreateUser(ctx context.Context, in store.CreateUserInput) (store.User, error)
	DeleteUser(ctx context.Context, id int64) error
	GetUserByID(ctx context.Context, id int64) (store.User, error)
	ListAPIKeys(ctx context.Context, userID int64) ([]store.APIKey, error)
	CreateAPIKey(ctx context.Context, userID int64, apiKey, label string) (store.APIKey, error)
	DeleteAPIKey(ctx context.Context, userID, keyID int64) error
}

// NZBGenerator builds an NZB for a release GUID.
type NZBGenerator interface {
	ForGUID(ctx context.Context, guid string) (data []byte, filename string, err error)
}

// DiscoveredGroup is a newsgroup the provider carries, returned by discovery.
type DiscoveredGroup struct {
	Name           string `json:"name"`
	EstimatedCount int64  `json:"estimated_count"`
	Status         string `json:"status"`
}

// Discoverer searches the provider's carried newsgroups. A nil discoverer
// disables the discovery endpoint.
type Discoverer interface {
	// SearchGroups returns a filtered, paginated page of groups plus the total
	// match count and the cache timestamp. refresh forces a cache reload.
	SearchGroups(ctx context.Context, query string, limit, offset int, refresh bool) (groups []DiscoveredGroup, total int, cachedAt time.Time, err error)
}

// LogSource exposes recent captured log entries for the admin log view. A nil
// source disables the logs endpoint.
type LogSource interface {
	// Recent returns up to limit newest-first entries at or above minLevel
	// (minLevel nil = all levels).
	Recent(limit int, minLevel *slog.Level) []logbuf.Entry
}

// ServerManager applies the currently-active news server to the running NNTP
// pool. The server package implements it; a nil manager means server changes
// take effect only on restart.
type ServerManager interface {
	// ApplyActive reloads the active server from the store and reconfigures the
	// live NNTP pool.
	ApplyActive(ctx context.Context) error
}

// JobController triggers pipeline jobs and reports their status. The worker
// scheduler (Task 13) implements this; a nil controller disables the endpoints.
type JobController interface {
	// TriggerScan requests a forward scan (optionally of a single group; empty
	// means all active groups).
	TriggerScan(group string) error
	// TriggerBackfill requests a backfill pass.
	TriggerBackfill(group string) error
	// TriggerPostProcess requests an immediate post-processing pass, so an
	// operator can recover names for pending releases without waiting for a
	// scan or the downstream interval.
	TriggerPostProcess() error
	// Status returns a snapshot of job/pipeline status and metrics as a
	// JSON-serialisable value.
	Status() any
	// CurrentSchedule returns the live pipeline intervals.
	CurrentSchedule() Schedule
	// Reconfigure applies new pipeline intervals live. Any interval <= 0 is
	// left unchanged.
	Reconfigure(Schedule)
}

// Schedule holds the runtime-tunable pipeline intervals. It mirrors the
// worker's schedule but lives here so the REST layer does not import the worker
// package.
type Schedule struct {
	ScanInterval        time.Duration
	DownstreamInterval  time.Duration
	BuildInterval       time.Duration
	PostProcessInterval time.Duration
}

// Authenticator authenticates login credentials and issues sessions.
type Authenticator interface {
	Login(ctx context.Context, username, password string) (token string, p auth.Principal, err error)
}

// API bundles the REST handler dependencies.
type API struct {
	store   Store
	nzb     NZBGenerator
	authn   Authenticator
	jobs      JobController
	servers   ServerManager
	logs      LogSource
	discoverer Discoverer
	session   *auth.Service
	probe     SystemProbe
	log       *slog.Logger
}

// SetSystemProbe attaches a health probe (NNTP pool / config facts) used by the
// admin health report. Optional; when unset those fields are omitted.
func (a *API) SetSystemProbe(p SystemProbe) { a.probe = p }

// New creates a REST API. servers, logs, and discoverer may be nil, disabling
// their respective endpoints.
func New(st Store, nzb NZBGenerator, authn Authenticator, session *auth.Service, jobs JobController, servers ServerManager, logs LogSource, discoverer Discoverer, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{store: st, nzb: nzb, authn: authn, jobs: jobs, servers: servers, logs: logs, discoverer: discoverer, session: session, log: log}
}

// Routes returns the REST API mux mounted under /api/v1.
func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("POST /api/v1/login", a.handleLogin)
	mux.HandleFunc("GET /api/v1/health", a.handleHealth)
	mux.HandleFunc("GET /api/v1/ready", a.handleReady)
	mux.HandleFunc("GET /api/v1/setup/status", a.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/setup", a.handleSetup)

	// Session-protected (any authenticated user).
	sess := a.session.RequireSession
	mux.Handle("GET /api/v1/me", sess(http.HandlerFunc(a.handleMe)))
	mux.Handle("GET /api/v1/categories", sess(http.HandlerFunc(a.handleCategories)))
	mux.Handle("GET /api/v1/releases", sess(http.HandlerFunc(a.handleSearch)))
	mux.Handle("GET /api/v1/releases/{guid}", sess(http.HandlerFunc(a.handleReleaseDetail)))
	mux.Handle("GET /api/v1/releases/{guid}/nzb", sess(http.HandlerFunc(a.handleDownload)))
	mux.Handle("GET /api/v1/apikeys", sess(http.HandlerFunc(a.handleListMyKeys)))
	mux.Handle("POST /api/v1/apikeys", sess(http.HandlerFunc(a.handleCreateMyKey)))
	mux.Handle("DELETE /api/v1/apikeys/{id}", sess(http.HandlerFunc(a.handleDeleteMyKey)))

	// Admin-protected.
	admin := a.session.RequireAdmin
	mux.Handle("GET /api/v1/admin/groups", admin(http.HandlerFunc(a.handleListGroups)))
	mux.Handle("POST /api/v1/admin/groups", admin(http.HandlerFunc(a.handleCreateGroup)))
	mux.Handle("POST /api/v1/admin/groups/bulk", admin(http.HandlerFunc(a.handleBulkGroups)))
	mux.Handle("PATCH /api/v1/admin/groups/{id}", admin(http.HandlerFunc(a.handleUpdateGroup)))
	mux.Handle("PUT /api/v1/admin/groups/{id}/backfill", admin(http.HandlerFunc(a.handleSetGroupBackfill)))
	mux.Handle("DELETE /api/v1/admin/groups/{id}", admin(http.HandlerFunc(a.handleDeleteGroup)))
	mux.Handle("GET /api/v1/admin/users", admin(http.HandlerFunc(a.handleListUsers)))
	mux.Handle("POST /api/v1/admin/users", admin(http.HandlerFunc(a.handleCreateUser)))
	mux.Handle("DELETE /api/v1/admin/users/{id}", admin(http.HandlerFunc(a.handleDeleteUser)))
	mux.Handle("GET /api/v1/admin/servers", admin(http.HandlerFunc(a.handleListServers)))
	mux.Handle("POST /api/v1/admin/servers", admin(http.HandlerFunc(a.handleCreateServer)))
	mux.Handle("PUT /api/v1/admin/servers/{id}", admin(http.HandlerFunc(a.handleUpdateServer)))
	mux.Handle("DELETE /api/v1/admin/servers/{id}", admin(http.HandlerFunc(a.handleDeleteServer)))
	mux.Handle("GET /api/v1/admin/health", admin(http.HandlerFunc(a.handleHealthReport)))
	mux.Handle("GET /api/v1/admin/schedule", admin(http.HandlerFunc(a.handleGetSchedule)))
	mux.Handle("PUT /api/v1/admin/schedule", admin(http.HandlerFunc(a.handleUpdateSchedule)))
	mux.Handle("POST /api/v1/admin/scan", admin(http.HandlerFunc(a.handleTriggerScan)))
	mux.Handle("POST /api/v1/admin/backfill", admin(http.HandlerFunc(a.handleTriggerBackfill)))
	mux.Handle("POST /api/v1/admin/postprocess", admin(http.HandlerFunc(a.handleTriggerPostProcess)))
	mux.Handle("POST /api/v1/admin/segments/backfill", admin(http.HandlerFunc(a.handleBackfillSegments)))
	mux.Handle("POST /api/v1/admin/postprocess/retry-failed", admin(http.HandlerFunc(a.handleRetryFailed)))
	mux.Handle("GET /api/v1/admin/status", admin(http.HandlerFunc(a.handleStatus)))
	mux.Handle("GET /api/v1/admin/stats", admin(http.HandlerFunc(a.handleStats)))
	mux.Handle("GET /api/v1/admin/logs", admin(http.HandlerFunc(a.handleLogs)))
	mux.Handle("GET /api/v1/admin/overview", admin(http.HandlerFunc(a.handleAdminOverview)))
	mux.Handle("GET /api/v1/admin/discover", admin(http.HandlerFunc(a.handleDiscover)))

	return mux
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
