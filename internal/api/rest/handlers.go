package rest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/store"
)

// handleHealth is the liveness probe: it reports only that the process is up
// and serving, without touching any dependency. Always 200.
func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady is the readiness probe: it verifies the database is reachable so
// an orchestrator does not route traffic to an instance that cannot serve it.
// Returns 200 when the DB responds, 503 otherwise.
func (a *API) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"error":  "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	token, p, err := a.authn.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token, Username: p.Username, Role: p.Role})
}

// handleSetupStatus reports whether first-run setup is required (no users yet).
func (a *API) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.CountUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check setup status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"setup_required": n == 0})
}

// handleSetup creates the first admin account. It is only permitted when no
// users exist, preventing any privilege-escalation once the app is set up.
func (a *API) handleSetup(w http.ResponseWriter, r *http.Request) {
	n, err := a.store.CountUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check setup status")
		return
	}
	if n > 0 {
		// Setup already completed; refuse to create another account here.
		writeError(w, http.StatusConflict, "setup has already been completed")
		return
	}

	var req loginRequest // reuse {username, password}
	if err := decodeJSON(r, &req); err != nil || req.Username == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := a.store.CreateUser(r.Context(), store.CreateUserInput{
		Username: req.Username, PasswordHash: hash, Role: store.RoleAdmin,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create admin user")
		return
	}

	// Issue a session token so the UI can proceed straight into the app.
	token, p, err := a.authn.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		// The account exists even if token issuance failed; the user can log in.
		writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username, "role": store.RoleAdmin})
		return
	}
	writeJSON(w, http.StatusCreated, loginResponse{Token: token, Username: p.Username, Role: p.Role})
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  p.UserID,
		"username": p.Username,
		"role":     p.Role,
		"is_admin": p.IsAdmin(),
	})
}

func (a *API) handleCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := a.store.ListCategories(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load categories")
		return
	}
	writeJSON(w, http.StatusOK, cats)
}

type searchResponse struct {
	Total    int             `json:"total"`
	Limit    int             `json:"limit"`
	Offset   int             `json:"offset"`
	Releases []store.Release `json:"releases"`
	// Approximate is true when Total was capped (the real total is >= Total).
	Approximate bool `json:"approximate"`
	// NextCursor is an opaque token for the next page via keyset pagination
	// (#120); empty when there is no further page. Pass it as ?cursor= to fetch
	// the next page without an OFFSET scan.
	NextCursor string `json:"next_cursor,omitempty"`
	// HasMore indicates another page likely exists.
	HasMore bool `json:"has_more"`
}

func (a *API) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 50)
	if limit > 200 {
		limit = 200
	}
	offset := parseIntDefault(q.Get("offset"), 0)

	// cat accepts a comma-separated list (e.g. "5030,5040"), matching the
	// newznab handler, so clients like Sonarr/Radarr that request several
	// categories are honoured. Invalid entries are ignored.
	var cats []int
	for _, part := range strings.Split(q.Get("cat"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			cats = append(cats, n)
		}
	}

	filter := store.SearchFilter{
		Query:      q.Get("q"),
		Categories: cats,
		Limit:      limit,
		Offset:     offset,
		// Obfuscated (unusable) releases are hidden unless explicitly requested.
		IncludeObfuscated: q.Get("include_obfuscated") == "1" || q.Get("include_obfuscated") == "true",
	}
	// Keyset pagination (#120): a ?cursor= token pages without an OFFSET scan.
	// When present it takes precedence over offset.
	if c := q.Get("cursor"); c != "" {
		cur, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		filter.Cursor = cur
		filter.Offset = 0
	}

	page, err := a.store.SearchReleasesPage(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	releases := page.Releases
	if releases == nil {
		releases = []store.Release{}
	}
	resp := searchResponse{
		Total: page.Total, Limit: limit, Offset: offset, Releases: releases,
		Approximate: page.Approximate, HasMore: page.HasMore,
	}
	if page.HasMore && page.NextCursor != nil {
		resp.NextCursor = encodeCursor(*page.NextCursor)
	}
	writeJSON(w, http.StatusOK, resp)
}

// encodeCursor renders a keyset position as an opaque base64 token
// "<sortUnixNano>:<id>".
func encodeCursor(c store.SearchCursor) string {
	raw := fmt.Sprintf("%d:%d", c.Sort.UTC().UnixNano(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a token produced by encodeCursor.
func decodeCursor(tok string) (*store.SearchCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, err
	}
	var nanos, id int64
	if _, err := fmt.Sscanf(string(b), "%d:%d", &nanos, &id); err != nil {
		return nil, err
	}
	return &store.SearchCursor{Sort: time.Unix(0, nanos).UTC(), ID: id}, nil
}

func (a *API) handleReleaseDetail(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	rel, err := a.store.GetReleaseByGUID(r.Context(), guid)
	if err != nil {
		writeError(w, http.StatusNotFound, "release not found")
		return
	}
	files, err := a.store.GetReleaseFiles(r.Context(), rel.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load release files")
		return
	}
	if files == nil {
		files = []store.ReleaseFile{}
	}
	resp := map[string]any{
		"release": rel,
		"files":   files,
	}
	// Include external metadata when the release has been enriched (best-effort:
	// absence or error simply omits it).
	if md, err := a.store.GetReleaseMetadata(r.Context(), rel.ID); err == nil && md.Matched {
		resp["metadata"] = md
	}
	// Include normalized external identifiers (imdb/tvdb/tmdb) when present.
	ids, err := a.store.GetReleaseIdentifiers(r.Context(), rel.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load release identifiers")
		return
	}
	if ids == nil {
		ids = []store.ReleaseIdentifier{}
	}
	resp["identifiers"] = ids
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleDownload(w http.ResponseWriter, r *http.Request) {
	guid := r.PathValue("guid")
	data, filename, err := a.nzb.ForGUID(r.Context(), guid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "release not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to generate NZB")
		return
	}
	w.Header().Set("Content-Type", "application/x-nzb")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// --- self-service API keys ---

func (a *API) handleListMyKeys(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	keys, err := a.store.ListAPIKeys(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list keys")
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

type createKeyRequest struct {
	Label string `json:"label"`
}

func (a *API) handleCreateMyKey(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var req createKeyRequest
	_ = decodeJSON(r, &req) // label optional; ignore decode errors on empty body

	key, err := auth.GenerateAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate key")
		return
	}
	label := req.Label
	if label == "" {
		label = "default"
	}
	created, err := a.store.CreateAPIKey(r.Context(), p.UserID, key, label)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create key")
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (a *API) handleDeleteMyKey(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid key id")
		return
	}
	if err := a.store.DeleteAPIKey(r.Context(), p.UserID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete key")
		return
	}
	// Drop cached auth so the deleted key stops authenticating immediately.
	a.session.InvalidateAPIKeyCache()
	w.WriteHeader(http.StatusNoContent)
}

// parseIntDefault parses s or returns def.
func parseIntDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
