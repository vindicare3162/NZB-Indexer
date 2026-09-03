package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ctxKey is the private type for context values set by this package.
type ctxKey int

const principalKey ctxKey = iota

// WithPrincipal returns a copy of ctx carrying the given principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom extracts the authenticated principal from ctx, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// RequireSession is middleware that authenticates a SPA request via a Bearer
// token (Authorization header) or a "token" cookie, rejecting unauthenticated
// requests with 401.
func (s *Service) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie("token"); err == nil {
				token = c.Value
			}
		}
		if token == "" {
			writeAuthError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		p, err := s.AuthenticateSession(token)
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireAdmin wraps RequireSession and additionally requires the admin role.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return s.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := PrincipalFrom(r.Context()); !ok || !p.IsAdmin() {
			writeAuthError(w, http.StatusForbidden, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// RequireAPIKey is middleware for the Newznab API. It reads the API key from
// the "apikey" query parameter (Newznab convention), authenticates it, applies
// rate limiting, and attaches the principal. On rate-limit it returns 429.
func (s *Service) RequireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.URL.Query().Get("apikey")
		p, err := s.AuthenticateAPIKey(r.Context(), apiKey)
		switch {
		case errors.Is(err, ErrRateLimited):
			writeAuthError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		case err != nil:
			// Newznab clients expect a 401 for a bad key.
			writeAuthError(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if strings.HasPrefix(h, prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// writeAuthError writes a small JSON error body with the given status.
func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// Minimal hand-rolled JSON to avoid a dependency here.
	_, _ = w.Write([]byte(`{"error":` + quoteJSON(msg) + `}`))
}

// quoteJSON returns a JSON-quoted string.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
