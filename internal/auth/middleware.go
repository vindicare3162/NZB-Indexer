package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ctxKey is the private type for context values set by this package.
type ctxKey int

const (
	principalKey ctxKey = iota
	holderKey
)

// principalHolder is a mutable cell an outer middleware (e.g. an access logger)
// can install so it can observe the principal that downstream auth middleware
// attaches on a child context. Because WithPrincipal derives a new context, the
// value it sets is not visible to a parent handler; the holder bridges that gap
// without exposing auth internals.
type principalHolder struct {
	p     Principal
	isSet bool
}

// WithPrincipalHolder returns a copy of ctx carrying an empty holder plus the
// holder itself. An outer middleware installs this before calling the handler,
// then reads the holder after the handler returns to learn who was authenticated.
func WithPrincipalHolder(ctx context.Context) (context.Context, *principalHolder) {
	h := &principalHolder{}
	return context.WithValue(ctx, holderKey, h), h
}

// Principal returns the captured principal and whether one was set.
func (h *principalHolder) Principal() (Principal, bool) {
	if h == nil {
		return Principal{}, false
	}
	return h.p, h.isSet
}

// WithPrincipal returns a copy of ctx carrying the given principal. When an
// upstream holder is present it is also populated, so an outer middleware can
// observe the authenticated identity.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	if h, ok := ctx.Value(holderKey).(*principalHolder); ok {
		h.p = p
		h.isSet = true
	}
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
			// Tell the client when to retry so it backs off instead of hammering.
			var rle *RateLimitedError
			if errors.As(err, &rle) && !rle.ResetAt.IsZero() {
				secs := int(time.Until(rle.ResetAt).Seconds())
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(rle.ResetAt.Unix(), 10))
			}
			writeAuthError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		case err != nil:
			// Newznab clients expect a 401 for a bad key.
			writeAuthError(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		// Standard rate-limit headers on successful authenticated responses.
		if p.RateLimit > 0 {
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(p.RateLimit))
			if p.RateLimitRemaining >= 0 {
				w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(p.RateLimitRemaining))
			}
			if !p.RateLimitReset.IsZero() {
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(p.RateLimitReset.Unix(), 10))
			}
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
