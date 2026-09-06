package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

// fastParams keep the tests quick; production defaults are far costlier.
var fastParams = config.PasswordHash{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1}

func TestHashAndVerify(t *testing.T) {
	h := NewHasher(fastParams)
	hash, err := h.Hash(context.Background(), "correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("hash = %q, want a PHC argon2id string with the configured parameters", hash)
	}
	if strings.Contains(hash, "correct horse") {
		t.Error("hash contains the password")
	}

	ok, err := h.Verify(context.Background(), "correct horse battery staple", hash)
	if err != nil || !ok {
		t.Errorf("Verify(correct) = %v, %v", ok, err)
	}
	ok, err = h.Verify(context.Background(), "correct horse battery staple ", hash)
	if err != nil || ok {
		t.Errorf("Verify(near miss) = %v, %v; want false", ok, err)
	}
	ok, err = h.Verify(context.Background(), "", hash)
	if err != nil || ok {
		t.Errorf("Verify(empty) = %v, %v; want false", ok, err)
	}
	ok, err = h.Verify(context.Background(), strings.Repeat("x", MaxPasswordLength+1), hash)
	if err != nil || ok {
		t.Errorf("Verify(oversized) = %v, %v; want false without error", ok, err)
	}
}

func TestHashIsSaltedAndVerifiableAcrossParameters(t *testing.T) {
	h := NewHasher(fastParams)
	a, _ := h.Hash(context.Background(), "same password")
	b, _ := h.Hash(context.Background(), "same password")
	if a == b {
		t.Error("two hashes of the same password are identical: salt is not random")
	}

	stronger := NewHasher(config.PasswordHash{MemoryKiB: 16 * 1024, Time: 2, Parallelism: 2})
	if ok, err := stronger.Verify(context.Background(), "same password", a); err != nil || !ok {
		t.Errorf("hash with other parameters not verifiable: %v, %v", ok, err)
	}
	if !stronger.NeedsRehash(a) {
		t.Error("NeedsRehash = false for a hash with weaker parameters")
	}
	if h.NeedsRehash(a) {
		t.Error("NeedsRehash = true for a hash with the current parameters")
	}
}

func TestHashRejectsUnusablePasswords(t *testing.T) {
	h := NewHasher(fastParams)
	if _, err := h.Hash(context.Background(), ""); err == nil {
		t.Error("Hash accepted an empty password")
	}
	if _, err := h.Hash(context.Background(), strings.Repeat("x", MaxPasswordLength+1)); err == nil {
		t.Error("Hash accepted an oversized password")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	h := NewHasher(fastParams)
	good, _ := h.Hash(context.Background(), "a password to test with")
	parts := strings.Split(good, "$")

	cases := map[string]string{
		"empty":                "",
		"plain text":           "a password to test with",
		"bcrypt":               "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		"argon2i variant":      strings.Replace(good, "argon2id", "argon2i", 1),
		"wrong version":        strings.Replace(good, "v=19", "v=16", 1),
		"missing field":        strings.Join(parts[:5], "$"),
		"non numeric params":   strings.Replace(good, "m=8192", "m=lots", 1),
		"zero memory":          strings.Replace(good, "m=8192", "m=0", 1),
		"hostile memory":       strings.Replace(good, "m=8192", "m=4294967295", 1),
		"hostile iterations":   strings.Replace(good, "t=1,", "t=4000000000,", 1),
		"salt not base64":      strings.Join(append(append([]string{}, parts[:4]...), "*not*base64*", parts[5]), "$"),
		"key too short":        strings.Join(append(append([]string{}, parts[:5]...), "AAAA"), "$"),
		"trailing garbage":     good + "$extra",
		"padded base64 digest": good + "==",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := h.Verify(context.Background(), "a password to test with", encoded)
			if !errors.Is(err, ErrInvalidHash) || ok {
				t.Errorf("Verify = %v, %v; want ErrInvalidHash", ok, err)
			}
			if !h.NeedsRehash(encoded) {
				t.Error("NeedsRehash = false for a malformed hash")
			}
		})
	}
}

func TestValidateNewPassword(t *testing.T) {
	if err := ValidateNewPassword("twelve chars"); err != nil {
		t.Errorf("12-character password rejected: %v", err)
	}
	if err := ValidateNewPassword("dvanáct znaků"); err != nil {
		t.Errorf("12-rune password rejected: %v", err)
	}
	var domErr *domain.Error
	err := ValidateNewPassword("short")
	if !errors.As(err, &domErr) || domErr.Kind != domain.KindValidation || len(domErr.Details) != 1 || domErr.Details[0].Field != "password" {
		t.Errorf("short password: err = %v", err)
	}
	if err := ValidateNewPassword(strings.Repeat("x", MaxPasswordLength+1)); err == nil {
		t.Error("oversized password accepted")
	}
}

func TestHasherBoundsConcurrentComputations(t *testing.T) {
	params := fastParams
	params.MaxConcurrent = 1
	h := NewHasher(params)
	hash, err := h.Hash(context.Background(), "a password to test with")
	if err != nil {
		t.Fatal(err)
	}

	// Hold the only slot, then show that another computation waits and
	// gives up when its context ends instead of running alongside.
	h.slots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = h.Verify(ctx, "a password to test with", hash)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify while saturated: err = %v, want deadline exceeded", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Error("Verify returned before the context deadline")
	}
	if _, err := h.Hash(ctx, "another password here"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Hash while saturated: err = %v", err)
	}
	<-h.slots

	ok, err := h.Verify(context.Background(), "a password to test with", hash)
	if err != nil || !ok {
		t.Errorf("Verify after the slot was released: %v, %v", ok, err)
	}
}

func TestHasherDefaultsToOneSlotWhenUnconfigured(t *testing.T) {
	h := NewHasher(config.PasswordHash{MemoryKiB: 8 * 1024, Time: 1, Parallelism: 1})
	if cap(h.slots) != 1 {
		t.Errorf("slots = %d, want 1", cap(h.slots))
	}
}
