package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/config"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
)

var (
	testSecret     = []byte("test-secret-test-secret-test-secret-32")
	previousSecret = []byte("previous-secret-previous-secret-32")
	otherSecret    = []byte("another-secret-another-secret-32!")
	epoch          = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
)

func testJWT() config.JWT {
	return config.JWT{
		KeyID:           "k2",
		Secret:          testSecret,
		PreviousSecrets: map[string]config.Secret{"k1": previousSecret},
		Issuer:          "vitalmesh-test",
		TTL:             15 * time.Minute,
		ClockSkew:       30 * time.Second,
	}
}

// clock is a settable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTokens(t *testing.T) (*Tokens, *clock) {
	t.Helper()
	c := &clock{t: epoch}
	return NewTokens(testJWT(), c.now), c
}

func decodeSegment(t *testing.T, seg string, dst any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
}

func TestIssueProducesRequiredClaims(t *testing.T) {
	tokens, _ := newTokens(t)
	subject := uuid.New()

	token, claims, err := tokens.Issue(subject, domain.RoleOperator)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	var head map[string]any
	decodeSegment(t, parts[0], &head)
	if head["alg"] != "HS256" || head["typ"] != "JWT" || head["kid"] != "k2" {
		t.Errorf("header = %v", head)
	}
	var body map[string]any
	decodeSegment(t, parts[1], &body)
	for _, claim := range []string{"iss", "sub", "role", "iat", "exp", "jti"} {
		if _, ok := body[claim]; !ok {
			t.Errorf("claim %s missing: %v", claim, body)
		}
	}
	if body["sub"] != subject.String() || body["role"] != "OPERATOR" || body["iss"] != "vitalmesh-test" {
		t.Errorf("identity claims = %v", body)
	}
	if body["iat"] != float64(epoch.Unix()) || body["exp"] != float64(epoch.Add(15*time.Minute).Unix()) {
		t.Errorf("time claims = iat %v exp %v", body["iat"], body["exp"])
	}
	if claims.Subject != subject || claims.Role != domain.RoleOperator || claims.TokenID != body["jti"] ||
		!claims.IssuedAt.Equal(epoch) || !claims.ExpiresAt.Equal(epoch.Add(15*time.Minute)) {
		t.Errorf("returned claims = %+v", claims)
	}
	if _, err := uuid.Parse(claims.TokenID); err != nil {
		t.Errorf("jti %q is not a UUID", claims.TokenID)
	}
}

func TestTokenIDsAreUnique(t *testing.T) {
	tokens, _ := newTokens(t)
	seen := map[string]bool{}
	for range 100 {
		_, claims, err := tokens.Issue(uuid.New(), domain.RoleUser)
		if err != nil {
			t.Fatal(err)
		}
		if seen[claims.TokenID] {
			t.Fatalf("duplicate token id %s", claims.TokenID)
		}
		seen[claims.TokenID] = true
	}
}

func TestIssueRejectsInvalidInput(t *testing.T) {
	tokens, _ := newTokens(t)
	if _, _, err := tokens.Issue(uuid.Nil, domain.RoleUser); err == nil {
		t.Error("Issue accepted a nil subject")
	}
	if _, _, err := tokens.Issue(uuid.New(), domain.Role("ROOT")); err == nil {
		t.Error("Issue accepted an unknown role")
	}
}

func TestVerifyRoundTrip(t *testing.T) {
	tokens, c := newTokens(t)
	token, issued, err := tokens.Issue(uuid.New(), domain.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	c.t = epoch.Add(14 * time.Minute)
	claims, err := tokens.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims != issued {
		t.Errorf("claims = %+v, want %+v", claims, issued)
	}
}

func TestVerifyExpiry(t *testing.T) {
	tokens, c := newTokens(t)
	token, _, err := tokens.Issue(uuid.New(), domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}

	c.t = epoch.Add(15*time.Minute + 29*time.Second)
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("token within clock skew rejected: %v", err)
	}
	c.t = epoch.Add(15*time.Minute + 30*time.Second)
	if _, err := tokens.Verify(token); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("token past expiry: err = %v, want ErrTokenExpired", err)
	}
	c.t = epoch.Add(24 * time.Hour)
	if _, err := tokens.Verify(token); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("token long past expiry: err = %v, want ErrTokenExpired", err)
	}
}

