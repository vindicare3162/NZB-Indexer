package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the JWT claims for a SPA session token.
type Claims struct {
	UserID   int64  `json:"uid"`
	Username string `json:"usr"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// TokenIssuer signs and verifies session tokens using an HMAC secret.
type TokenIssuer struct {
	secret []byte
	ttl    time.Duration
}

// NewTokenIssuer creates a TokenIssuer. A non-empty secret is required.
func NewTokenIssuer(secret string, ttl time.Duration) (*TokenIssuer, error) {
	if secret == "" {
		return nil, fmt.Errorf("auth: JWT secret is required")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &TokenIssuer{secret: []byte(secret), ttl: ttl}, nil
}

// Issue creates a signed session token for the given user.
func (t *TokenIssuer) Issue(userID int64, username, role string) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID:   userID,
		Username: username,
		Role:     role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   fmt.Sprintf("%d", userID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(t.ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(t.secret)
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// Verify parses and validates a session token, returning its claims. It
// rejects tokens signed with an unexpected algorithm.
func (t *TokenIssuer) Verify(tokenStr string) (*Claims, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(tokenStr, &claims, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", tok.Header["alg"])
		}
		return t.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("auth: verify token: %w", err)
	}
	return &claims, nil
}
