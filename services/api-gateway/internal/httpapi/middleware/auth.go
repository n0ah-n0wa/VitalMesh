package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
)

// TokenVerifier verifies access tokens. auth.Tokens satisfies it.
type TokenVerifier interface {
	Verify(token string) (auth.Claims, error)
}

// Realm is the protection space named in WWW-Authenticate challenges.
const Realm = respond.BearerRealm

// Authenticate requires a valid bearer access token and places the
// resulting principal in the request context. Requests without a token get
// 401 AUTHENTICATION_REQUIRED; requests with an unacceptable token get 401
// INVALID_TOKEN or TOKEN_EXPIRED. Every 401 carries a WWW-Authenticate
// challenge. Tokens are never logged; the failure reason is.
func Authenticate(verifier TokenVerifier, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, present := bearerToken(r.Header.Get("Authorization"))
			if !present {
				respond.Error(w, r, logger, auth.AuthenticationRequired())
				return
			}
			claims, err := verifier.Verify(token)
			if err != nil {
				logger.InfoContext(r.Context(), "authentication failed", "reason", err.Error())
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+Realm+`", error="invalid_token"`)
				respond.Error(w, r, logger, auth.TokenError(err))
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), auth.PrincipalOf(claims))))
		})
	}
}

// bearerToken extracts the credentials of a Bearer authorization header.
// present is false when the header is absent or empty; any other header
// shape yields an empty token, which the verifier rejects as malformed.
func bearerToken(header string) (token string, present bool) {
	if header == "" {
		return "", false
	}
	scheme, credentials, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", true
	}
	return strings.TrimSpace(credentials), true
}
