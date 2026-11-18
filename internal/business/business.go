// Package business lets owners publish businesses, their services and weekly
// working hours, and lets anyone search and view them.
package business

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/suderdagi/randevum-go/internal/auth"
	"github.com/suderdagi/randevum-go/internal/httpx"
)

type Handler struct {
	DB   *pgxpool.Pool
	Auth *auth.Service
}

type Business struct {
	ID                int            `json:"id"`
	Name              string         `json:"name"`
	Slug              string         `json:"slug"`
	Category          string         `json:"category"`
	City              string         `json:"city"`
	Address           *string        `json:"address"`
	Timezone          string         `json:"timezone"`
	CancellationHours int            `json:"cancellationHours"`
	Services          []Service      `json:"services,omitempty"`
	WorkingHours      []WorkingHours `json:"workingHours,omitempty"`
}

type Service struct {
	ID              int     `json:"id"`
	Name            string  `json:"name"`
	DurationMinutes int     `json:"durationMinutes"`
	Price           float64 `json:"price"`
}

// WorkingHours uses ISO weekdays: 1 = Monday ... 7 = Sunday. A missing day means closed.
type WorkingHours struct {
	Weekday  int    `json:"weekday"`
	OpensAt  string `json:"opensAt"`  // "09:00"
	ClosesAt string `json:"closesAt"` // "18:30"
}

func (h *Handler) Routes(mux *http.ServeMux) {
	owner := func(fn httpx.Handler) http.Handler { return h.Auth.Require(fn, auth.RoleOwner) }

	mux.Handle("GET /api/v1/businesses", httpx.Handler(h.list))
	mux.Handle("GET /api/v1/businesses/{id}", httpx.Handler(h.get))
	mux.Handle("POST /api/v1/businesses", owner(h.create))
	mux.Handle("PUT /api/v1/businesses/{id}/working-hours", owner(h.setWorkingHours))
	mux.Handle("POST /api/v1/businesses/{id}/services", owner(h.createService))
}

type createInput struct {
	Name              string  `json:"name"`
	Category          string  `json:"category"`
	City              string  `json:"city"`
	Address           *string `json:"address"`
	Timezone          string  `json:"timezone"`
	CancellationHours *int    `json:"cancellationHours"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	var in createInput
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	in.Name, in.Category, in.City = strings.TrimSpace(in.Name), strings.TrimSpace(in.Category), strings.TrimSpace(in.City)
	switch {
	case in.Name == "" || len(in.Name) > 120:
		return httpx.Invalid("name", "Name is required (max 120 characters)")
	case in.Category == "":
		return httpx.Invalid("category", "Category is required")
	case in.City == "":
		return httpx.Invalid("city", "City is required")
	}
	if in.Timezone == "" {
		in.Timezone = "Europe/Istanbul"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil {
		return httpx.Invalid("timezone", "Unknown timezone, use an IANA name such as Europe/Istanbul")
	}
	cancellation := 2
	if in.CancellationHours != nil {
		if *in.CancellationHours < 0 || *in.CancellationHours > 168 {
			return httpx.Invalid("cancellationHours", "cancellationHours must be between 0 and 168")
		}
		cancellation = *in.CancellationHours
	}

	var id int
	err := h.DB.QueryRow(r.Context(),
		`INSERT INTO businesses (owner_id, name, slug, category, city, address, timezone, cancellation_hours)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		auth.FromContext(r.Context()).ID, in.Name, slugify(in.Name), strings.ToLower(in.Category), in.City,
		in.Address, in.Timezone, cancellation,
	).Scan(&id)
	if err != nil {
		return err
	}
	b, err := h.load(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, b)
	return nil
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	b, err := h.load(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, b)
	return nil
}

// list filters by ?city=, ?category= and free text ?q=, with pagination.
// Filters are added as parameters, never concatenated into the SQL.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := httpx.Page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	items := []Business{}
	total := 0
	rows, err := h.DB.Query(r.Context(),
		`SELECT id, name, slug, category, city, address, timezone, cancellation_hours, count(*) OVER () AS total
		 FROM businesses
		 WHERE ($1 = '' OR lower(city) = lower($1))
		   AND ($2 = '' OR category = lower($2))
		   AND ($3 = '' OR name ILIKE '%' || $3 || '%')
		 ORDER BY name, id
		 LIMIT $4 OFFSET $5`,
		q.Get("city"), q.Get("category"), escapeLike(q.Get("q")), limit, offset)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var b Business
		if err := rows.Scan(&b.ID, &b.Name, &b.Slug, &b.Category, &b.City, &b.Address, &b.Timezone, &b.CancellationHours, &total); err != nil {
			return err
		}
		items = append(items, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
	return nil
}