func TestVerifyRejectsTokensFromTheFuture(t *testing.T) {
	tokens, c := newTokens(t)
	token, _, err := tokens.Issue(uuid.New(), domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	c.t = epoch.Add(-31 * time.Second)
	if _, err := tokens.Verify(token); !errors.Is(err, ErrTokenClaims) {
		t.Errorf("token issued in the future: err = %v, want ErrTokenClaims", err)
	}
	c.t = epoch.Add(-29 * time.Second)
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("token within clock skew rejected: %v", err)
	}
}

func TestVerifyAcceptsPreviousKeyAndRejectsUnknownKey(t *testing.T) {
	c := &clock{t: epoch}
	old := NewTokens(config.JWT{KeyID: "k1", Secret: previousSecret, Issuer: "vitalmesh-test", TTL: time.Minute, ClockSkew: time.Second}, c.now)
	token, _, err := old.Issue(uuid.New(), domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	current := NewTokens(testJWT(), c.now)
	if _, err := current.Verify(token); err != nil {
		t.Errorf("token signed with the previous key rejected after rotation: %v", err)
	}

	retired := NewTokens(config.JWT{KeyID: "k3", Secret: otherSecret, Issuer: "vitalmesh-test", TTL: time.Minute, ClockSkew: time.Second}, c.now)
	if _, err := retired.Verify(token); !errors.Is(err, ErrTokenUnknownKey) {
		t.Errorf("token with unknown kid: err = %v, want ErrTokenUnknownKey", err)
	}
}

func TestVerifyRejectsMalformedTokens(t *testing.T) {
	tokens, _ := newTokens(t)
	valid, _, _ := tokens.Issue(uuid.New(), domain.RoleUser)
	parts := strings.Split(valid, ".")

	cases := map[string]string{
		"empty":               "",
		"not a jwt":           "definitely-not-a-token",
		"two segments":        parts[0] + "." + parts[1],
		"four segments":       valid + ".extra",
		"empty signature":     parts[0] + "." + parts[1] + ".",
		"header not base64":   "!!!." + parts[1] + "." + parts[2],
		"header not json":     base64.RawURLEncoding.EncodeToString([]byte("{")) + "." + parts[1] + "." + parts[2],
		"signature not b64":   parts[0] + "." + parts[1] + ".###",
		"padded base64":       parts[0] + "." + parts[1] + "." + parts[2] + "==",
		"oversized":           strings.Repeat("a", MaxTokenLength+1),
		"whitespace":          " " + valid,
		"bearer prefix":       "Bearer " + valid,
		"different algorithm": segment(t, map[string]any{"alg": "HS512", "typ": "JWT", "kid": "k2"}) + "." + parts[1] + "." + parts[2],
		"algorithm none":      segment(t, map[string]any{"alg": "none", "kid": "k2"}) + "." + parts[1] + ".",
		"wrong type":          segment(t, map[string]any{"alg": "HS256", "typ": "JWE", "kid": "k2"}) + "." + parts[1] + "." + parts[2],
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tokens.Verify(token)
			if !errors.Is(err, ErrTokenMalformed) {
				t.Errorf("err = %v, want ErrTokenMalformed", err)
			}
		})
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	tokens, _ := newTokens(t)
	subject := uuid.New()
	valid, _, err := tokens.Issue(subject, domain.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, ".")

	var body map[string]any
	decodeSegment(t, parts[1], &body)
	escalated := map[string]any{}
	for k, v := range body {
		escalated[k] = v
	}
	escalated["role"] = "ADMIN"
	extended := map[string]any{}
	for k, v := range body {
		extended[k] = v
	}
	extended["exp"] = epoch.Add(365 * 24 * time.Hour).Unix()

	forged := NewTokens(config.JWT{KeyID: "k2", Secret: otherSecret, Issuer: "vitalmesh-test", TTL: time.Hour, ClockSkew: time.Second}, func() time.Time { return epoch })
	forgedToken, _, _ := forged.Issue(subject, domain.RoleAdmin)

	cases := map[string]string{
		"role escalated":       parts[0] + "." + segment(t, escalated) + "." + parts[2],
		"expiry extended":      parts[0] + "." + segment(t, extended) + "." + parts[2],
		"signature bit flip":   parts[0] + "." + parts[1] + "." + flipLastChar(parts[2]),
		"signature truncated":  parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4],
		"signature from other": parts[0] + "." + parts[1] + "." + strings.Split(forgedToken, ".")[2],
		"forged with own key":  forgedToken,
		"header kid swapped":   segment(t, map[string]any{"alg": "HS256", "typ": "JWT", "kid": "k1"}) + "." + parts[1] + "." + parts[2],
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tokens.Verify(token)
			if !errors.Is(err, ErrTokenSignature) {
				t.Errorf("err = %v, want ErrTokenSignature", err)
			}
		})
	}
}

