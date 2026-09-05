package rest

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/store"
)

// --- groups ---

func (a *API) handleListGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.GroupFilter{
		Search:     strings.TrimSpace(q.Get("q")),
		Status:     q.Get("status"),
		ErrorsOnly: q.Get("errors") == "true" || q.Get("errors") == "1",
		Sort:       q.Get("sort"),
		Desc:       q.Get("order") == "desc",
		Limit:      parseIntDefault(q.Get("limit"), 50),
		Offset:     parseIntDefault(q.Get("offset"), 0),
	}
	page, err := a.store.ListGroupsPage(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}
	if page.Groups == nil {
		page.Groups = []store.Group{}
	}

	// Enrich each group with derived health and estimated storage (#127). The
	// storage figures come from one batch query over the page's group ids.
	ids := make([]int64, len(page.Groups))
	for i, g := range page.Groups {
		ids[i] = g.ID
	}
	storage, err := a.store.GroupStorageBytes(r.Context(), ids)
	if err != nil {
		// Storage is best-effort context; log-free degradation keeps the list
		// usable even if the aggregate query fails.
		storage = map[int64]int64{}
	}
	now := time.Now()
	items := make([]groupWithHealth, len(page.Groups))
	for i, g := range page.Groups {
		items[i] = groupWithHealth{
			Group:        g,
			Health:       store.ClassifyGroupHealth(g, a.healthThresholds, now),
			StorageBytes: storage[g.ID],
		}
	}
	writeJSON(w, http.StatusOK, groupHealthPage{
		Groups: items,
		Total:  page.Total,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

// diagnosticsResponse aggregates recent pipeline errors from every durable and
// in-process source into one actionable view (#133).
type diagnosticsResponse struct {
	// PipelineErrors is the worker's recent in-process error history (bounded,
	// process-lifetime scope).
	PipelineErrors []PipelineError `json:"pipeline_errors"`
	// GroupErrors are active groups whose most recent scan failed.
	GroupErrors []store.Group `json:"group_errors"`
	// ReleaseErrors are releases stuck in failed post-processing.
	ReleaseErrors []store.ReleaseError `json:"release_errors"`
	// FailedJobs are recent pipeline jobs that ended in the failed state.
	FailedJobs []store.Job `json:"failed_jobs"`
	// Summary carries counts + the most actionable remediation hints.
	Summary diagnosticsSummary `json:"summary"`
}

// diagnosticsSummary holds counts and actionable hints for the diagnostics view.
type diagnosticsSummary struct {
	GroupErrorCount        int    `json:"group_error_count"`
	FailedReleaseCount     int64  `json:"failed_release_count"`
	PermanentReleaseCount  int64  `json:"permanent_release_count"`
	RetryableReleaseHint   string `json:"retryable_release_hint,omitempty"`
	GroupErrorHint         string `json:"group_error_hint,omitempty"`
}

// handleDiagnostics aggregates recent pipeline errors from the worker's
// in-process history, per-group scan errors (#114), failed post-processing
// releases (#132), and failed jobs (#113) into one actionable view (#133).
// Each section degrades independently so one failing source never blanks the
// rest.
func (a *API) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
	var resp diagnosticsResponse

	if a.errHistory != nil {
		resp.PipelineErrors = a.errHistory.RecentErrors(limit)
	}
	if resp.PipelineErrors == nil {
		resp.PipelineErrors = []PipelineError{}
	}

	// Groups whose last scan errored (reuse the paginated, errors-only filter).
	if page, err := a.store.ListGroupsPage(ctx, store.GroupFilter{ErrorsOnly: true, Limit: limit}); err == nil {
		resp.GroupErrors = page.Groups
		resp.Summary.GroupErrorCount = page.Total
	}
	if resp.GroupErrors == nil {
		resp.GroupErrors = []store.Group{}
	}

	// Failed post-processing releases + counts.
	if rel, err := a.store.RecentReleaseErrors(ctx, limit); err == nil {
		resp.ReleaseErrors = rel
	}
	if resp.ReleaseErrors == nil {
		resp.ReleaseErrors = []store.ReleaseError{}
	}
	if total, perm, err := a.store.CountFailedReleases(ctx); err == nil {
		resp.Summary.FailedReleaseCount = total
		resp.Summary.PermanentReleaseCount = perm
	}

	// Recent failed jobs (filter the newest jobs to the failed state).
	if jobs, err := a.store.ListJobs(ctx, 200); err == nil {
		for _, j := range jobs {
			if j.State == store.JobFailed {
				resp.FailedJobs = append(resp.FailedJobs, j)
				if len(resp.FailedJobs) >= limit {
					break
				}
			}
		}
	}
	if resp.FailedJobs == nil {
		resp.FailedJobs = []store.Job{}
	}

	// Actionable hints.
	if retryable := resp.Summary.FailedReleaseCount - resp.Summary.PermanentReleaseCount; retryable > 0 {
		resp.Summary.RetryableReleaseHint = "Use \"Retry failed post-processing\" to requeue non-permanent failures."
	}
	if resp.Summary.GroupErrorCount > 0 {
		resp.Summary.GroupErrorHint = "Filter the Newsgroups panel by errors to inspect and re-trigger affected groups."
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCapacity returns current database sizes, observed ingest rate, per-group
// rankings, and growth/retention projections for capacity planning (#131).
func (a *API) handleCapacity(w http.ResponseWriter, r *http.Request) {
	stats, err := a.store.CapacityStats(r.Context(), parseIntDefault(r.URL.Query().Get("top"), 10))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to gather capacity stats")
		return
	}
	// Forecast from the observed basis. The retention window (from config, #118)
	// feeds the steady-state estimate; 0 means retention is disabled.
	forecast := store.ProjectCapacity(
		stats.DatabaseBytes, stats.ObservedArtsPerSecond, stats.BytesPerArticle,
		a.retentionDays, nil)
	writeJSON(w, http.StatusOK, capacityResponse{Stats: stats, Forecast: forecast})
}

// capacityResponse bundles the measured basis and the derived forecast (#131).
type capacityResponse struct {
	Stats    store.CapacityStats    `json:"stats"`
	Forecast store.CapacityForecast `json:"forecast"`
}

// groupWithHealth is a group plus its derived health and estimated storage for
// the admin listing (#127). The embedded Group is flattened into the JSON so
// existing fields (id, name, lag inputs, etc.) are unchanged for the SPA.
type groupWithHealth struct {
	store.Group
	Health       store.GroupHealth `json:"health"`
	StorageBytes int64             `json:"storage_bytes"`
}

// groupHealthPage mirrors store.GroupPage but carries the enriched group items.
type groupHealthPage struct {
	Groups []groupWithHealth `json:"groups"`
	Total  int               `json:"total"`
	Limit  int               `json:"limit"`
	Offset int               `json:"offset"`
}

type createGroupRequest struct {
	Name   string `json:"name"`
	Active *bool  `json:"active"`
}

func (a *API) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "group name is required")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	g, err := a.store.UpsertGroup(r.Context(), req.Name, active)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create group")
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

type bulkGroupsRequest struct {
	// Names is the list of newsgroup names to add/enable. Newline- or
	// comma-separated input from the UI is split client-side; here it is a
	// clean list. Blank and clearly-invalid tokens are skipped.
	Names []string `json:"names"`
	// Active sets whether the added groups are enabled (default true).
	Active *bool `json:"active"`
	// BackfillDays, when > 0, sets a per-group backfill window on each group so
	// a scan backfills that many days of history (e.g. 7 for one week).
	BackfillDays int `json:"backfill_days"`
}

type bulkGroupResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // added | existing | error
	Error  string `json:"error,omitempty"`
}

