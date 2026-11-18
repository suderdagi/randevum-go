// Package auth handles accounts and tokens.
//
// Access tokens are short-lived JWTs (15 minutes) checked without a database query.
// Refresh tokens are random strings stored only as SHA-256 hashes; each one can be
// used once. If a used token is presented again it has probably been stolen, so
// every session of that user is revoked.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/suderdagi/randevum-go/internal/httpx"
)

type Role string

const (
	RoleCustomer Role = "customer"
	RoleOwner    Role = "owner"
)

type User struct {
	ID       int    `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"fullName"`
	Role     Role   `json:"role"`
}

type Tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"` // seconds until the access token expires
}

type Service struct {
	DB         *pgxpool.Pool
	Secret     []byte
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

type RegisterInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	FullName string `json:"fullName"`
	Role     Role   `json:"role"`
}

func (in *RegisterInput) validate() error {
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.FullName = strings.TrimSpace(in.FullName)
	if _, err := mail.ParseAddress(in.Email); err != nil {
		return httpx.Invalid("email", "Invalid email address")
	}
	if len(in.Password) < 8 || len(in.Password) > 72 { // bcrypt only uses the first 72 bytes
		return httpx.Invalid("password", "Password must be 8 to 72 characters")
	}
	if in.FullName == "" {
		return httpx.Invalid("fullName", "Full name is required")
	}
	if in.Role == "" {
		in.Role = RoleCustomer
	}
	if in.Role != RoleCustomer && in.Role != RoleOwner {
		return httpx.Invalid("role", "Role must be customer or owner")
	}
	return nil
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (User, Tokens, error) {
	if err := in.validate(); err != nil {
		return User{}, Tokens{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, Tokens{}, err
	}
	var u User
	err = s.DB.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, full_name, role) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (email) DO NOTHING
		 RETURNING id, email, full_name, role`,
		in.Email, string(hash), in.FullName, in.Role,
	).Scan(&u.ID, &u.Email, &u.FullName, &u.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, Tokens{}, httpx.Conflict("EMAIL_TAKEN", "An account with this email already exists")
	}
	if err != nil {
		return User{}, Tokens{}, err
	}
	tokens, err := s.issue(ctx, s.DB, u)
	return u, tokens, err
}

func (s *Service) Login(ctx context.Context, email, password string) (User, Tokens, error) {
	var u User
	var hash string
	err := s.DB.QueryRow(ctx,
		`SELECT id, email, full_name, role, password_hash FROM users WHERE email = $1`,
		strings.ToLower(strings.TrimSpace(email)),
	).Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Compare anyway so a missing account takes as long as a wrong password
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return User{}, Tokens{}, httpx.Unauthorized("Invalid email or password")
	}
	if err != nil {
		return User{}, Tokens{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return User{}, Tokens{}, httpx.Unauthorized("Invalid email or password")
	}
	tokens, err := s.issue(ctx, s.DB, u)
	return u, tokens, err
}

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

// Refresh exchanges a refresh token for a new pair. The old token is revoked in the
// same transaction, and the row lock means two parallel refreshes can't both succeed.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	var tokens Tokens
	reused := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var (
			id        int
			u         User
			expiresAt time.Time
			revokedAt *time.Time
		)
		err := tx.QueryRow(ctx,
			`SELECT rt.id, rt.expires_at, rt.revoked_at, u.id, u.email, u.full_name, u.role
			 FROM refresh_tokens rt JOIN users u ON u.id = rt.user_id
			 WHERE rt.token_hash = $1 FOR UPDATE OF rt`,
			hashToken(refreshToken),
		).Scan(&id, &expiresAt, &revokedAt, &u.ID, &u.Email, &u.FullName, &u.Role)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.Unauthorized("Invalid refresh token")
		}
		if err != nil {
			return err
		}
		if revokedAt != nil {
			// Reuse of a rotated token: log the user out everywhere
			_, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, u.ID)
			reused = true
			return err // nil commits the revocation; the 401 is returned below
		}
		if time.Now().After(expiresAt) {
			return httpx.Unauthorized("Refresh token expired")
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		tokens, err = s.issue(ctx, tx, u)
		return err
	})
	if err == nil && reused {
		return Tokens{}, httpx.Unauthorized("Refresh token was already used; all sessions have been signed out")
	}
	return tokens, err
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	_, err := s.DB.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, hashToken(refreshToken))
	return err
}

// execer is satisfied by both the pool and a transaction, so tokens can be issued inside either.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Service) issue(ctx context.Context, db execer, u User) (Tokens, error) {
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   strconv.Itoa(u.ID),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(s.AccessTTL)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	}).SignedString(s.Secret)
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := randomToken()
	if err != nil {
		return Tokens{}, err
	}
	_, err = db.Exec(ctx, `INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		u.ID, hashToken(refresh), time.Now().Add(s.RefreshTTL))
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{AccessToken: access, RefreshToken: refresh, ExpiresIn: int(s.AccessTTL.Seconds())}, nil
}

// ParseAccessToken returns the user id in a valid, unexpired access token.
func (s *Service) ParseAccessToken(token string) (int, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return s.Secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(claims.Subject)
	if err != nil {
		return 0, fmt.Errorf("bad subject: %w", err)
	}
	return id, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
