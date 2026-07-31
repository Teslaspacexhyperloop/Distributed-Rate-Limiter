// Package auth handles JWT generation and validation for the gateway.
// JWTs are self-contained: each gateway instance validates them locally using
// the shared JWT_SECRET — zero database calls per request.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the fields the gateway reads from every JWT.
// sub  → user identity (used as the rate-limit key subject)
// plan → "free" | "pro" | "enterprise" (selects the limit tier)
// algo → optional preferred rate-limiting algorithm
type Claims struct {
	jwt.RegisteredClaims
	Plan      string `json:"plan"`
	Algorithm string `json:"algorithm,omitempty"`
}

// UserID returns the JWT subject, which is the unique user identifier.
func (c *Claims) UserID() string { return c.Subject }

// Sign creates a signed HS256 JWT for userID with the given plan and optional
// algorithm preference. The token expires after ttl.
func Sign(userID, plan, algorithm, secret string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Plan:      plan,
		Algorithm: algorithm,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// Parse validates tokenStr and returns its claims. Returns an error if the
// token is expired, tampered with, or signed with the wrong method.
func Parse(tokenStr, secret string) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := tok.Claims.(*Claims)
	if !ok || !tok.Valid {
		return nil, errors.New("invalid token claims")
	}
	return claims, nil
}
