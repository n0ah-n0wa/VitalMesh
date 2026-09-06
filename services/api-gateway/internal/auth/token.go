package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// Token verification failures. None of them ever carries token material, so
// they are safe to log.
var (
	ErrTokenMalformed  = errors.New("token is malformed")
	ErrTokenUnknownKey = errors.New("token key id is unknown")
	ErrTokenSignature  = errors.New("token signature is invalid")
	ErrTokenExpired    = errors.New("token has expired")
	ErrTokenClaims     = errors.New("token claims are invalid")
)

// MaxTokenLength bounds the tokens Verify will parse.
const MaxTokenLength = 4096

const (
	algorithm = "HS256"
	tokenType = "JWT"
)

// Claims are the verified contents of an access token.
type Claims struct {
	// Subject is the authenticated user.
	Subject uuid.UUID
	Role    domain.Role
	// TokenID is the unique identifier (jti) of the token.
	TokenID   string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Tokens issues and verifies HS256 JSON Web Tokens. Every token names its
// signing key in the `kid` header, so the active key can be rotated while
// tokens signed with retired keys stay verifiable until they expire.
type Tokens struct {
	keyID  string
	secret []byte
	keys   map[string][]byte
	issuer string
	ttl    time.Duration
	skew   time.Duration
	now    func() time.Time
}

// NewTokens returns a Tokens using cfg. now supplies the current time; nil
// means time.Now.
func NewTokens(cfg config.JWT, now func() time.Time) *Tokens {
	if now == nil {
		now = time.Now
	}
	keys := make(map[string][]byte, len(cfg.PreviousSecrets)+1)
	for kid, secret := range cfg.PreviousSecrets {
		keys[kid] = secret
	}
	keys[cfg.KeyID] = cfg.Secret
	return &Tokens{
		keyID:  cfg.KeyID,
		secret: cfg.Secret,
		keys:   keys,
		issuer: cfg.Issuer,
		ttl:    cfg.TTL,
		skew:   cfg.ClockSkew,
		now:    now,
	}
}

// TTL is the lifetime of issued tokens.
func (t *Tokens) TTL() time.Duration { return t.ttl }

type header struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ,omitempty"`
	KeyID     string `json:"kid"`
}

type payload struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Role      string `json:"role"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	TokenID   string `json:"jti"`
}

// Issue signs a new token for subject with role and returns it with its
// claims.
func (t *Tokens) Issue(subject uuid.UUID, role domain.Role) (string, Claims, error) {
	if subject == uuid.Nil {
		return "", Claims{}, errors.New("issue token: subject is required")
	}
	if !validRole(role) {
		return "", Claims{}, fmt.Errorf("issue token: unknown role %q", role)
	}
	now := t.now().UTC().Truncate(time.Second)
	claims := Claims{
		Subject:   subject,
		Role:      role,
		TokenID:   uuid.NewString(),
		IssuedAt:  now,
		ExpiresAt: now.Add(t.ttl),
	}

	head, err := json.Marshal(header{Algorithm: algorithm, Type: tokenType, KeyID: t.keyID})
	if err != nil {
		return "", Claims{}, fmt.Errorf("encode header: %w", err)
	}
	body, err := json.Marshal(payload{
		Issuer:    t.issuer,
		Subject:   claims.Subject.String(),
		Role:      string(claims.Role),
		IssuedAt:  claims.IssuedAt.Unix(),
		ExpiresAt: claims.ExpiresAt.Unix(),
		TokenID:   claims.TokenID,
	})
	if err != nil {
		return "", Claims{}, fmt.Errorf("encode claims: %w", err)
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(head) + "." + enc.EncodeToString(body)
	signature := sign(t.secret, signingInput)
	return signingInput + "." + enc.EncodeToString(signature), claims, nil
}

// Verify checks token's structure, key, signature, issuer and time claims
// and returns its claims. Failures are one of the Err* values of this
// package, possibly wrapped with a reason that never includes the token.
func (t *Tokens) Verify(token string) (Claims, error) {
	if token == "" || len(token) > MaxTokenLength {
		return Claims{}, ErrTokenMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, ErrTokenMalformed
	}
	enc := base64.RawURLEncoding

	rawHeader, err := enc.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	var head header
	if err := json.Unmarshal(rawHeader, &head); err != nil {
		return Claims{}, ErrTokenMalformed
	}
	// The algorithm is fixed; a token naming any other one (including
	// "none") is rejected before its key is even looked up.
	if head.Algorithm != algorithm || (head.Type != "" && head.Type != tokenType) {
		return Claims{}, fmt.Errorf("%w: unsupported algorithm or type", ErrTokenMalformed)
	}
	secret, ok := t.keys[head.KeyID]
	if !ok {
		return Claims{}, ErrTokenUnknownKey
	}

	signature, err := enc.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	if !hmac.Equal(signature, sign(secret, parts[0]+"."+parts[1])) {
		return Claims{}, ErrTokenSignature
	}

	// Only a token with a valid signature reaches the claims, so every
	// failure below is a token we issued that is no longer acceptable.
	rawPayload, err := enc.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	var body payload
	if err := json.Unmarshal(rawPayload, &body); err != nil {
		return Claims{}, fmt.Errorf("%w: undecodable payload", ErrTokenClaims)
	}
	return t.claimsOf(body)
}

func (t *Tokens) claimsOf(body payload) (Claims, error) {
	if body.Issuer != t.issuer {
		return Claims{}, fmt.Errorf("%w: issuer mismatch", ErrTokenClaims)
	}
	subject, err := uuid.Parse(body.Subject)
	if err != nil || subject == uuid.Nil {
		return Claims{}, fmt.Errorf("%w: subject is not a user id", ErrTokenClaims)
	}
	role := domain.Role(body.Role)
	if !validRole(role) {
		return Claims{}, fmt.Errorf("%w: unknown role", ErrTokenClaims)
	}
	if body.TokenID == "" || len(body.TokenID) > 64 {
		return Claims{}, fmt.Errorf("%w: missing token id", ErrTokenClaims)
	}
	if body.IssuedAt <= 0 || body.ExpiresAt <= body.IssuedAt {
		return Claims{}, fmt.Errorf("%w: invalid time claims", ErrTokenClaims)
	}
	claims := Claims{
		Subject:   subject,
		Role:      role,
		TokenID:   body.TokenID,
		IssuedAt:  time.Unix(body.IssuedAt, 0).UTC(),
		ExpiresAt: time.Unix(body.ExpiresAt, 0).UTC(),
	}
	now := t.now()
	if claims.IssuedAt.After(now.Add(t.skew)) {
		return Claims{}, fmt.Errorf("%w: issued in the future", ErrTokenClaims)
	}
	if !now.Before(claims.ExpiresAt.Add(t.skew)) {
		return Claims{}, ErrTokenExpired
	}
	return claims, nil
}

func sign(secret []byte, signingInput string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

func validRole(role domain.Role) bool {
	switch role {
	case domain.RoleAdmin, domain.RoleOperator, domain.RoleUser:
		return true
	}
	return false
}
