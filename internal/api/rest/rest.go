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

	"github.com/vindicare/goindex/internal/api/openapi"
	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/notify"
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
	// SearchReleasesPage runs a paginated search with bounded count + keyset
	// pagination metadata (#120).
	SearchReleasesPage(ctx context.Context, f store.SearchFilter) (store.SearchResult, error)
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
	// RetentionCandidates reports (dry-run) the raw parts a retention pass would
	// prune for releases older than the given age.
	RetentionCandidates(ctx context.Context, olderThan time.Duration) (store.RetentionReport, error)
	// PruneRetainedParts deletes raw parts for reconstructable, released,
	// fully-processed releases older than olderThan, in bounded batches. Returns
	// the total deleted.
	PruneRetainedPartsAll(ctx context.Context, olderThan time.Duration, batchSize, maxBatches int) (int64, error)
	// Jobs history (#113).
	ListJobs(ctx context.Context, limit int) ([]store.Job, error)
	GetJob(ctx context.Context, id string) (store.Job, error)

	// Groups (admin)
	ListGroups(ctx context.Context, activeOnly bool) ([]store.Group, error)
	// ListGroupsPage returns a filtered, sorted, paginated page of groups (#123).
	ListGroupsPage(ctx context.Context, f store.GroupFilter) (store.GroupPage, error)
	GetGroupByName(ctx context.Context, name string) (store.Group, error)
	UpsertGroup(ctx context.Context, name string, active bool) (store.Group, error)
	SetGroupActive(ctx context.Context, id int64, active bool) error
	SetGroupBackfillTarget(ctx context.Context, id int64, days *int, articles *int64) error
	// SetGroupScanConfig sets a group's scan priority and forward budget (#126).
	SetGroupScanConfig(ctx context.Context, id int64, priority int, forwardArticles *int64) error
	// GroupStorageBytes estimates retained raw-part storage per group (#127).
	GroupStorageBytes(ctx context.Context, ids []int64) (map[int64]int64, error)
	// CapacityStats reports current sizes and observed ingest rate for capacity
	// planning (#131).
	CapacityStats(ctx context.Context, topN int) (store.CapacityStats, error)
	// RecentReleaseErrors + CountFailedReleases surface failed post-processing
	// releases for the diagnostics view (#133).
	RecentReleaseErrors(ctx context.Context, limit int) ([]store.ReleaseError, error)
	CountFailedReleases(ctx context.Context) (total, permanent int64, err error)
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

// LogStreamer exposes a live subscription to new log entries so they can be
// streamed to the admin UI via Server-Sent Events (#121). Optional: when nil,
// the events endpoint still streams periodic status snapshots but no live logs.
// The logbuf.Buffer satisfies it.
type LogStreamer interface {
	// Subscribe returns a channel of newly-added entries and a cancel function
	// that unregisters the subscriber. The channel is buffered; a slow consumer
	// misses entries rather than blocking log capture.
	Subscribe() (<-chan logbuf.Entry, func())
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
	// means all active groups) and returns the persistent job id (#113).
	TriggerScan(group string) (string, error)
	// TriggerBackfill requests a backfill pass and returns the job id.
	TriggerBackfill(group string) (string, error)
	// TriggerPostProcess requests an immediate post-processing pass and returns
	// the job id, so an operator can recover names for pending releases without
	// waiting for a scan or the downstream interval.
	TriggerPostProcess() (string, error)
	// CancelJob requests cooperative cancellation of a job.
	CancelJob(id string) error
	// Status returns a snapshot of job/pipeline status and metrics as a
	// JSON-serialisable value.
	Status() any
	// CurrentSchedule returns the live pipeline intervals.
	CurrentSchedule() Schedule
	// Reconfigure applies new pipeline intervals live. Any interval <= 0 is
	// left unchanged.
	Reconfigure(Schedule)
}