func (h *Handler) load(ctx context.Context, id int) (Business, error) {
	var b Business
	err := h.DB.QueryRow(ctx,
		`SELECT id, name, slug, category, city, address, timezone, cancellation_hours FROM businesses WHERE id = $1`, id,
	).Scan(&b.ID, &b.Name, &b.Slug, &b.Category, &b.City, &b.Address, &b.Timezone, &b.CancellationHours)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, httpx.NotFound("Business")
	}
	if err != nil {
		return b, err
	}

	rows, _ := h.DB.Query(ctx,
		`SELECT id, name, duration_minutes, price::float8 FROM services WHERE business_id = $1 AND is_active ORDER BY id`, id)
	b.Services, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Service, error) {
		var s Service
		err := row.Scan(&s.ID, &s.Name, &s.DurationMinutes, &s.Price)
		return s, err
	})
	if err != nil {
		return b, err
	}

	rows, _ = h.DB.Query(ctx,
		`SELECT weekday, to_char(opens_at, 'HH24:MI'), to_char(closes_at, 'HH24:MI')
		 FROM working_hours WHERE business_id = $1 ORDER BY weekday`, id)
	b.WorkingHours, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkingHours, error) {
		var wh WorkingHours
		err := row.Scan(&wh.Weekday, &wh.OpensAt, &wh.ClosesAt)
		return wh, err
	})
	return b, err
}

// requireOwner returns 404 (not 403) for someone else's business, so ids of other
// businesses can't be probed.
func (h *Handler) requireOwner(r *http.Request) (int, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return 0, err
	}
	var owns bool
	err = h.DB.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM businesses WHERE id = $1 AND owner_id = $2)`,
		id, auth.FromContext(r.Context()).ID).Scan(&owns)
	if err != nil {
		return 0, err
	}
	if !owns {
		return 0, httpx.NotFound("Business")
	}
	return id, nil
}

var clock = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// setWorkingHours replaces the whole week in one transaction, so a failed
// request never leaves half of the old and half of the new schedule.
func (h *Handler) setWorkingHours(w http.ResponseWriter, r *http.Request) error {
	id, err := h.requireOwner(r)
	if err != nil {
		return err
	}
	var in struct {
		Days []WorkingHours `json:"days"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, d := range in.Days {
		if d.Weekday < 1 || d.Weekday > 7 || seen[d.Weekday] {
			return httpx.Invalid("days", "Each weekday (1 = Monday ... 7 = Sunday) may appear once")
		}
		if !clock.MatchString(d.OpensAt) || !clock.MatchString(d.ClosesAt) || d.OpensAt >= d.ClosesAt {
			return httpx.Invalid("days", "Times must be HH:MM and opensAt must be before closesAt")
		}
		seen[d.Weekday] = true
	}

	err = pgx.BeginFunc(r.Context(), h.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM working_hours WHERE business_id = $1`, id); err != nil {
			return err
		}
		for _, d := range in.Days {
			_, err := tx.Exec(r.Context(),
				`INSERT INTO working_hours (business_id, weekday, opens_at, closes_at) VALUES ($1, $2, $3, $4)`,
				id, d.Weekday, d.OpensAt, d.ClosesAt)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	b, err := h.load(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, b)
	return nil
}

func (h *Handler) createService(w http.ResponseWriter, r *http.Request) error {
	id, err := h.requireOwner(r)
	if err != nil {
		return err
	}
	var in Service
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case in.Name == "":
		return httpx.Invalid("name", "Name is required")
	case in.DurationMinutes < 5 || in.DurationMinutes > 480 || in.DurationMinutes%5 != 0:
		return httpx.Invalid("durationMinutes", "Duration must be 5 to 480 minutes, in steps of 5")
	case in.Price < 0:
		return httpx.Invalid("price", "Price can't be negative")
	}
	err = h.DB.QueryRow(r.Context(),
		`INSERT INTO services (business_id, name, duration_minutes, price) VALUES ($1, $2, $3, $4) RETURNING id`,
		id, in.Name, in.DurationMinutes, in.Price).Scan(&in.ID)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, in)
	return nil
}

var turkish = strings.NewReplacer("ç", "c", "ğ", "g", "ı", "i", "ö", "o", "ş", "s", "ü", "u", "İ", "i")
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify turns "Kuaför Ayşe" into "kuafor-ayse-3f9a1c"; the random suffix keeps slugs unique.
func slugify(name string) string {
	s := nonSlug.ReplaceAllString(turkish.Replace(strings.ToLower(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return s + "-" + hex.EncodeToString(b)
}

// escapeLike makes % and _ in a search term match literally.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.TrimSpace(s))
}
