package recurrence

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
)

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func ev(start time.Time, dur time.Duration, rule string, props string) *models.Event {
	end := start.Add(dur)
	e := &models.Event{DTStart: start, RecurrenceRule: rule, Properties: json.RawMessage("{}")}
	if dur > 0 {
		e.DTEnd = &end
	}
	if props != "" {
		e.Properties = json.RawMessage(props)
	}
	return e
}

func TestSingleEvent(t *testing.T) {
	e := ev(utc(2026, 1, 5, 9, 0), time.Hour, "", "")
	if got := Expand(e, utc(2026, 1, 5, 0, 0), utc(2026, 1, 6, 0, 0), 0); len(got) != 1 {
		t.Fatalf("want 1 occurrence, got %d", len(got))
	}
	if got := Expand(e, utc(2026, 1, 6, 0, 0), utc(2026, 1, 7, 0, 0), 0); len(got) != 0 {
		t.Fatalf("want 0 occurrences outside window, got %d", len(got))
	}
}

func TestPastStartedWeeklySeriesAppearsInCurrentMonth(t *testing.T) {
	// Started months ago, DTEND long past: the original audit bug.
	e := ev(utc(2026, 1, 5, 9, 0), time.Hour, "FREQ=WEEKLY;BYDAY=MO", "")
	got := Expand(e, utc(2026, 11, 1, 0, 0), utc(2026, 12, 1, 0, 0), 0)
	if len(got) != 5 { // Mondays in Nov 2026: 2, 9, 16, 23, 30
		t.Fatalf("want 5 Mondays in Nov 2026, got %d", len(got))
	}
	for _, o := range got {
		if o.Start.Weekday() != time.Monday || o.Start.Hour() != 9 {
			t.Fatalf("unexpected occurrence %v", o.Start)
		}
	}
}

func TestCountAndUntil(t *testing.T) {
	e := ev(utc(2026, 1, 5, 9, 0), time.Hour, "FREQ=DAILY;COUNT=3", "")
	if got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2026, 2, 1, 0, 0), 0); len(got) != 3 {
		t.Fatalf("COUNT=3: got %d", len(got))
	}
	e = ev(utc(2026, 1, 5, 9, 0), time.Hour, "FREQ=DAILY;UNTIL=20260107T090000Z", "")
	if got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2026, 2, 1, 0, 0), 0); len(got) != 3 {
		t.Fatalf("UNTIL: got %d", len(got))
	}
	if got := Expand(e, utc(2026, 3, 1, 0, 0), utc(2026, 4, 1, 0, 0), 0); len(got) != 0 {
		t.Fatalf("after UNTIL: got %d", len(got))
	}
}

func TestExdateAndRdate(t *testing.T) {
	e := ev(utc(2026, 1, 5, 9, 0), time.Hour, "FREQ=WEEKLY;COUNT=3",
		`{"exdates":["20260112T090000Z"],"rdates":["20260120T140000Z"]}`)
	got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2026, 2, 1, 0, 0), 0)
	if len(got) != 3 { // Jan 5, Jan 19, + RDATE Jan 20 (Jan 12 excluded)
		t.Fatalf("want 3, got %d: %+v", len(got), got)
	}
	for _, o := range got {
		if o.Start.Equal(utc(2026, 1, 12, 9, 0)) {
			t.Fatal("EXDATE occurrence was not excluded")
		}
	}
}

func TestTimezoneDSTKeepsWallClock(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata not available")
	}
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, loc) // Monday before US DST starts Mar 8
	e := ev(start, time.Hour, "FREQ=WEEKLY;BYDAY=MO", `{"tzid":"America/New_York"}`)
	got := Expand(e, utc(2026, 3, 1, 0, 0), utc(2026, 3, 31, 0, 0), 0)
	if len(got) < 3 {
		t.Fatalf("want >=3 occurrences, got %d", len(got))
	}
	for _, o := range got {
		if h := o.Start.In(loc).Hour(); h != 9 {
			t.Fatalf("wall-clock drifted across DST: %v is %d:00 local", o.Start, h)
		}
	}
}

func TestAllDayDefaultsToOneDay(t *testing.T) {
	e := &models.Event{DTStart: utc(2026, 1, 5, 0, 0), AllDay: true, Properties: json.RawMessage("{}")}
	if d := Duration(e); d != 24*time.Hour {
		t.Fatalf("all-day duration = %v", d)
	}
}

func TestZeroLengthTimedEvent(t *testing.T) {
	e := ev(utc(2026, 1, 5, 9, 0), 0, "", "")
	if Duration(e) != 0 {
		t.Fatal("timed event with no end should have zero duration")
	}
	if !HasOccurrenceIn(e, utc(2026, 1, 5, 0, 0), utc(2026, 1, 6, 0, 0)) {
		t.Fatal("zero-length event inside the window should still occur")
	}
}

func TestSubHourlyRecurrenceIsNotExpanded(t *testing.T) {
	e := ev(utc(2020, 1, 1, 0, 0), time.Minute, "FREQ=MINUTELY", "")
	got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2026, 1, 2, 0, 0), 0)
	if len(got) > 1 {
		t.Fatalf("MINUTELY must not expand, got %d", len(got))
	}
}

func TestLimit(t *testing.T) {
	e := ev(utc(2026, 1, 1, 0, 0), time.Hour, "FREQ=DAILY", "")
	if got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2030, 1, 1, 0, 0), 10); len(got) != 10 {
		t.Fatalf("limit not applied: %d", len(got))
	}
}
