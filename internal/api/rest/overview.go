package rest

import (
	"net/http"
	"time"

	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/store"
)

// overviewResponse is the aggregated admin dashboard envelope. Each section is
// populated independently; a subsystem that fails is recorded in Errors and its
// field is left null/empty rather than failing the whole response. This lets
// the admin page load everything it needs in one request while still surfacing
// partial failures.
type overviewResponse struct {
	// GeneratedAt is the server time the envelope was assembled (RFC3339), so
	// the client can display freshness and discard stale overlapping responses.
	GeneratedAt time.Time `json:"generated_at"`

	Health   *healthResponse         `json:"health"`
	Status   any                     `json:"status"`
	Stats    *store.PipelineStats    `json:"stats"`
	Groups   []store.Group           `json:"groups"`
	// GroupsTotal is the full group count; Groups holds only a bounded first
	// page for the dashboard (#123).
	GroupsTotal int `json:"groups_total"`
	Servers  []serverOverview        `json:"servers"`
	Users    []store.User            `json:"users"`
	Schedule *scheduleResponse       `json:"schedule"`
	Logs     []logbuf.Entry          `json:"logs"`

	// Errors maps a section name to a message when that section could not be
	// loaded. Absent sections loaded successfully.
	Errors map[string]string `json:"errors,omitempty"`
}

// serverOverview mirrors the server list view: credentials are never included,
// only whether a password is set.
type serverOverview struct {
	store.Server
	HasPassword bool `json:"has_password"`
}

// handleAdminOverview assembles the admin dashboard in a single request. It is
// admin-authenticated (wired with the admin middleware). Each subsystem is
// gathered best-effort so one failure does not blank the rest of the page.
func (a *API) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := overviewResponse{
		GeneratedAt: time.Now().UTC(),
		Groups:      []store.Group{},
		Servers:     []serverOverview{},
		Users:       []store.User{},
		Logs:        []logbuf.Entry{},
		Errors:      map[string]string{},
	}

	// Health is itself an aggregate with its own internal best-effort checks.
	health := a.buildHealthReport(ctx)
	resp.Health = &health

	// Worker status (jobs controller may be absent, e.g. in tests).
	if a.jobs != nil {
		resp.Status = a.jobs.Status()
		sched := buildScheduleResponse(a.jobs.CurrentSchedule())
		resp.Schedule = &sched
	} else {
		resp.Status = map[string]any{"jobs": "unavailable"}
	}

	// Pipeline statistics.
	if stats, err := a.store.PipelineStatistics(ctx); err != nil {
		resp.Errors["stats"] = err.Error()
	} else {
		resp.Stats = &stats
	}

	// Groups.
	// Groups: only a bounded first page here so the overview stays light even
	// with thousands of groups (#123). The admin UI loads/pages the full list
	// via GET /admin/groups. GroupsTotal reports the full count.
	if page, err := a.store.ListGroupsPage(ctx, store.GroupFilter{Limit: 50}); err != nil {
		resp.Errors["groups"] = err.Error()
	} else {
		resp.Groups = page.Groups
		resp.GroupsTotal = page.Total
	}

	// Servers (credential-redacted).
	if servers, err := a.store.ListServers(ctx); err != nil {
		resp.Errors["servers"] = err.Error()
	} else {
		views := make([]serverOverview, 0, len(servers))
		for _, s := range servers {
			views = append(views, serverOverview{Server: s, HasPassword: s.Password != ""})
		}
		resp.Servers = views
	}

	// Users (password hashes are never serialised: User.PasswordHash is json:"-").
	if users, err := a.store.ListUsers(ctx); err != nil {
		resp.Errors["users"] = err.Error()
	} else if users != nil {
		resp.Users = users
	}

	// Recent logs, bounded.
	resp.Logs = a.recentLogs(200, nil)

	if len(resp.Errors) == 0 {
		resp.Errors = nil
	}
	writeJSON(w, http.StatusOK, resp)
}