type bulkGroupsResponse struct {
	Added    int               `json:"added"`
	Existing int               `json:"existing"`
	Errors   int               `json:"errors"`
	Results  []bulkGroupResult `json:"results"`
}

// handleBulkGroups adds/enables many groups in one request, optionally applying
// a backfill window to each. It is idempotent: names that already exist are
// reported as "existing" (and still have the backfill window applied).
func (a *API) handleBulkGroups(w http.ResponseWriter, r *http.Request) {
	var req bulkGroupsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.BackfillDays < 0 {
		writeError(w, http.StatusBadRequest, "backfill_days must not be negative")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}

	// Normalise + de-duplicate the requested names.
	seen := map[string]bool{}
	var names []string
	for _, n := range req.Names {
		n = strings.TrimSpace(strings.ToLower(n))
		if !isValidGroupName(n) || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		writeError(w, http.StatusBadRequest, "no valid group names provided")
		return
	}

	var days *int
	if req.BackfillDays > 0 {
		d := req.BackfillDays
		days = &d
	}

	resp := bulkGroupsResponse{}
	for _, name := range names {
		res := bulkGroupResult{Name: name}

		// Determine new-vs-existing before the upsert so the report is accurate.
		existed := true
		if _, err := a.store.GetGroupByName(r.Context(), name); errors.Is(err, store.ErrNotFound) {
			existed = false
		}

		g, err := a.store.UpsertGroup(r.Context(), name, active)
		if err != nil {
			res.Status = "error"
			res.Error = "failed to add"
			resp.Errors++
			resp.Results = append(resp.Results, res)
			continue
		}
		if days != nil {
			if err := a.store.SetGroupBackfillTarget(r.Context(), g.ID, days, nil); err != nil {
				res.Status = "error"
				res.Error = "added but failed to set backfill"
				resp.Errors++
				resp.Results = append(resp.Results, res)
				continue
			}
		}
		if existed {
			res.Status = "existing"
			resp.Existing++
		} else {
			res.Status = "added"
			resp.Added++
		}
		resp.Results = append(resp.Results, res)
	}

	writeJSON(w, http.StatusOK, resp)
}

