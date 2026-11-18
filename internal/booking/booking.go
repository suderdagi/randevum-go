// Package booking calculates free slots and manages appointments.
package booking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/suderdagi/randevum-go/internal/auth"
	"github.com/suderdagi/randevum-go/internal/httpx"
)

const maxDaysAhead = 60

type Handler struct {
	DB   *pgxpool.Pool
	Auth *auth.Service
	Now  func() time.Time // replaceable in tests
}

type Appointment struct {
	ID         int       `json:"id"`
	BusinessID int       `json:"businessId"`
	Business   string    `json:"business"`
	ServiceID  int       `json:"serviceId"`
	Service    string    `json:"service"`
	CustomerID int       `json:"customerId"`
	StartsAt   time.Time `json:"startsAt"`
	EndsAt     time.Time `json:"endsAt"`
	Status     Status    `json:"status"`
	Note       *string   `json:"note"`
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/businesses/{id}/availability", httpx.Handler(h.availability))
	mux.Handle("POST /api/v1/appointments", h.Auth.Require(httpx.Handler(h.create), auth.RoleCustomer))
	mux.Handle("GET /api/v1/appointments/me", h.Auth.Require(httpx.Handler(h.mine)))
	mux.Handle("PATCH /api/v1/appointments/{id}/status", h.Auth.Require(httpx.Handler(h.updateStatus)))
}

type service struct {
	ID         int
	BusinessID int
	Duration   time.Duration
	Location   *time.Location
}

func (h *Handler) loadService(ctx context.Context, id int) (service, error) {
	var s service
	var minutes int
	var tz string
	err := h.DB.QueryRow(ctx,
		`SELECT s.id, s.business_id, s.duration_minutes, b.timezone
		 FROM services s JOIN businesses b ON b.id = s.business_id
		 WHERE s.id = $1 AND s.is_active`, id,
	).Scan(&s.ID, &s.BusinessID, &minutes, &tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, httpx.NotFound("Service")
	}
	if err != nil {
		return s, err
	}
	s.Duration = time.Duration(minutes) * time.Minute
	s.Location, err = time.LoadLocation(tz)
	return s, err
}

func (h *Handler) loadHours(ctx context.Context, businessID int, day time.Time) (*Hours, error) {
	var hrs Hours
	err := h.DB.QueryRow(ctx,
		`SELECT to_char(opens_at, 'HH24:MI'), to_char(closes_at, 'HH24:MI')
		 FROM working_hours WHERE business_id = $1 AND weekday = $2`,
		businessID, isoWeekday(day),
	).Scan(&hrs.OpensAt, &hrs.ClosesAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // closed that day
	}
	return &hrs, err
}

func (h *Handler) availability(w http.ResponseWriter, r *http.Request) error {
	businessID, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	serviceID, err := strconv.Atoi(r.URL.Query().Get("serviceId"))
	if err != nil {
		return httpx.Invalid("serviceId", "serviceId is required")
	}
	svc, err := h.loadService(r.Context(), serviceID)
	if err != nil {
		return err
	}
	if svc.BusinessID != businessID {
		return httpx.NotFound("Service")
	}
	date := r.URL.Query().Get("date")
	day, err := time.ParseInLocation(time.DateOnly, date, svc.Location)
	if err != nil {
		return httpx.Invalid("date", "date must be YYYY-MM-DD")
	}

	hours, err := h.loadHours(r.Context(), businessID, day)
	if err != nil {
		return err
	}
	rows, _ := h.DB.Query(r.Context(),
		`SELECT starts_at, ends_at FROM appointments
		 WHERE business_id = $1 AND status IN ('pending', 'confirmed') AND starts_at < $3 AND ends_at > $2`,
		businessID, day, day.AddDate(0, 0, 1))
	busy, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Range, error) {
		var b Range
		err := row.Scan(&b.Start, &b.End)
		return b, err
	})
	if err != nil {
		return err
	}

	slots := Slots(day, svc.Location, hours, svc.Duration, busy, h.Now())
	formatted := make([]string, len(slots))
	for i, s := range slots {
		formatted[i] = s.In(svc.Location).Format(time.RFC3339)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"date": date, "timezone": svc.Location.String(), "durationMinutes": int(svc.Duration.Minutes()), "slots": formatted,
	})
	return nil
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ServiceID int     `json:"serviceId"`
		StartsAt  string  `json:"startsAt"` // RFC 3339 with an offset, e.g. 2026-12-01T10:30:00+03:00
		Note      *string `json:"note"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	start, err := time.Parse(time.RFC3339, in.StartsAt)
	if err != nil {
		return httpx.Invalid("startsAt", "startsAt must be an RFC 3339 time with an offset")
	}
	if in.Note != nil && len(*in.Note) > 500 {
		return httpx.Invalid("note", "Note can be at most 500 characters")
	}
	svc, err := h.loadService(r.Context(), in.ServiceID)
	if err != nil {
		return err
	}

	now := h.Now()
	if !start.After(now) {
		return httpx.BadRequest("Appointment time must be in the future")
	}
	if start.After(now.AddDate(0, 0, maxDaysAhead)) {
		return httpx.BadRequest(fmt.Sprintf("Appointments can be booked at most %d days ahead", maxDaysAhead))
	}
	hours, err := h.loadHours(r.Context(), svc.BusinessID, start.In(svc.Location))
	if err != nil {
		return err
	}
	if !IsValidStart(start, svc.Location, hours, svc.Duration) {
		return &httpx.Error{Status: http.StatusBadRequest, Code: "INVALID_SLOT", Message: "Selected time is outside working hours or not a valid slot"}
	}

	// No "is it free?" check here: two requests could both pass it and both insert.
	// The appointments_no_overlap exclusion constraint rejects the second insert
	// atomically, and httpx maps that error to 409 SLOT_TAKEN.
	var id int
	err = h.DB.QueryRow(r.Context(),
		`INSERT INTO appointments (business_id, service_id, customer_id, starts_at, ends_at, note)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		svc.BusinessID, svc.ID, auth.FromContext(r.Context()).ID, start, start.Add(svc.Duration), in.Note,
	).Scan(&id)
	if err != nil {
		return err
	}
	a, err := h.get(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, a)
	return nil
}

