package auth

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// ErrUnauthorized indicates missing or invalid credentials.
var ErrUnauthorized = errors.New("auth: unauthorized")

// ErrRateLimited indicates the caller exceeded their request budget.
var ErrRateLimited = errors.New("auth: rate limited")

// RateLimitedError is returned when a caller exceeds their budget; it carries
// the window reset time so callers can emit a Retry-After header. It matches
// ErrRateLimited under errors.Is.
type RateLimitedError struct {
	ResetAt time.Time
}

func (e *RateLimitedError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitedError) Is(target error) bool {
	return target == ErrRateLimited
}

// Repo is the subset of the store the auth service needs.
type Repo interface {
	GetUserByUsername(ctx context.Context, username string) (store.User, error)
	GetAPIKeyWithUser(ctx context.Context, apiKey string) (store.APIKeyUser, error)
	TouchAPIKey(ctx context.Context, id int64) error
}

// Principal is the authenticated identity attached to a request.
type Principal struct {
	UserID   int64
	Username string
	Role     string
	// APIKeyID is set when authentication was via API key (0 for sessions).
	APIKeyID int64
	// Rate-limit snapshot for the current request, set on API-key auth so the
	// caller can emit X-RateLimit-* headers. Limit <= 0 means unlimited.
	RateLimit          int
	RateLimitRemaining int
	RateLimitReset     time.Time
}

// IsAdmin reports whether the principal has the admin role.
func (p Principal) IsAdmin() bool { return p.Role == store.RoleAdmin }

// Service authenticates users and API keys and enforces rate limits.
type Service struct {
	repo             Repo
	tokens           *TokenIssuer
	limiter          *RateLimiter
	defaultRateLimit int
}

// NewService constructs an auth Service.
func NewService(repo Repo, tokens *TokenIssuer, limiter *RateLimiter, defaultRateLimit int) *Service {
	return &Service{
		repo:             repo,
		tokens:           tokens,
		limiter:          limiter,
		defaultRateLimit: defaultRateLimit,
	}
}

// Login verifies username/password credentials and issues a session token.
func (s *Service) Login(ctx context.Context, username, password string) (token string, p Principal, err error) {
	u, err := s.repo.GetUserByUsername(ctx, username)
	if err != nil {
		return "", Principal{}, ErrUnauthorized
	}
	if !u.Active || !CheckPassword(u.PasswordHash, password) {
		return "", Principal{}, ErrUnauthorized
	}
	tok, err := s.tokens.Issue(u.ID, u.Username, u.Role)
	if err != nil {
		return "", Principal{}, err
	}
	return tok, Principal{UserID: u.ID, Username: u.Username, Role: u.Role}, nil
}

// AuthenticateSession validates a session token and returns its principal.
func (s *Service) AuthenticateSession(token string) (Principal, error) {
	claims, err := s.tokens.Verify(token)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		UserID:   claims.UserID,
		Username: claims.Username,
		Role:     claims.Role,
	}, nil
}

// AuthenticateAPIKey validates a Newznab API key, applies the owner's rate
// limit, and returns the principal. It returns ErrRateLimited when the key's
// budget is exhausted.
func (s *Service) AuthenticateAPIKey(ctx context.Context, apiKey string) (Principal, error) {
	if apiKey == "" {
		return Principal{}, ErrUnauthorized
	}
	rec, err := s.repo.GetAPIKeyWithUser(ctx, apiKey)
	if err != nil {
		return Principal{}, ErrUnauthorized
	}

	limit := rec.User.RateLimit
	if limit <= 0 {
		limit = s.defaultRateLimit
	}
	var (
		remaining = -1 // -1 => unlimited/unknown
		resetAt   time.Time
	)
	if s.limiter != nil {
		ok, rem, reset := s.limiter.Allow(strconv.FormatInt(rec.Key.ID, 10), limit)
		remaining, resetAt = rem, reset
		if !ok {
			return Principal{}, &RateLimitedError{ResetAt: reset}
		}
	}

	// Best-effort last-used update; ignore errors.
	_ = s.repo.TouchAPIKey(ctx, rec.Key.ID)

	return Principal{
		UserID:             rec.User.ID,
		Username:           rec.User.Username,
		Role:               rec.User.Role,
		APIKeyID:           rec.Key.ID,
		RateLimit:          limit,
		RateLimitRemaining: remaining,
		RateLimitReset:     resetAt,
	}, nil
}

// RateLimitFor returns the effective rate limit for a user.
func (s *Service) RateLimitFor(u store.User) int {
	if u.RateLimit > 0 {
		return u.RateLimit
	}
	return s.defaultRateLimit
}

// CleanupLoop periodically prunes expired rate-limit windows until ctx is done.
func (s *Service) CleanupLoop(ctx context.Context, every time.Duration) {
	if s.limiter == nil {
		return
	}
	if every <= 0 {
		every = 10 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.limiter.Cleanup()
		}
	}
}