// isValidGroupName does a light sanity check on a newsgroup name: non-empty,
// dotted hierarchy, no whitespace, reasonable characters. This screens out
// stray tokens from pasted input without being overly strict.
func isValidGroupName(n string) bool {
	if n == "" || len(n) > 512 || !strings.Contains(n, ".") {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_' || r == '+':
		default:
			return false
		}
	}
	return true
}

type updateGroupRequest struct {
	Active *bool `json:"active"`
}

func (a *API) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	var req updateGroupRequest
	if err := decodeJSON(r, &req); err != nil || req.Active == nil {
		writeError(w, http.StatusBadRequest, "active field is required")
		return
	}
	if err := a.store.SetGroupActive(r.Context(), id, *req.Active); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type backfillTargetRequest struct {
	// Days and Articles are the per-group backfill targets. A field set to null
	// (omitted) clears that dimension's override; 0 means "no bound / unlimited".
	Days     *int   `json:"days"`
	Articles *int64 `json:"articles"`
}

func (a *API) handleSetGroupBackfill(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	var req backfillTargetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if (req.Days != nil && *req.Days < 0) || (req.Articles != nil && *req.Articles < 0) {
		writeError(w, http.StatusBadRequest, "backfill targets must not be negative")
		return
	}
	if err := a.store.SetGroupBackfillTarget(r.Context(), id, req.Days, req.Articles); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to set backfill target")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scanConfigRequest sets a group's scan priority and forward budget (#126).
type scanConfigRequest struct {
	// Priority orders the scan set (higher scanned first). Defaults to 0.
	Priority int `json:"priority"`
	// ForwardArticles is the per-group forward per-pass article cap: null clears
	// the override (use global default); 0 means unbounded; positive is an
	// explicit cap.
	ForwardArticles *int64 `json:"forward_articles"`
}

func (a *API) handleSetGroupScanConfig(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	var req scanConfigRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ForwardArticles != nil && *req.ForwardArticles < 0 {
		writeError(w, http.StatusBadRequest, "forward article budget must not be negative")
		return
	}
	if err := a.store.SetGroupScanConfig(r.Context(), id, req.Priority, req.ForwardArticles); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to set scan config")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	if err := a.store.DeleteGroup(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- newsgroup discovery ---

type discoverResponse struct {
	Total    int               `json:"total"`
	Limit    int               `json:"limit"`
	Offset   int               `json:"offset"`
	CachedAt string            `json:"cached_at,omitempty"`
	Groups   []DiscoveredGroup `json:"groups"`
}

func (a *API) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if a.discoverer == nil {
		writeError(w, http.StatusServiceUnavailable, "discovery not available")
		return
	}
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 50)
	if limit > 200 {
		limit = 200
	}
	offset := parseIntDefault(q.Get("offset"), 0)
	refresh := q.Get("refresh") == "1" || strings.EqualFold(q.Get("refresh"), "true")

	groups, total, cachedAt, err := a.discoverer.SearchGroups(r.Context(), q.Get("q"), limit, offset, refresh)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch group list from provider: "+err.Error())
		return
	}
	if groups == nil {
		groups = []DiscoveredGroup{}
	}
	resp := discoverResponse{Total: total, Limit: limit, Offset: offset, Groups: groups}
	if !cachedAt.IsZero() {
		resp.CachedAt = cachedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- news servers ---

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.store.ListServers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list servers")
		return
	}
	if servers == nil {
		servers = []store.Server{}
	}
	// Passwords are never serialised (Server.Password has json:"-"), but signal
	// whether one is set so the UI can show a placeholder.
	type serverView struct {
		store.Server
		HasPassword bool `json:"has_password"`
	}
	views := make([]serverView, 0, len(servers))
	for _, s := range servers {
		views = append(views, serverView{Server: s, HasPassword: s.Password != ""})
	}
	writeJSON(w, http.StatusOK, views)
}

