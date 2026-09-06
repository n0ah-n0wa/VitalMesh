package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/requestid"
)

// Error codes returned to clients. Each is explicit about what failed
// without revealing whether an account exists.
const (
	// CodeInvalidCredentials: the email/password pair does not identify an
	// active credential. 401.
	CodeInvalidCredentials = "INVALID_CREDENTIALS" // #nosec G101 -- an error code, not a credential
	// CodeAccountDisabled: the credentials are right but the account is
	// disabled. 403.
	CodeAccountDisabled = "ACCOUNT_DISABLED"
	// CodeAuthenticationRequired: no access token was presented. 401.
	CodeAuthenticationRequired = "AUTHENTICATION_REQUIRED"
	// CodeInvalidToken: the access token is malformed, tampered with, signed
	// with an unknown key or carries unacceptable claims. 401.
	CodeInvalidToken = "INVALID_TOKEN"
	// CodeTokenExpired: the access token was valid but has expired. 401.
	CodeTokenExpired = "TOKEN_EXPIRED"
)

// Client-safe errors. Each call returns a fresh value.

// InvalidCredentials is the login failure for a wrong email or password.
func InvalidCredentials() *domain.Error {
	return domain.New(domain.KindUnauthorized, CodeInvalidCredentials, "The email or password is incorrect.")
}

// AccountDisabled is the failure for a disabled account.
func AccountDisabled() *domain.Error {
	return domain.New(domain.KindForbidden, CodeAccountDisabled, "The account is disabled.")
}

// AuthenticationRequired is the failure for a request without a token.
func AuthenticationRequired() *domain.Error {
	return domain.New(domain.KindUnauthorized, CodeAuthenticationRequired, "An access token is required.")
}

// InvalidToken is the failure for a token that cannot be accepted, with
// the internal cause attached for logging.
func InvalidToken(cause error) *domain.Error {
	return domain.Wrap(cause, domain.KindUnauthorized, CodeInvalidToken, "The access token is invalid.")
}

// TokenExpired is the failure for a token past its expiry.
func TokenExpired(cause error) *domain.Error {
	return domain.Wrap(cause, domain.KindUnauthorized, CodeTokenExpired, "The access token has expired.")
}

// TokenError maps a Verify failure to its client-safe error: expiry is
// reported as such, every other failure as an invalid token.
func TokenError(err error) *domain.Error {
	if errors.Is(err, ErrTokenExpired) {
		return TokenExpired(err)
	}
	return InvalidToken(err)
}

// UserStore is the persistence the service needs. The PostgreSQL users
// repository satisfies it.
type UserStore interface {
	GetByEmail(ctx context.Context, email string) (domain.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (domain.User, error)
	UpdatePasswordHash(ctx context.Context, id uuid.UUID, passwordHash string) error
}

// Auditor records security events. Package app adapts the audit repository.
type Auditor interface {
	// RecordLogin appends a LOGIN entry for userID under requestID.
	RecordLogin(ctx context.Context, userID uuid.UUID, requestID string) error
}

// Session is the outcome of a successful login.
type Session struct {
	Token  string
	Claims Claims
	User   domain.User
}

// Service authenticates users.
type Service struct {
	users  UserStore
	audit  Auditor
	hasher *Hasher
	tokens *Tokens
	logger *slog.Logger
	// dummyHash is verified against when the email is unknown so that the
	// response time does not reveal whether an account exists.
	dummyHash string
}

// NewService wires a Service. It hashes one throwaway password to obtain
// the timing-equalisation hash, so it costs one hash computation.
func NewService(users UserStore, audit Auditor, hasher *Hasher, tokens *Tokens, logger *slog.Logger) (*Service, error) {
	dummy, err := hasher.Hash(context.Background(), uuid.NewString())
	if err != nil {
		return nil, fmt.Errorf("prepare timing hash: %w", err)
	}
	return &Service{users: users, audit: audit, hasher: hasher, tokens: tokens, logger: logger, dummyHash: dummy}, nil
}

// NormalizeEmail returns the canonical form of an email address: trimmed
// and lower case, matching how accounts are stored.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Login verifies email and password and, on success, issues an access token
// and records a LOGIN audit entry. Wrong credentials and unknown accounts
// are indistinguishable to the caller and take the same time. A stored hash
// with outdated parameters is upgraded transparently.
func (s *Service) Login(ctx context.Context, email, password string) (Session, error) {
	user, err := s.users.GetByEmail(ctx, NormalizeEmail(email))
	if err != nil {
		var domErr *domain.Error
		if errors.As(err, &domErr) && domErr.Kind == domain.KindNotFound {
			_, _ = s.hasher.Verify(ctx, password, s.dummyHash)
			s.logger.InfoContext(ctx, "login failed", "reason", "unknown_email")
			return Session{}, InvalidCredentials()
		}
		return Session{}, fmt.Errorf("look up user: %w", err)
	}

	ok, err := s.hasher.Verify(ctx, password, user.PasswordHash)
	if err != nil {
		return Session{}, fmt.Errorf("verify password for user %s: %w", user.ID, err)
	}
	if !ok {
		s.logger.InfoContext(ctx, "login failed", "reason", "wrong_password", "user_id", user.ID)
		return Session{}, InvalidCredentials()
	}
	if user.Status != domain.UserActive {
		s.logger.InfoContext(ctx, "login failed", "reason", "account_disabled", "user_id", user.ID)
		return Session{}, AccountDisabled()
	}

	if s.hasher.NeedsRehash(user.PasswordHash) {
		s.upgradeHash(ctx, user.ID, password)
	}

	token, claims, err := s.tokens.Issue(user.ID, user.Role)
	if err != nil {
		return Session{}, fmt.Errorf("issue token: %w", err)
	}
	if err := s.audit.RecordLogin(ctx, user.ID, requestid.FromContext(ctx)); err != nil {
		return Session{}, fmt.Errorf("record login: %w", err)
	}
	s.logger.InfoContext(ctx, "login succeeded", "user_id", user.ID, "token_id", claims.TokenID)
	return Session{Token: token, Claims: claims, User: user}, nil
}

// upgradeHash re-hashes a verified password with the current parameters.
// Failure is logged and otherwise ignored: the login has already succeeded
// and the old hash keeps working.
func (s *Service) upgradeHash(ctx context.Context, userID uuid.UUID, password string) {
	hash, err := s.hasher.Hash(ctx, password)
	if err == nil {
		err = s.users.UpdatePasswordHash(ctx, userID, hash)
	}
	if err != nil {
		s.logger.WarnContext(ctx, "password hash upgrade failed", "user_id", userID, "error", err)
		return
	}
	s.logger.InfoContext(ctx, "password hash upgraded", "user_id", userID)
}

// CurrentUser loads the account behind an authenticated principal. It
// fails when the account no longer exists or has been disabled since the
// token was issued.
func (s *Service) CurrentUser(ctx context.Context, p Principal) (domain.User, error) {
	user, err := s.users.GetByID(ctx, p.UserID)
	if err != nil {
		var domErr *domain.Error
		if errors.As(err, &domErr) && domErr.Kind == domain.KindNotFound {
			return domain.User{}, InvalidToken(errors.New("subject no longer exists"))
		}
		return domain.User{}, fmt.Errorf("load user %s: %w", p.UserID, err)
	}
	if user.Status != domain.UserActive {
		return domain.User{}, AccountDisabled()
	}
	return user, nil
}
