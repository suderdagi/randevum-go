package booking

import (
	"testing"
	"time"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func clockTimes(slots []time.Time, loc *time.Location) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.In(loc).Format("15:04")
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSlots(t *testing.T) {
	ist := mustLoad(t, "Europe/Istanbul")
	day := time.Date(2026, 12, 1, 0, 0, 0, 0, ist)
	hours := &Hours{OpensAt: "09:00", ClosesAt: "10:00"}
	longAgo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(clock string) time.Time {
		c, _ := time.Parse("15:04", clock)
		return time.Date(2026, 12, 1, c.Hour(), c.Minute(), 0, 0, ist)
	}

	tests := []struct {
		name     string
		hours    *Hours
		duration time.Duration
		busy     []Range
		now      time.Time
		want     []string
	}{
		{"every 15 minutes while the service fits", hours, 30 * time.Minute, nil, longAgo, []string{"09:00", "09:15", "09:30"}},
		{"closed day", nil, 30 * time.Minute, nil, longAgo, []string{}},
		{"service longer than the day", hours, 90 * time.Minute, nil, longAgo, []string{}},
		{"skips overlapping bookings", hours, 30 * time.Minute, []Range{{at("09:15"), at("09:45")}}, longAgo, []string{}},
		{"a booking ending at the start is not an overlap", hours, 15 * time.Minute, []Range{{at("09:00"), at("09:30")}}, longAgo, []string{"09:30", "09:45"}},
		{"skips times in the past", hours, 15 * time.Minute, nil, at("09:20"), []string{"09:30", "09:45"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clockTimes(Slots(day, ist, tt.hours, tt.duration, tt.busy, tt.now), ist)
			if !equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// On 29 March 2026 clocks in Berlin jump from 02:00 to 03:00. Opening hours are local
// times, so the first slot must still be 09:00 local, which is 07:00 UTC instead of 08:00.
func TestSlotsAcrossDaylightSavingChange(t *testing.T) {
	berlin := mustLoad(t, "Europe/Berlin")
	day := time.Date(2026, 3, 29, 0, 0, 0, 0, berlin)
	slots := Slots(day, berlin, &Hours{"09:00", "09:30"}, 30*time.Minute, nil, time.Time{})

	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].UTC().Format("15:04"); got != "07:00" {
		t.Errorf("first slot is %s UTC, want 07:00", got)
	}
}

func TestIsValidStart(t *testing.T) {
	ist := mustLoad(t, "Europe/Istanbul")
	hours := &Hours{"09:00", "18:00"}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	tests := []struct {
		start string
		want  bool
	}{
		{"2026-12-01T09:00:00+03:00", true},
		{"2026-12-01T06:00:00Z", true}, // same instant, written in UTC
		{"2026-12-01T09:10:00+03:00", false},
		{"2026-12-01T08:45:00+03:00", false},
		{"2026-12-01T17:45:00+03:00", false}, // a 30 minute service would end after closing
		{"2026-12-01T17:30:00+03:00", true},
	}
	for _, tt := range tests {
		if got := IsValidStart(at(tt.start), ist, hours, 30*time.Minute); got != tt.want {
			t.Errorf("IsValidStart(%s) = %v, want %v", tt.start, got, tt.want)
		}
	}
	if IsValidStart(at("2026-12-01T09:00:00+03:00"), ist, nil, 30*time.Minute) {
		t.Error("a closed day must have no valid start")
	}
}

func TestCanTransition(t *testing.T) {
	tests := []struct {
		who      actor
		from, to Status
		want     bool
	}{
		{asOwner, Pending, Confirmed, true},
		{asOwner, Confirmed, Completed, true},
		{asOwner, Pending, Completed, false},
		{asOwner, Cancelled, Confirmed, false},
		{asCustomer, Pending, Cancelled, true},
		{asCustomer, Pending, Confirmed, false},
		{asCustomer, Completed, Cancelled, false},
	}
	for _, tt := range tests {
		if got := canTransition(tt.who, tt.from, tt.to); got != tt.want {
			t.Errorf("%s: %s -> %s = %v, want %v", tt.who, tt.from, tt.to, got, tt.want)
		}
	}
}

func TestISOWeekday(t *testing.T) {
	if got := isoWeekday(time.Date(2026, 11, 29, 12, 0, 0, 0, time.UTC)); got != 7 { // Sunday
		t.Errorf("Sunday = %d, want 7", got)
	}
	if got := isoWeekday(time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC)); got != 1 { // Monday
		t.Errorf("Monday = %d, want 1", got)
	}
}