type serverRequest struct {
	Name     string  `json:"name"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	TLS      bool    `json:"tls"`
	Username string  `json:"username"`
	Password *string `json:"password"` // nil on update = leave unchanged
	MaxConns int     `json:"max_conns"`
	Priority int     `json:"priority"`
	Enabled  bool    `json:"enabled"`
}

func (req serverRequest) toInput() store.ServerInput {
	return store.ServerInput{
		Name:     req.Name,
		Host:     req.Host,
		Port:     req.Port,
		TLS:      req.TLS,
		Username: req.Username,
		Password: req.Password,
		MaxConns: req.MaxConns,
		Priority: req.Priority,
		Enabled:  req.Enabled,
	}
}

func (a *API) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var req serverRequest
	if err := decodeJSON(r, &req); err != nil || req.Host == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name and host are required")
		return
	}
	srv, err := a.store.CreateServer(r.Context(), req.toInput())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create server")
		return
	}
	a.applyActiveServer(r)
	writeJSON(w, http.StatusCreated, srv)
}

func (a *API) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid server id")
		return
	}
	var req serverRequest
	if err := decodeJSON(r, &req); err != nil || req.Host == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name and host are required")
		return
	}
	srv, err := a.store.UpdateServer(r.Context(), id, req.toInput())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update server")
		return
	}
	a.applyActiveServer(r)
	writeJSON(w, http.StatusOK, srv)
}

func (a *API) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid server id")
		return
	}
	if err := a.store.DeleteServer(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete server")
		return
	}
	a.applyActiveServer(r)
	w.WriteHeader(http.StatusNoContent)
}

// applyActiveServer reconfigures the live NNTP pool to the current active
// server, if a manager is wired. This applies connection parameters AND safely
// resizes the pool's connection ceiling (#111), so a max-connections change
// takes effect live. Errors are logged, not surfaced, since the change is
// already persisted and would otherwise take effect on the next restart.
func (a *API) applyActiveServer(r *http.Request) {
	if a.servers == nil {
		return
	}
	if err := a.servers.ApplyActive(r.Context()); err != nil {
		a.log.Warn("failed to apply active news server to pool", "err", err)
	}
}

// --- users ---

func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	if users == nil {
		users = []store.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
}

func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil || req.Username == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	role := store.RoleUser
	if req.Admin {
		role = store.RoleAdmin
	}
	u, err := a.store.CreateUser(r.Context(), store.CreateUserInput{
		Username: req.Username, PasswordHash: hash, Role: role,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (a *API) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	// Prevent an admin from deleting themselves.
	if p, ok := auth.PrincipalFrom(r.Context()); ok && p.UserID == id {
		writeError(w, http.StatusBadRequest, "cannot delete your own account")
		return
	}
	if err := a.store.DeleteUser(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	// The deleted user's keys must stop authenticating immediately.
	a.session.InvalidateAPIKeyCache()
	w.WriteHeader(http.StatusNoContent)
}

// --- jobs / status ---

type triggerRequest struct {
	Group string `json:"group"`
}

func (a *API) handleTriggerScan(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	var req triggerRequest
	_ = decodeJSON(r, &req)
	jobID, err := a.jobs.TriggerScan(req.Group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "job_id": jobID})
}

func (a *API) handleTriggerBackfill(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	var req triggerRequest
	_ = decodeJSON(r, &req)
	jobID, err := a.jobs.TriggerBackfill(req.Group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "job_id": jobID})
}

func (a *API) handleTriggerPostProcess(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	jobID, err := a.jobs.TriggerPostProcess()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "job_id": jobID})
}

// handleListJobs returns recent pipeline jobs (newest first).
func (a *API) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
	jobs, err := a.store.ListJobs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleGetJob returns a single job by id.
func (a *API) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleCancelJob requests cooperative cancellation of a job.
func (a *API) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	if err := a.jobs.CancelJob(r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "job not found or not cancellable")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "cancellation requested"})
}

func (a *API) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.RequeueFailedReleases(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Kick a post-processing pass so the requeued releases are picked up
	// promptly (best-effort; the scheduled loop would pick them up anyway).
	if a.jobs != nil {
		_, _ = a.jobs.TriggerPostProcess()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "failed releases requeued", "requeued": n})
}

// handleBackfillSegments snapshots durable NZB segments for legacy releases
// that lack them, making them retention-safe. Bounded per call; run repeatedly
// to process a large backlog.
func (a *API) handleBackfillSegments(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	repaired, unresolved, err := a.store.BackfillReleaseSegments(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "segment backfill failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repaired":   repaired,
		"unresolved": unresolved,
	})
}

// --- schedule ---

// scheduleKeys maps the JSON/setting field to its settings-table key.
const (
	settingScanInterval    = "schedule.scan_interval"
	settingDownstream      = "schedule.downstream_interval"
	settingBuildInterval   = "schedule.build_interval"
	settingPostProcInterval = "schedule.postprocess_interval"
)

// scheduleResponse reports intervals both as human duration strings (e.g.
// "5m0s") and as seconds, so the UI can render either.
type scheduleResponse struct {
	ScanInterval           string `json:"scan_interval"`
	DownstreamInterval     string `json:"downstream_interval"`
	BuildInterval          string `json:"build_interval"`
	PostProcessInterval    string `json:"postprocess_interval"`
	ScanIntervalSec        int64  `json:"scan_interval_sec"`
	DownstreamIntervalSec  int64  `json:"downstream_interval_sec"`
	BuildIntervalSec       int64  `json:"build_interval_sec"`
	PostProcessIntervalSec int64  `json:"postprocess_interval_sec"`
}

func (a *API) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	writeJSON(w, http.StatusOK, buildScheduleResponse(a.jobs.CurrentSchedule()))
}

// buildScheduleResponse renders a Schedule as the API response shape.
func buildScheduleResponse(s Schedule) scheduleResponse {
	return scheduleResponse{
		ScanInterval:           s.ScanInterval.String(),
		DownstreamInterval:     s.DownstreamInterval.String(),
		BuildInterval:          s.BuildInterval.String(),
		PostProcessInterval:    s.PostProcessInterval.String(),
		ScanIntervalSec:        int64(s.ScanInterval.Seconds()),
		DownstreamIntervalSec:  int64(s.DownstreamInterval.Seconds()),
		BuildIntervalSec:       int64(s.BuildInterval.Seconds()),
		PostProcessIntervalSec: int64(s.PostProcessInterval.Seconds()),
	}
}

// updateScheduleRequest accepts each interval as a Go duration string (e.g.
// "5m", "30s", "1h"). Omitted/empty fields are left unchanged.
type updateScheduleRequest struct {
	ScanInterval        string `json:"scan_interval"`
	DownstreamInterval  string `json:"downstream_interval"`
	BuildInterval       string `json:"build_interval"`
	PostProcessInterval string `json:"postprocess_interval"`
}

func (a *API) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	var req updateScheduleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Parse each provided field; a field must be a positive duration.
	var sched Schedule
	persist := map[string]string{}
	parse := func(raw, settingKey string, dst *time.Duration) bool {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return true // unchanged
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, "invalid duration for "+settingKey+" (use e.g. 30s, 5m, 1h)")
			return false
		}
		*dst = d
		persist[settingKey] = d.String()
		return true
	}
	if !parse(req.ScanInterval, settingScanInterval, &sched.ScanInterval) {
		return
	}
	if !parse(req.DownstreamInterval, settingDownstream, &sched.DownstreamInterval) {
		return
	}
	if !parse(req.BuildInterval, settingBuildInterval, &sched.BuildInterval) {
		return
	}
	if !parse(req.PostProcessInterval, settingPostProcInterval, &sched.PostProcessInterval) {
		return
	}

	if len(persist) == 0 {
		writeError(w, http.StatusBadRequest, "no schedule fields provided")
		return
	}

	// Persist first so the change survives a restart, then apply live.
	if err := a.store.SetSettings(r.Context(), persist); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist schedule")
		return
	}
	a.jobs.Reconfigure(sched)

	// Return the new effective schedule.
	a.handleGetSchedule(w, r)
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, a.jobs.Status())
}

func (a *API) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.store.PipelineStatistics(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	if a.logs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 200)
	writeJSON(w, http.StatusOK, a.recentLogs(limit, parseLogLevel(q.Get("level"))))
}

// parseLogLevel maps a level string to a minimum slog.Level, or nil for "all".
func parseLogLevel(s string) *slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		l := slog.LevelDebug
		return &l
	case "info":
		l := slog.LevelInfo
		return &l
	case "warn", "warning":
		l := slog.LevelWarn
		return &l
	case "error":
		l := slog.LevelError
		return &l
	}
	return nil
}

// recentLogs returns bounded recent log entries (never nil), reused by the logs
// endpoint and the admin overview.
func (a *API) recentLogs(limit int, minLevel *slog.Level) []logbuf.Entry {
	if a.logs == nil {
		return []logbuf.Entry{}
	}
	if limit > 1000 {
		limit = 1000
	}
	entries := a.logs.Recent(limit, minLevel)
	if entries == nil {
		entries = []logbuf.Entry{}
	}
	return entries
}
