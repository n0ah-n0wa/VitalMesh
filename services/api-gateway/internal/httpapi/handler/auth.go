package handler

import (
	"log/slog"
	"net/http"

	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/auth"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/domain"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/model"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/request"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/httpapi/respond"
	"github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/validate"
)

// MaxEmailLength matches the users table constraint.
const MaxEmailLength = 320

// Auth serves the authentication endpoints.
type Auth struct {
	service *auth.Service
	logger  *slog.Logger
}

// NewAuth returns an Auth handler backed by service.
func NewAuth(service *auth.Service, logger *slog.Logger) *Auth {
	return &Auth{service: service, logger: logger}
}

// Login handles POST /auth/login: it exchanges an email and password for an
// access token. The request body is never logged.
func (h *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var in model.LoginRequest
	if err := request.DecodeJSON(r, &in); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	var v validate.Validator
	v.Required("email", in.Email)
	v.MaxLength("email", in.Email, MaxEmailLength)
	v.Email("email", in.Email)
	v.Check(in.Password != "", "password", "is required")
	v.Check(len(in.Password) <= auth.MaxPasswordLength, "password", "is too long")
	if err := v.Err(); err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}

	session, err := h.service.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	body := model.LoginResponse{
		AccessToken: session.Token,
		TokenType:   "Bearer",
		ExpiresIn:   int64(session.Claims.ExpiresAt.Sub(session.Claims.IssuedAt).Seconds()),
		ExpiresAt:   session.Claims.ExpiresAt,
		User:        userModel(session.User),
	}
	if err := respond.JSON(w, http.StatusOK, body); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

// Me handles GET /auth/me: it returns the account behind the presented
// token. The route must be guarded by the authentication middleware.
func (h *Auth) Me(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		respond.Error(w, r, h.logger, auth.AuthenticationRequired())
		return
	}
	user, err := h.service.CurrentUser(r.Context(), principal)
	if err != nil {
		respond.Error(w, r, h.logger, err)
		return
	}
	if err := respond.JSON(w, http.StatusOK, userModel(user)); err != nil {
		respond.Error(w, r, h.logger, err)
	}
}

func userModel(u domain.User) model.User {
	return model.User{
		ID:        u.ID,
		Email:     u.Email,
		Role:      string(u.Role),
		Status:    string(u.Status),
		CreatedAt: u.CreatedAt,
	}
}