// JobStore is the persistence the jobs endpoints read from (#113). The
// store.Store satisfies it; nil disables the jobs listing endpoints.
type JobStore interface {
	ListJobs(ctx context.Context, limit int) ([]store.Job, error)
	GetJob(ctx context.Context, id string) (store.Job, error)
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
	logStream LogStreamer

	// Retention window/batch defaults for the admin retention endpoints (#118).
	retentionDays       int
	retentionBatchSize  int
	retentionMaxBatches int

	// Per-group health thresholds used to classify groups in the admin group
	// listing (#127). Defaults are applied in New.
	healthThresholds store.GroupHealthThresholds

	// notifier exposes webhook delivery history for the admin notifications
	// endpoint (#137). Optional; nil disables it.
	notifier NotifyHistorian

	// errHistory exposes recent in-process pipeline errors for the diagnostics
	// endpoint (#133). Optional; nil omits that section.
	errHistory ErrorHistorian

	// searchBackend optionally routes release search through a derived index
	// (OpenSearch, #139) with automatic PostgreSQL fallback. Nil (the default)
	// uses PostgreSQL directly via the store, preserving keyset pagination.
	searchBackend SearchBackend

	// reindexer optionally rebuilds the derived search index from PostgreSQL
	// (#139). Nil (the default) means no derived index is configured, so the
	// reindex endpoint reports the feature is disabled.
	reindexer Reindexer

	log *slog.Logger
}

// Reindexer rebuilds the derived release-search index from PostgreSQL (#139),
// returning the number of documents indexed. The server wires this only when
// OpenSearch is enabled.
type Reindexer interface {
	Reindex(ctx context.Context) (int, error)
}

// SearchBackend answers release searches (#139). The default PostgreSQL path
// uses the store directly; when an optional derived backend (OpenSearch) is
// configured it is used instead, with fallback handled inside the backend.
type SearchBackend interface {
	Search(ctx context.Context, f store.SearchFilter) (store.SearchResult, error)
}

// NotifyHistorian exposes recent webhook delivery outcomes (#137). The
// notify.Service satisfies it.
type NotifyHistorian interface {
	History(limit int) []notify.Delivery
}