func TestVerifyRejectsUnacceptableClaims(t *testing.T) {
	c := &clock{t: epoch}
	tokens := NewTokens(testJWT(), c.now)
	base := func() map[string]any {
		return map[string]any{
			"iss": "vitalmesh-test", "sub": uuid.NewString(), "role": "USER",
			"iat": epoch.Unix(), "exp": epoch.Add(time.Minute).Unix(), "jti": uuid.NewString(),
		}
	}
	mutate := func(f func(m map[string]any)) string {
		m := base()
		f(m)
		return signedWith(t, testSecret, "k2", m)
	}

	cases := map[string]string{
		"wrong issuer":      mutate(func(m map[string]any) { m["iss"] = "someone-else" }),
		"missing issuer":    mutate(func(m map[string]any) { delete(m, "iss") }),
		"subject not uuid":  mutate(func(m map[string]any) { m["sub"] = "alice" }),
		"nil subject":       mutate(func(m map[string]any) { m["sub"] = uuid.Nil.String() }),
		"unknown role":      mutate(func(m map[string]any) { m["role"] = "SUPERUSER" }),
		"missing role":      mutate(func(m map[string]any) { delete(m, "role") }),
		"missing jti":       mutate(func(m map[string]any) { delete(m, "jti") }),
		"oversized jti":     mutate(func(m map[string]any) { m["jti"] = strings.Repeat("j", 65) }),
		"missing iat":       mutate(func(m map[string]any) { delete(m, "iat") }),
		"missing exp":       mutate(func(m map[string]any) { delete(m, "exp") }),
		"exp before iat":    mutate(func(m map[string]any) { m["exp"] = epoch.Add(-time.Minute).Unix() }),
		"exp not a number":  mutate(func(m map[string]any) { m["exp"] = "later" }),
		"payload not json":  signedRaw(t, testSecret, "k2", []byte("not json")),
		"payload is array":  signedRaw(t, testSecret, "k2", []byte("[]")),
		"payload not b64":   signedSegments(testSecret, segment(t, map[string]any{"alg": "HS256", "kid": "k2"}), "!!!"),
		"iat in the future": mutate(func(m map[string]any) { m["iat"] = epoch.Add(time.Hour).Unix() }),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tokens.Verify(token)
			if !errors.Is(err, ErrTokenClaims) && !errors.Is(err, ErrTokenMalformed) {
				t.Errorf("err = %v, want ErrTokenClaims or ErrTokenMalformed", err)
			}
			if err != nil && strings.Contains(err.Error(), token[len(token)-20:]) {
				t.Errorf("error message leaks token material: %v", err)
			}
		})
	}
}

func TestVerifyIgnoresUnknownClaimsAndHeaders(t *testing.T) {
	tokens, _ := newTokens(t)
	m := map[string]any{
		"iss": "vitalmesh-test", "sub": uuid.NewString(), "role": "USER",
		"iat": epoch.Unix(), "exp": epoch.Add(time.Minute).Unix(), "jti": "abc", "aud": "future", "custom": 1,
	}
	token := signedRawHeader(t, testSecret, map[string]any{"alg": "HS256", "kid": "k2", "cty": "x"}, mustJSON(t, m))
	if _, err := tokens.Verify(token); err != nil {
		t.Errorf("forward-compatible token rejected: %v", err)
	}
}

// helpers

func segment(t *testing.T, v any) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(mustJSON(t, v))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func signedWith(t *testing.T, secret []byte, kid string, claims map[string]any) string {
	t.Helper()
	return signedRaw(t, secret, kid, mustJSON(t, claims))
}

func signedRaw(t *testing.T, secret []byte, kid string, payload []byte) string {
	t.Helper()
	return signedRawHeader(t, secret, map[string]any{"alg": "HS256", "typ": "JWT", "kid": kid}, payload)
}

func signedRawHeader(t *testing.T, secret []byte, head map[string]any, payload []byte) string {
	t.Helper()
	return signedSegments(secret, segment(t, head), base64.RawURLEncoding.EncodeToString(payload))
}

// signedSegments signs already-encoded header and payload segments, so a
// test can present a validly signed token with an undecodable segment.
func signedSegments(secret []byte, headSeg, payloadSeg string) string {
	input := headSeg + "." + payloadSeg
	return input + "." + base64.RawURLEncoding.EncodeToString(sign(secret, input))
}

func flipLastChar(s string) string {
	last := s[len(s)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return s[:len(s)-1] + string(replacement)
}
