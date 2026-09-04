package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

func TestPasswordHashAndCheck(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Error("expected error for short password")
	}
	h, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct-horse") {
		t.Error("valid password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Error("invalid password accepted")
	}
}

func TestGenerateAPIKeyUnique(t *testing.T) {
	k1, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := GenerateAPIKey()
	if len(k1) != 32 {
		t.Errorf("api key length = %d, want 32", len(k1))
	}
	if k1 == k2 {
		t.Error("api keys should be unique")
	}
}

func TestTokenIssueVerify(t *testing.T) {
	ti, err := NewTokenIssuer("test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ti.Issue(42, "alice", store.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ti.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.UserID != 42 || claims.Username != "alice" || claims.Role != store.RoleAdmin {
		t.Errorf("claims = %+v", claims)
	}

	// A token from a different secret must fail.
	other, _ := NewTokenIssuer("other-secret", time.Hour)
	if _, err := other.Verify(tok); err == nil {
		t.Error("token verified under wrong secret")
	}
}

func TestTokenExpiry(t *testing.T) {
	ti, _ := NewTokenIssuer("s", -time.Hour) // ttl clamped to 24h, so force manually
	// Issue with a negative TTL isn't possible via Issue (clamped), so test
	// verification of an already-expired token by issuing with a tiny TTL.
	ti2 := &TokenIssuer{secret: []byte("s"), ttl: time.Millisecond}
	tok, _ := ti2.Issue(1, "u", "user")
	time.Sleep(5 * time.Millisecond)
	if _, err := ti.Verify(tok); err == nil {
		t.Error("expected expired token to fail verification")
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(time.Minute)
	base := time.Now()
	rl.now = func() time.Time { return base }

	// Limit of 3: first three allowed, fourth denied.
	for i := 0; i < 3; i++ {
		if ok, _, _ := rl.Allow("k", 3); !ok {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	if ok, rem, _ := rl.Allow("k", 3); ok || rem != 0 {
		t.Errorf("4th request: allowed=%v remaining=%d, want denied/0", ok, rem)
	}

	// Advancing past the window resets the counter.
	rl.now = func() time.Time { return base.Add(2 * time.Minute) }
	if ok, _, _ := rl.Allow("k", 3); !ok {
		t.Error("request after window reset should be allowed")
	}

	// A limit of 0 means unlimited.
	for i := 0; i < 100; i++ {
		if ok, _, _ := rl.Allow("unlimited", 0); !ok {
			t.Fatal("limit 0 should be unlimited")
		}
	}
}

// --- service tests with a fake repo ---

type fakeRepo struct {
	users   map[string]store.User
	keys    map[string]store.APIKeyUser
	touched int
	lookups int
}

func (f *fakeRepo) GetUserByUsername(_ context.Context, u string) (store.User, error) {
	if user, ok := f.users[u]; ok {
		return user, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeRepo) GetAPIKeyWithUser(_ context.Context, k string) (store.APIKeyUser, error) {
	f.lookups++
	if rec, ok := f.keys[k]; ok {
		return rec, nil
	}
	return store.APIKeyUser{}, store.ErrNotFound
}

func (f *fakeRepo) TouchAPIKey(_ context.Context, _ int64) error {
	f.touched++
	return nil
}

func newTestService(t *testing.T, repo Repo, rl *RateLimiter, defLimit int) *Service {
	t.Helper()
	ti, err := NewTokenIssuer("secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(repo, ti, rl, defLimit)
}

func TestServiceLogin(t *testing.T) {
	hash, _ := HashPassword("password123")
	repo := &fakeRepo{users: map[string]store.User{
		"alice": {ID: 1, Username: "alice", PasswordHash: hash, Role: store.RoleAdmin, Active: true},
		"bob":   {ID: 2, Username: "bob", PasswordHash: hash, Role: store.RoleUser, Active: false},
	}}
	svc := newTestService(t, repo, nil, 100)
	ctx := context.Background()

	tok, p, err := svc.Login(ctx, "alice", "password123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if p.UserID != 1 || !p.IsAdmin() {
		t.Errorf("principal = %+v", p)
	}
	if _, err := svc.AuthenticateSession(tok); err != nil {
		t.Errorf("session from login token failed: %v", err)
	}

	if _, _, err := svc.Login(ctx, "alice", "wrong"); err != ErrUnauthorized {
		t.Error("wrong password should be unauthorized")
	}
	if _, _, err := svc.Login(ctx, "bob", "password123"); err != ErrUnauthorized {
		t.Error("inactive user should be unauthorized")
	}
	if _, _, err := svc.Login(ctx, "nobody", "x"); err != ErrUnauthorized {
		t.Error("unknown user should be unauthorized")
	}
}

func TestServiceAuthenticateAPIKeyAndRateLimit(t *testing.T) {
	repo := &fakeRepo{keys: map[string]store.APIKeyUser{
		"goodkey": {
			Key:  store.APIKey{ID: 7, Active: true},
			User: store.User{ID: 3, Username: "carol", Role: store.RoleUser, RateLimit: 2, Active: true},
		},
	}}
	rl := NewRateLimiter(time.Minute)
	svc := newTestService(t, repo, rl, 100)
	ctx := context.Background()

	// First two allowed (user rate_limit = 2).
	for i := 0; i < 2; i++ {
		if _, err := svc.AuthenticateAPIKey(ctx, "goodkey"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	// Third exceeds the limit.
	if _, err := svc.AuthenticateAPIKey(ctx, "goodkey"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
	// Bad key.
	if _, err := svc.AuthenticateAPIKey(ctx, "badkey"); err != ErrUnauthorized {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
	if repo.touched == 0 {
		t.Error("expected TouchAPIKey to be called for valid requests")
	}
}

// --- middleware tests ---

func TestRequireSessionMiddleware(t *testing.T) {
	hash, _ := HashPassword("password123")
	repo := &fakeRepo{users: map[string]store.User{
		"alice": {ID: 1, Username: "alice", PasswordHash: hash, Role: store.RoleAdmin, Active: true},
	}}
	svc := newTestService(t, repo, nil, 100)
	tok, _, _ := svc.Login(context.Background(), "alice", "password123")

	var gotPrincipal Principal
	handler := svc.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// No token -> 401.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rec.Code)
	}

	// Valid Bearer token -> 200 with principal.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token: status = %d, want 200", rec.Code)
	}
	if gotPrincipal.UserID != 1 {
		t.Errorf("principal not attached: %+v", gotPrincipal)
	}
}

func TestRequireAdminMiddleware(t *testing.T) {
	hash, _ := HashPassword("password123")
	repo := &fakeRepo{users: map[string]store.User{
		"admin": {ID: 1, Username: "admin", PasswordHash: hash, Role: store.RoleAdmin, Active: true},
		"user":  {ID: 2, Username: "user", PasswordHash: hash, Role: store.RoleUser, Active: true},
	}}
	svc := newTestService(t, repo, nil, 100)
	adminTok, _, _ := svc.Login(context.Background(), "admin", "password123")
	userTok, _, _ := svc.Login(context.Background(), "user", "password123")

	handler := svc.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, tc := range []struct {
		name string
		tok  string
		want int
	}{
		{"admin", adminTok, http.StatusOK},
		{"user", userTok, http.StatusForbidden},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tc.tok)
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

func TestRequireAPIKeyMiddleware(t *testing.T) {
	repo := &fakeRepo{keys: map[string]store.APIKeyUser{
		"validkey": {
			Key:  store.APIKey{ID: 5, Active: true},
			User: store.User{ID: 9, Username: "svc", Role: store.RoleUser, Active: true},
		},
		// A key with a tiny per-key limit so we can trip the limiter.
		"limitedkey": {
			Key:  store.APIKey{ID: 6, Active: true},
			User: store.User{ID: 10, Username: "lil", Role: store.RoleUser, RateLimit: 1, Active: true},
		},
	}}
	rl := NewRateLimiter(time.Minute)
	svc := newTestService(t, repo, rl, 100)

	handler := svc.RequireAPIKey(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Valid key -> 200 with rate-limit headers.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?apikey=validkey&t=caps", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("valid key: status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") != "100" {
		t.Errorf("X-RateLimit-Limit = %q, want 100", rec.Header().Get("X-RateLimit-Limit"))
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("expected X-RateLimit-Remaining header on success")
	}
	// Bad key -> 401.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?apikey=nope", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad key: status = %d, want 401", rec.Code)
	}

	// Limited key: first request OK, second exceeds limit -> 429 with Retry-After.
	req := httptest.NewRequest(http.MethodGet, "/api?apikey=limitedkey&t=caps", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?apikey=limitedkey&t=caps", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited: status = %d, want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Error("expected Retry-After header on 429")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", ra)
	}
}

// TestAPIKeyCacheAvoidsRepeatLookups verifies repeated valid-key auth within
// the TTL hits the cache (one DB lookup, one throttled last-used write) and
// that invalidation forces a fresh lookup (#107).
func TestAPIKeyCacheAvoidsRepeatLookups(t *testing.T) {
	repo := &fakeRepo{keys: map[string]store.APIKeyUser{
		"goodkey": {
			Key:  store.APIKey{ID: 7, APIKey: "goodkey", Active: true},
			User: store.User{ID: 3, Username: "carol", Role: store.RoleUser, Active: true},
		},
	}}
	svc := newTestService(t, repo, nil, 100) // no rate limiter
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := svc.AuthenticateAPIKey(ctx, "goodkey"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if repo.lookups != 1 {
		t.Errorf("DB lookups = %d, want 1 (cached after first)", repo.lookups)
	}
	if repo.touched != 1 {
		t.Errorf("last-used writes = %d, want 1 (throttled)", repo.touched)
	}
	st := svc.APIKeyCacheStats()
	if st.Hits != 4 || st.Misses != 1 {
		t.Errorf("cache stats hits=%d misses=%d, want 4/1", st.Hits, st.Misses)
	}

	// Invalidation forces a fresh lookup on the next request.
	svc.InvalidateAPIKey("goodkey")
	if _, err := svc.AuthenticateAPIKey(ctx, "goodkey"); err != nil {
		t.Fatal(err)
	}
	if repo.lookups != 2 {
		t.Errorf("after invalidation lookups = %d, want 2", repo.lookups)
	}
}

// TestAPIKeyCacheExpiryForcesLookup verifies an expired entry is re-resolved.
func TestAPIKeyCacheExpiryForcesLookup(t *testing.T) {
	repo := &fakeRepo{keys: map[string]store.APIKeyUser{
		"k": {Key: store.APIKey{ID: 1, APIKey: "k", Active: true}, User: store.User{ID: 1, Active: true}},
	}}
	ti, _ := NewTokenIssuer("secret", time.Hour)
	svc := NewService(repo, ti, nil, 100)
	// Replace the cache with a very short TTL for the test.
	svc.keyCache = newAPIKeyCache(10*time.Millisecond, time.Hour, 16)
	ctx := context.Background()

	if _, err := svc.AuthenticateAPIKey(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := svc.AuthenticateAPIKey(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if repo.lookups != 2 {
		t.Errorf("lookups = %d, want 2 (cache expired between requests)", repo.lookups)
	}
}
