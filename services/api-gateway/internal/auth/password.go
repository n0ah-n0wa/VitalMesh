// Package auth authenticates users: it hashes and verifies passwords, issues
// and verifies access tokens, and exposes the authenticated principal to
// the rest of the request. It has no HTTP knowledge; the transport lives in
// httpapi.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// Password length bounds. The maximum bounds the hashing cost of a request;
// the minimum applies to new passwords only.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 1024
)

// ErrInvalidHash reports a stored hash that is not a well-formed Argon2id
// PHC string this package can verify.
var ErrInvalidHash = errors.New("password hash is malformed")

const (
	argon2Version = argon2.Version // 19
	saltLength    = 16
	keyLength     = 32
	minKeyLength  = 16
	maxKeyLength  = 128
	hashPrefix    = "$argon2id$"
)

// Hasher derives Argon2id password hashes with configured cost parameters.
// Hashes carry their own parameters, so a Hasher verifies hashes produced
// with any parameters and reports when one should be upgraded.
//
// Argon2id is memory-hard by design, so every computation commits
// MemoryKiB of memory. The Hasher bounds how many run at once; callers
// wait for a slot until their context ends. Without the bound a burst of
// unauthenticated login attempts could exhaust the process's memory.
type Hasher struct {
	params config.PasswordHash
	slots  chan struct{}
}

// NewHasher returns a Hasher using the configured cost parameters.
func NewHasher(params config.PasswordHash) *Hasher {
	concurrent := params.MaxConcurrent
	if concurrent <= 0 {
		concurrent = 1
	}
	return &Hasher{params: params, slots: make(chan struct{}, concurrent)}
}

// acquire waits for a hashing slot or for ctx to end.
func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for password hashing slot: %w", ctx.Err())
	}
}

func (h *Hasher) release() { <-h.slots }

// Hash returns the PHC-encoded Argon2id hash of password with a fresh
// random salt, for example
// $argon2id$v=19$m=65536,t=3,p=1$<salt>$<hash>.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if password == "" || len(password) > MaxPasswordLength {
		return "", fmt.Errorf("password length must be between 1 and %d bytes", MaxPasswordLength)
	}
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.MemoryKiB, h.params.Parallelism, keyLength)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s",
		hashPrefix, argon2Version, h.params.MemoryKiB, h.params.Time, h.params.Parallelism,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify reports whether password produces encoded. The comparison is
// constant-time. ErrInvalidHash is returned when encoded cannot be parsed;
// a wrong password is not an error.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	if len(password) > MaxPasswordLength {
		return false, nil
	}
	parsed, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	key := argon2.IDKey([]byte(password), parsed.salt, parsed.params.Time, parsed.params.MemoryKiB, parsed.params.Parallelism, parsed.keyLength)
	return subtle.ConstantTimeCompare(key, parsed.key) == 1, nil
}

// NeedsRehash reports whether encoded was produced with parameters other
// than the configured ones, so that a hash can be upgraded once its password
// is known again (at login).
func (h *Hasher) NeedsRehash(encoded string) bool {
	parsed, err := parseHash(encoded)
	if err != nil {
		return true
	}
	return parsed.params.MemoryKiB != h.params.MemoryKiB ||
		parsed.params.Time != h.params.Time ||
		parsed.params.Parallelism != h.params.Parallelism ||
		parsed.keyLength != keyLength
}

type parsedHash struct {
	params    config.PasswordHash
	salt      []byte
	key       []byte
	keyLength uint32
}

// parseHash reads a PHC string produced by Hash. Parameters are bounded so a
// corrupt or hostile hash cannot make verification arbitrarily expensive.
func parseHash(encoded string) (parsedHash, error) {
	if !strings.HasPrefix(encoded, hashPrefix) {
		return parsedHash{}, ErrInvalidHash
	}
	parts := strings.Split(strings.TrimPrefix(encoded, hashPrefix), "$")
	if len(parts) != 4 {
		return parsedHash{}, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[0], "v=%d", &version); err != nil || version != argon2Version {
		return parsedHash{}, ErrInvalidHash
	}
	var p parsedHash
	var parallelism uint32
	if _, err := fmt.Sscanf(parts[1], "m=%d,t=%d,p=%d", &p.params.MemoryKiB, &p.params.Time, &parallelism); err != nil {
		return parsedHash{}, ErrInvalidHash
	}
	if p.params.MemoryKiB == 0 || p.params.MemoryKiB > config.MaxPasswordHashMemoryKiB ||
		p.params.Time == 0 || p.params.Time > config.MaxPasswordHashTime ||
		parallelism == 0 || parallelism > config.MaxPasswordHashThreads {
		return parsedHash{}, ErrInvalidHash
	}
	p.params.Parallelism = uint8(parallelism)

	enc := base64.RawStdEncoding
	var err error
	if p.salt, err = enc.DecodeString(parts[2]); err != nil || len(p.salt) < 8 {
		return parsedHash{}, ErrInvalidHash
	}
	if p.key, err = enc.DecodeString(parts[3]); err != nil || len(p.key) < minKeyLength || len(p.key) > maxKeyLength {
		return parsedHash{}, ErrInvalidHash
	}
	p.keyLength = uint32(len(p.key)) // #nosec G115 -- bounded to [minKeyLength, maxKeyLength] above
	return p, nil
}

// ValidateNewPassword checks a password chosen for an account against the
// length policy. It returns a domain validation error naming the field.
func ValidateNewPassword(password string) error {
	var v validate.Validator
	v.Check(utf8.RuneCountInString(password) >= MinPasswordLength, "password",
		fmt.Sprintf("must be at least %d characters", MinPasswordLength))
	v.Check(len(password) <= MaxPasswordLength, "password",
		fmt.Sprintf("must be at most %d bytes", MaxPasswordLength))
	return v.Err()
}