// PipelineError mirrors worker.PipelineError for the diagnostics view (#133),
// kept here so the REST layer does not import the worker package.
type PipelineError struct {
	Seq     int64     `json:"seq"`
	Stage   string    `json:"stage,omitempty"`
	Group   string    `json:"group,omitempty"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// ErrorHistorian exposes the worker's recent in-process pipeline errors (#133).
// The server adapts worker.Worker into this. Optional; nil omits that section.
type ErrorHistorian interface {
	RecentErrors(limit int) []PipelineError
}

// SetSystemProbe attaches a health probe (NNTP pool / config facts) used by the
// admin health report. Optional; when unset those fields are omitted.
func (a *API) SetSystemProbe(p SystemProbe) { a.probe = p }

// SetLogStreamer attaches a live log subscription used by the admin
// Server-Sent Events endpoint (#121). Optional; when unset the events stream
// still delivers periodic status snapshots but no live log lines.
func (a *API) SetLogStreamer(s LogStreamer) { a.logStream = s }

// SetRetention configures the raw-part retention window and batch defaults used
// by the admin retention endpoints. days<=0 means the endpoints require an
// explicit ?days= override.
func (a *API) SetRetention(days, batchSize, maxBatches int) {
	a.retentionDays = days
	a.retentionBatchSize = batchSize
	a.retentionMaxBatches = maxBatches
}

// SetGroupHealthThresholds configures the thresholds used to classify per-group
// health in the admin group listing (#127). A zero-value struct field disables
// that check; call with DefaultGroupHealthThresholds() to restore defaults.
func (a *API) SetGroupHealthThresholds(t store.GroupHealthThresholds) {
	a.healthThresholds = t
}

// SetNotifier attaches the webhook notifier so the admin notifications endpoint
// can report recent delivery history (#137). Optional; nil disables the
// endpoint.
func (a *API) SetNotifier(n NotifyHistorian) { a.notifier = n }

// SetErrorHistorian attaches the worker's recent-error source for the admin
// diagnostics endpoint (#133). Optional; nil omits the in-process error list.
func (a *API) SetErrorHistorian(e ErrorHistorian) { a.errHistory = e }

// SetSearchBackend routes release search through an optional derived backend
// (OpenSearch, #139). When nil (the default) search uses PostgreSQL directly.
func (a *API) SetSearchBackend(b SearchBackend) { a.searchBackend = b }

// SetReindexer attaches the derived-index rebuild trigger for the admin reindex
// endpoint (#139). Optional; nil reports the feature is disabled.
func (a *API) SetReindexer(r Reindexer) { a.reindexer = r }

// New creates a REST API. servers, logs, and discoverer may be nil, disabling
// their respective endpoints.
func New(st Store, nzb NZBGenerator, authn Authenticator, session *auth.Service, jobs JobController, servers ServerManager, logs LogSource, discoverer Discoverer, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{store: st, nzb: nzb, authn: authn, jobs: jobs, servers: servers, logs: logs, discoverer: discoverer, session: session, healthThresholds: store.DefaultGroupHealthThresholds(), log: log}
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
	// OpenAPI spec + docs (#138): public API documentation, no auth.
	mux.HandleFunc("GET /api/v1/openapi.yaml", openapi.SpecHandler)
	mux.HandleFunc("GET /api/v1/docs", openapi.DocsHandler)

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
	mux.Handle("PUT /api/v1/admin/groups/{id}/scan-config", admin(http.HandlerFunc(a.handleSetGroupScanConfig)))
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
	mux.Handle("GET /api/v1/admin/jobs", admin(http.HandlerFunc(a.handleListJobs)))
	mux.Handle("GET /api/v1/admin/jobs/{id}", admin(http.HandlerFunc(a.handleGetJob)))
	mux.Handle("POST /api/v1/admin/jobs/{id}/cancel", admin(http.HandlerFunc(a.handleCancelJob)))
	mux.Handle("POST /api/v1/admin/segments/backfill", admin(http.HandlerFunc(a.handleBackfillSegments)))
	mux.Handle("GET /api/v1/admin/retention/preview", admin(http.HandlerFunc(a.handleRetentionPreview)))
	mux.Handle("POST /api/v1/admin/retention/prune", admin(http.HandlerFunc(a.handleRetentionPrune)))
	mux.Handle("POST /api/v1/admin/postprocess/retry-failed", admin(http.HandlerFunc(a.handleRetryFailed)))
	mux.Handle("GET /api/v1/admin/status", admin(http.HandlerFunc(a.handleStatus)))
	mux.Handle("GET /api/v1/admin/stats", admin(http.HandlerFunc(a.handleStats)))
	mux.Handle("GET /api/v1/admin/logs", admin(http.HandlerFunc(a.handleLogs)))
	mux.Handle("GET /api/v1/admin/events", admin(http.HandlerFunc(a.handleEvents)))
	mux.Handle("GET /api/v1/admin/overview", admin(http.HandlerFunc(a.handleAdminOverview)))
	mux.Handle("GET /api/v1/admin/discover", admin(http.HandlerFunc(a.handleDiscover)))
	mux.Handle("GET /api/v1/admin/notifications", admin(http.HandlerFunc(a.handleNotifications)))
	mux.Handle("GET /api/v1/admin/capacity", admin(http.HandlerFunc(a.handleCapacity)))
	mux.Handle("GET /api/v1/admin/diagnostics", admin(http.HandlerFunc(a.handleDiagnostics)))
	mux.Handle("POST /api/v1/admin/search/reindex", admin(http.HandlerFunc(a.handleSearchReindex)))

	return mux
}

// handleNotifications returns recent webhook delivery outcomes (#137).
func (a *API) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if a.notifier == nil {
		writeJSON(w, http.StatusOK, []notify.Delivery{})
		return
	}
	limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
	deliveries := a.notifier.History(limit)
	if deliveries == nil {
		deliveries = []notify.Delivery{}
	}
	writeJSON(w, http.StatusOK, deliveries)
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
