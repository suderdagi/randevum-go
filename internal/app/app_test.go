package app_test

// Integration tests: the real router and middleware against a real PostgreSQL database.
// They run when TEST_DATABASE_URL is set (CI starts a PostgreSQL service for them).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/suderdagi/randevum-go/internal/app"
	"github.com/suderdagi/randevum-go/internal/config"
	"github.com/suderdagi/randevum-go/internal/db"
)

// The tests run on a fixed "now" so slot times are predictable: Monday 30 Nov 2026, 08:00 in Istanbul.
var now = time.Date(2026, 11, 30, 8, 0, 0, 0, time.FixedZone("+03", 3*3600))

type env struct {
	t      *testing.T
	server *httptest.Server
	pool   *pgxpool.Pool
}

func setup(t *testing.T) *env {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// Start every test from an empty schema
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{JWTSecret: []byte("test-secret-that-is-long-enough-123456"), AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour}
	server := httptest.NewServer(app.New(cfg, pool, func() time.Time { return now }))
	t.Cleanup(func() {
		server.Close()
		pool.Close()
	})
	return &env{t, server, pool}
}

type response struct {
	Status int
	Body   map[string]any
}

func (e *env) do(method, path, token string, body any) response {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.server.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := response{Status: res.StatusCode, Body: map[string]any{}}
	_ = json.NewDecoder(res.Body).Decode(&out.Body)
	return out
}

func (e *env) expect(r response, status int) response {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("got status %d, want %d: %v", r.Status, status, r.Body)
	}
	return r
}

func errorCode(r response) string {
	if m, ok := r.Body["error"].(map[string]any); ok {
		code, _ := m["code"].(string)
		return code
	}
	return ""
}

// register creates a user and returns its access and refresh tokens.
func (e *env) register(email, role string) (access, refresh string) {
	r := e.expect(e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"email": email, "password": "password123", "fullName": "Test " + role, "role": role,
	}), 201)
	tokens := r.Body["tokens"].(map[string]any)
	return tokens["accessToken"].(string), tokens["refreshToken"].(string)
}

// barber sets up an owner with a business open 09:00–18:00 every day and a 30 minute haircut.
func (e *env) barber() (ownerToken string, businessID, serviceID int) {
	ownerToken, _ = e.register("owner@example.com", "owner")
	b := e.expect(e.do("POST", "/api/v1/businesses", ownerToken, map[string]any{
		"name": "Kuaför Ayşe", "category": "Barber", "city": "Elazığ",
	}), 201)
	businessID = int(b.Body["id"].(float64))

	var days []map[string]any
	for d := 1; d <= 7; d++ {
		days = append(days, map[string]any{"weekday": d, "opensAt": "09:00", "closesAt": "18:00"})
	}
	e.expect(e.do("PUT", fmt.Sprintf("/api/v1/businesses/%d/working-hours", businessID), ownerToken, map[string]any{"days": days}), 200)

	s := e.expect(e.do("POST", fmt.Sprintf("/api/v1/businesses/%d/services", businessID), ownerToken, map[string]any{
		"name": "Haircut", "durationMinutes": 30, "price": 350,
	}), 201)
	return ownerToken, businessID, int(s.Body["id"].(float64))
}

