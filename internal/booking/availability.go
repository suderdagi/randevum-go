package booking

import "time"

// SlotStep is the distance between possible start times.
const SlotStep = 15 * time.Minute

// Hours are the opening hours of one day, as "HH:MM" local times.
type Hours struct {
	OpensAt  string
	ClosesAt string
}

// Range is a booked period.
type Range struct{ Start, End time.Time }

func (a Range) overlaps(b Range) bool { return a.Start.Before(b.End) && b.Start.Before(a.End) }

// openInterval returns the working hours of a calendar day as absolute instants.
// time.Date resolves the local clock time in the business's location, so 09:00
// stays 09:00 local on days when daylight saving time starts or ends.
func openInterval(day time.Time, loc *time.Location, h Hours) Range {
	at := func(clock string) time.Time {
		t, _ := time.Parse("15:04", clock)
		return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	}
	return Range{at(h.OpensAt), at(h.ClosesAt)}
}

// Slots lists the free start times for a service on a day.
//
// Candidates are generated every 15 minutes inside working hours. One is kept if the
// whole appointment fits before closing time, doesn't overlap a booking and isn't in
// the past. hours == nil means the business is closed that day.
func Slots(day time.Time, loc *time.Location, hours *Hours, duration time.Duration, busy []Range, now time.Time) []time.Time {
	slots := []time.Time{}
	if hours == nil {
		return slots
	}
	open := openInterval(day, loc, *hours)
	for start := open.Start; ; start = start.Add(SlotStep) {
		candidate := Range{start, start.Add(duration)}
		if candidate.End.After(open.End) {
			break
		}
		if !start.After(now) || overlapsAny(candidate, busy) {
			continue
		}
		slots = append(slots, start)
	}
	return slots
}

// IsValidStart reports whether start is one of the slot starts of its day and the
// appointment ends before closing time. It doesn't check for other bookings: the
// database constraint does that, without a race.
func IsValidStart(start time.Time, loc *time.Location, hours *Hours, duration time.Duration) bool {
	if hours == nil {
		return false
	}
	local := start.In(loc)
	open := openInterval(local, loc, *hours)
	end := start.Add(duration)
	return !start.Before(open.Start) && !end.After(open.End) && start.Sub(open.Start)%SlotStep == 0
}

func overlapsAny(r Range, busy []Range) bool {
	for _, b := range busy {
		if r.overlaps(b) {
			return true
		}
	}
	return false
}

// isoWeekday converts Go's weekday (Sunday = 0) to ISO 8601 (Monday = 1 ... Sunday = 7).
func isoWeekday(t time.Time) int {
	if t.Weekday() == time.Sunday {
		return 7
	}
	return int(t.Weekday())
}