const appointmentSelect = `
	SELECT a.id, a.business_id, b.name, a.service_id, s.name, a.customer_id, a.starts_at, a.ends_at, a.status, a.note
	FROM appointments a
	JOIN services s ON s.id = a.service_id
	JOIN businesses b ON b.id = a.business_id`

func scanAppointment(row pgx.CollectableRow) (Appointment, error) {
	var a Appointment
	err := row.Scan(&a.ID, &a.BusinessID, &a.Business, &a.ServiceID, &a.Service, &a.CustomerID, &a.StartsAt, &a.EndsAt, &a.Status, &a.Note)
	return a, err
}

func (h *Handler) get(ctx context.Context, id int) (Appointment, error) {
	rows, _ := h.DB.Query(ctx, appointmentSelect+` WHERE a.id = $1`, id)
	return pgx.CollectExactlyOneRow(rows, scanAppointment)
}

// mine lists the caller's appointments, optionally filtered by ?status= and ?upcoming=true|false.
func (h *Handler) mine(w http.ResponseWriter, r *http.Request) error {
	limit, offset, err := httpx.Page(r)
	if err != nil {
		return err
	}
	status := r.URL.Query().Get("status")
	if status != "" && !validStatus(Status(status)) {
		return httpx.Invalid("status", "Unknown status")
	}
	upcoming := r.URL.Query().Get("upcoming")
	order := "DESC"
	if upcoming == "true" {
		order = "ASC" // next appointment first
	}
	rows, _ := h.DB.Query(r.Context(), appointmentSelect+`
		WHERE a.customer_id = $1
		  AND ($2 = '' OR a.status::text = $2)
		  AND ($3 = '' OR ($3 = 'true') = (a.starts_at >= $4))
		ORDER BY a.starts_at `+order+` LIMIT $5 OFFSET $6`,
		auth.FromContext(r.Context()).ID, status, upcoming, h.Now(), limit, offset)
	items, err := pgx.CollectRows(rows, scanAppointment)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func validStatus(s Status) bool {
	return s == Pending || s == Confirmed || s == Cancelled || s == Completed
}

// updateStatus changes an appointment's status inside a transaction. SELECT ... FOR UPDATE
// locks the row, so two changes to the same appointment can't interleave.
func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var in struct {
		Status Status `json:"status"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		return err
	}
	if !validStatus(in.Status) || in.Status == Pending {
		return httpx.Invalid("status", "status must be confirmed, cancelled or completed")
	}
	userID := auth.FromContext(r.Context()).ID

	err = pgx.BeginFunc(r.Context(), h.DB, func(tx pgx.Tx) error {
		var (
			current            Status
			customerID, owner  int
			startsAt           time.Time
			cancellationWindow int
		)
		err := tx.QueryRow(r.Context(),
			`SELECT a.status, a.customer_id, b.owner_id, a.starts_at, b.cancellation_hours
			 FROM appointments a JOIN businesses b ON b.id = a.business_id
			 WHERE a.id = $1 FOR UPDATE OF a`, id,
		).Scan(&current, &customerID, &owner, &startsAt, &cancellationWindow)
		if errors.Is(err, pgx.ErrNoRows) {
			return httpx.NotFound("Appointment")
		}
		if err != nil {
			return err
		}

		var who actor
		switch userID {
		case owner:
			who = asOwner
		case customerID:
			who = asCustomer
		default:
			return httpx.NotFound("Appointment") // don't reveal that it exists
		}

		if !canTransition(who, current, in.Status) {
			return httpx.Conflict("INVALID_TRANSITION", fmt.Sprintf("Cannot change status from %s to %s", current, in.Status))
		}
		if who == asCustomer && startsAt.Sub(h.Now()) < time.Duration(cancellationWindow)*time.Hour {
			return httpx.Forbidden(fmt.Sprintf("Appointments can be cancelled up to %d hours before", cancellationWindow))
		}
		if in.Status == Completed && startsAt.After(h.Now()) {
			return httpx.Conflict("INVALID_TRANSITION", "An appointment can't be completed before it starts")
		}
		_, err = tx.Exec(r.Context(), `UPDATE appointments SET status = $2, updated_at = now() WHERE id = $1`, id, in.Status)
		return err
	})
	if err != nil {
		return err
	}
	a, err := h.get(r.Context(), id)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, a)
	return nil
}