func TestAuthFlow(t *testing.T) {
	e := setup(t)
	access, refresh := e.register("Sude@Example.com", "customer")

	me := e.expect(e.do("GET", "/api/v1/auth/me", access, nil), 200)
	if email := me.Body["user"].(map[string]any)["email"]; email != "sude@example.com" {
		t.Errorf("email should be stored lower case, got %v", email)
	}

	e.expect(e.do("POST", "/api/v1/auth/register", "", map[string]any{
		"email": "sude@example.com", "password": "password123", "fullName": "Again",
	}), 409)
	e.expect(e.do("POST", "/api/v1/auth/login", "", map[string]any{"email": "sude@example.com", "password": "wrong-password"}), 401)
	e.expect(e.do("GET", "/api/v1/auth/me", "not-a-token", nil), 401)

	// Rotation: the refresh token works once and returns a new one
	r := e.expect(e.do("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh}), 200)
	newRefresh := r.Body["tokens"].(map[string]any)["refreshToken"].(string)

	// Reusing the old token revokes every session, including the new token
	e.expect(e.do("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": refresh}), 401)
	e.expect(e.do("POST", "/api/v1/auth/refresh", "", map[string]any{"refreshToken": newRefresh}), 401)
}

func TestRolesAndOwnership(t *testing.T) {
	e := setup(t)
	_, businessID, _ := e.barber()
	customer, _ := e.register("customer@example.com", "customer")
	otherOwner, _ := e.register("other@example.com", "owner")

	e.expect(e.do("POST", "/api/v1/businesses", customer, map[string]any{"name": "X", "category": "y", "city": "z"}), 403)
	// Another owner can't change this business, and gets 404 rather than 403
	e.expect(e.do("POST", fmt.Sprintf("/api/v1/businesses/%d/services", businessID), otherOwner, map[string]any{
		"name": "Shave", "durationMinutes": 20, "price": 100,
	}), 404)
}

func TestSearchBusinesses(t *testing.T) {
	e := setup(t)
	e.barber()

	r := e.expect(e.do("GET", "/api/v1/businesses?city=elazığ&category=barber&q=ayş", "", nil), 200)
	if r.Body["total"].(float64) != 1 {
		t.Errorf("want 1 result, got %v", r.Body)
	}
	r = e.expect(e.do("GET", "/api/v1/businesses?q=%25", "", nil), 200) // a literal %, not a wildcard
	if r.Body["total"].(float64) != 0 {
		t.Errorf("%% must not match everything, got %v", r.Body["total"])
	}
}

func TestBookingFlow(t *testing.T) {
	e := setup(t)
	owner, businessID, serviceID := e.barber()
	customer, _ := e.register("customer@example.com", "customer")
	availability := fmt.Sprintf("/api/v1/businesses/%d/availability?serviceId=%d&date=2026-12-01", businessID, serviceID)

	slots := e.expect(e.do("GET", availability, "", nil), 200).Body["slots"].([]any)
	if len(slots) != 35 || slots[0] != "2026-12-01T09:00:00+03:00" { // 09:00 ... 17:30
		t.Fatalf("unexpected slots: %d, first %v", len(slots), slots[0])
	}

	booked := e.expect(e.do("POST", "/api/v1/appointments", customer, map[string]any{
		"serviceId": serviceID, "startsAt": "2026-12-01T10:00:00+03:00",
	}), 201)
	id := int(booked.Body["id"].(float64))

	// The booked time and the slots that would overlap it disappear
	after := e.expect(e.do("GET", availability, "", nil), 200).Body["slots"].([]any)
	for _, s := range after {
		if s == "2026-12-01T09:45:00+03:00" || s == "2026-12-01T10:00:00+03:00" || s == "2026-12-01T10:15:00+03:00" {
			t.Errorf("slot %v should be taken", s)
		}
	}

	// Invalid requests
	if r := e.do("POST", "/api/v1/appointments", customer, map[string]any{"serviceId": serviceID, "startsAt": "2026-12-01T10:05:00+03:00"}); errorCode(r) != "INVALID_SLOT" {
		t.Errorf("off-grid time: got %v", r.Body)
	}
	e.expect(e.do("POST", "/api/v1/appointments", customer, map[string]any{"serviceId": serviceID, "startsAt": "2026-11-29T10:00:00+03:00"}), 400)
	e.expect(e.do("POST", "/api/v1/appointments", owner, map[string]any{"serviceId": serviceID, "startsAt": "2026-12-01T11:00:00+03:00"}), 403)

	// Status changes follow the transition table
	path := fmt.Sprintf("/api/v1/appointments/%d/status", id)
	e.expect(e.do("PATCH", path, customer, map[string]any{"status": "confirmed"}), 409)
	e.expect(e.do("PATCH", path, owner, map[string]any{"status": "confirmed"}), 200)
	e.expect(e.do("PATCH", path, owner, map[string]any{"status": "completed"}), 409) // hasn't started yet

	stranger, _ := e.register("stranger@example.com", "customer")
	e.expect(e.do("PATCH", path, stranger, map[string]any{"status": "cancelled"}), 404)

	e.expect(e.do("PATCH", path, customer, map[string]any{"status": "cancelled"}), 200)
	// A cancelled slot can be booked again
	e.expect(e.do("POST", "/api/v1/appointments", stranger, map[string]any{"serviceId": serviceID, "startsAt": "2026-12-01T10:00:00+03:00"}), 201)

	mine := e.expect(e.do("GET", "/api/v1/appointments/me?status=cancelled", customer, nil), 200)
	if n := len(mine.Body["items"].([]any)); n != 1 {
		t.Errorf("want 1 cancelled appointment, got %d", n)
	}
}

func TestCustomerCannotCancelInsideTheDeadline(t *testing.T) {
	e := setup(t)
	_, _, serviceID := e.barber() // cancellation deadline: 2 hours
	customer, _ := e.register("customer@example.com", "customer")

	// now is 08:00, the appointment is at 09:00: only one hour left
	r := e.expect(e.do("POST", "/api/v1/appointments", customer, map[string]any{"serviceId": serviceID, "startsAt": "2026-11-30T09:00:00+03:00"}), 201)
	e.expect(e.do("PATCH", fmt.Sprintf("/api/v1/appointments/%d/status", int(r.Body["id"].(float64))), customer, map[string]any{"status": "cancelled"}), 403)
}

// The main guarantee: when many customers try to book the same slot at the same
// moment, exactly one succeeds and the rest get 409 SLOT_TAKEN.
func TestConcurrentBookingsOfTheSameSlot(t *testing.T) {
	e := setup(t)
	_, _, serviceID := e.barber()

	const n = 8
	tokens := make([]string, n)
	for i := range tokens {
		tokens[i], _ = e.register(fmt.Sprintf("c%d@example.com", i), "customer")
	}

	results := make([]response, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = e.do("POST", "/api/v1/appointments", tokens[i], map[string]any{
				"serviceId": serviceID, "startsAt": "2026-12-01T14:00:00+03:00",
			})
		}()
	}
	close(start)
	wg.Wait()

	created, taken := 0, 0
	for _, r := range results {
		switch {
		case r.Status == 201:
			created++
		case r.Status == 409 && errorCode(r) == "SLOT_TAKEN":
			taken++
		default:
			t.Errorf("unexpected response %d %v", r.Status, r.Body)
		}
	}
	if created != 1 || taken != n-1 {
		t.Errorf("created %d, taken %d; want 1 and %d", created, taken, n-1)
	}

	// Overlapping (not identical) times are rejected too
	other, _ := e.register("late@example.com", "customer")
	r := e.do("POST", "/api/v1/appointments", other, map[string]any{"serviceId": serviceID, "startsAt": "2026-12-01T14:15:00+03:00"})
	if errorCode(r) != "SLOT_TAKEN" {
		t.Errorf("overlapping booking: got %d %v", r.Status, r.Body)
	}
}

func TestErrorsAreJSON(t *testing.T) {
	e := setup(t)
	r := e.expect(e.do("GET", "/api/v1/nope", "", nil), 404)
	if errorCode(r) != "NOT_FOUND" {
		t.Errorf("got %v", r.Body)
	}
	r = e.expect(e.do("POST", "/api/v1/auth/register", "", map[string]any{"email": "a@b.co", "password": "x", "fullName": "A", "extra": 1}), 400)
	if errorCode(r) != "BAD_REQUEST" {
		t.Errorf("unknown fields must be rejected, got %v", r.Body)
	}
}
