package rest

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/store"
)

// --- groups ---

func (a *API) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.store.ListGroups(r.Context(), false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}
	if groups == nil {
		groups = []store.Group{}
	}
	writeJSON(w, http.StatusOK, groups)
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
// server, if a manager is wired. Errors are logged, not surfaced, since the
// change is already persisted and will take effect on restart regardless.
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
	if err := a.jobs.TriggerScan(req.Group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "scan triggered"})
}

func (a *API) handleTriggerBackfill(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "job controller not available")
		return
	}
	var req triggerRequest
	_ = decodeJSON(r, &req)
	if err := a.jobs.TriggerBackfill(req.Group); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "backfill triggered"})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	if a.jobs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"jobs": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, a.jobs.Status())
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	if a.logs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 200)
	if limit > 1000 {
		limit = 1000
	}

	var minLevel *slog.Level
	switch strings.ToLower(q.Get("level")) {
	case "debug":
		l := slog.LevelDebug
		minLevel = &l
	case "info":
		l := slog.LevelInfo
		minLevel = &l
	case "warn", "warning":
		l := slog.LevelWarn
		minLevel = &l
	case "error":
		l := slog.LevelError
		minLevel = &l
	}

	entries := a.logs.Recent(limit, minLevel)
	if entries == nil {
		entries = []logbuf.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
