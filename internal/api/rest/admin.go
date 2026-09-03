package rest

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/vindicare/goindex/internal/auth"
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
