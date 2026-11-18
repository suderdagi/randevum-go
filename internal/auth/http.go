package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/suderdagi/randevum-go/internal/httpx"
)

type ctxKey struct{}

// CurrentUser is what handlers get from Require: the id and role of the caller.
type CurrentUser struct {
	ID   int
	Role Role
}

func FromContext(ctx context.Context) CurrentUser {
	u, _ := ctx.Value(ctxKey{}).(CurrentUser)
	return u
}

// Require checks the bearer token and, when roles are given, the caller's role.
// The role is read from the database rather than the token, so a role change
// takes effect immediately.
func (s *Service) Require(next http.Handler, roles ...Role) http.Handler {
	return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			return httpx.Unauthorized("Missing bearer token")
		}
		id, err := s.ParseAccessToken(token)
		if err != nil {
			return httpx.Unauthorized("Invalid or expired token")
		}
		var role Role
		if err := s.DB.QueryRow(r.Context(), `SELECT role FROM users WHERE id = $1`, id).Scan(&role); err != nil {
			return httpx.Unauthorized("Invalid or expired token")
		}
		if len(roles) > 0 && !hasRole(roles, role) {
			return httpx.Forbidden("You don't have permission to do this")
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, CurrentUser{ID: id, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
		return nil
	})
}

func hasRole(roles []Role, role Role) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/auth/register", httpx.Handler(s.handleRegister))
	mux.Handle("POST /api/v1/auth/login", httpx.Handler(s.handleLogin))
	mux.Handle("POST /api/v1/auth/refresh", httpx.Handler(s.handleRefresh))
	mux.Handle("POST /api/v1/auth/logout", httpx.Handler(s.handleLogout))
	mux.Handle("GET /api/v1/auth/me", s.Require(httpx.Handler(s.handleMe)))
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) error {
	var in RegisterInput
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	user, tokens, err := s.Register(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"user": user, "tokens": tokens})
	return nil
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	user, tokens, err := s.Login(r.Context(), in.Email, in.Password)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": user, "tokens": tokens})
	return nil
}

type refreshBody struct {
	RefreshToken string `json:"refreshToken"`
}

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) error {
	var in refreshBody
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	tokens, err := s.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"tokens": tokens})
	return nil
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) error {
	var in refreshBody
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	if err := s.Logout(r.Context(), in.RefreshToken); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) error {
	var u User
	err := s.DB.QueryRow(r.Context(), `SELECT id, email, full_name, role FROM users WHERE id = $1`,
		FromContext(r.Context()).ID).Scan(&u.ID, &u.Email, &u.FullName, &u.Role)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": u})
	return nil
}
