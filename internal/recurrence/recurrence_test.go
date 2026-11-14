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

// ---- series with RECURRENCE-ID overrides ----

func override(master *models.Event, recurrenceID string, start time.Time, dur time.Duration, mods ...func(*models.Event)) *models.Event {
	o := &models.Event{
		CalendarID: master.CalendarID, UID: master.UID, RecurrenceID: recurrenceID,
		DTStart: start, Status: "CONFIRMED", Properties: json.RawMessage("{}"),
	}
	if dur > 0 {
		end := start.Add(dur)
		o.DTEnd = &end
	}
	for _, m := range mods {
		m(o)
	}
	return o
}

func weeklyMaster() *models.Event {
	m := ev(utc(2026, 11, 2, 9, 0), time.Hour, "FREQ=WEEKLY", "") // Mondays 09:00: Nov 2, 9, 16, 23, 30
	m.CalendarID, m.UID = "cal", "s1"
	return m
}

var novFrom, novTo = utc(2026, 11, 1, 0, 0), utc(2026, 12, 1, 0, 0)

func startsOf(occ []Occurrence) []time.Time {
	var s []time.Time
	for _, o := range occ {
		s = append(s, o.Start)
	}
	return s
}

func TestOverrideReplacesOccurrence(t *testing.T) {
	m := weeklyMaster()
	o := override(m, "20261116T090000Z", utc(2026, 11, 16, 14, 0), time.Hour, func(e *models.Event) { e.Summary = "moved" })
	occ := Series{Master: m, Overrides: []*models.Event{o}}.Expand(novFrom, novTo, 0)

	if len(occ) != 5 {
		t.Fatalf("still five occurrences in November, got %d: %v", len(occ), startsOf(occ))
	}
	for _, x := range occ {
		if x.Start.Equal(utc(2026, 11, 16, 9, 0)) {
			t.Fatal("the original 09:00 occurrence must be replaced, not duplicated")
		}
	}
	moved := occ[2] // Nov 2, 9, 16(14:00), 23, 30
	if !moved.Start.Equal(utc(2026, 11, 16, 14, 0)) || moved.Event != o {
		t.Fatalf("the override must define the third occurrence, got %v from %v", moved.Start, moved.Event)
	}
}

func TestCancelledOverrideRemovesOccurrence(t *testing.T) {
	m := weeklyMaster()
	o := override(m, "20261116T090000Z", utc(2026, 11, 16, 9, 0), time.Hour, func(e *models.Event) { e.Status = "CANCELLED" })
	occ := Series{Master: m, Overrides: []*models.Event{o}}.Expand(novFrom, novTo, 0)
	if len(occ) != 4 {
		t.Fatalf("want 4 after cancelling one, got %d", len(occ))
	}
}

func TestOverrideMovedIntoWindowFromOutside(t *testing.T) {
	m := weeklyMaster()
	// the Dec 7 occurrence is moved back into November (Nov 28, a Saturday)
	o := override(m, "20261207T090000Z", utc(2026, 11, 28, 10, 0), time.Hour)
	occ := Series{Master: m, Overrides: []*models.Event{o}}.Expand(novFrom, novTo, 0)
	if len(occ) != 6 {
		t.Fatalf("want 5 regular + the moved-in one, got %d: %v", len(occ), startsOf(occ))
	}
	found := false
	for _, x := range occ {
		if x.Start.Equal(utc(2026, 11, 28, 10, 0)) && x.Event == o {
			found = true
		}
	}
	if !found {
		t.Fatalf("moved-in occurrence missing: %v", startsOf(occ))
	}
}

func TestOverrideMovedOutOfWindowDisappears(t *testing.T) {
	m := weeklyMaster()
	// Nov 16 moved to Dec 10: gone from November, present in December
	o := override(m, "20261116T090000Z", utc(2026, 12, 10, 9, 0), time.Hour)
	s := Series{Master: m, Overrides: []*models.Event{o}}
	if occ := s.Expand(novFrom, novTo, 0); len(occ) != 4 {
		t.Fatalf("November must lose the moved occurrence, got %d", len(occ))
	}
	dec := s.Expand(utc(2026, 12, 9, 0, 0), utc(2026, 12, 12, 0, 0), 0)
	if len(dec) != 1 || !dec[0].Start.Equal(utc(2026, 12, 10, 9, 0)) {
		t.Fatalf("December window must contain only the moved occurrence, got %v", startsOf(dec))
	}
}

func TestOverrideWithoutEndInheritsSeriesLength(t *testing.T) {
	m := weeklyMaster() // 1h
	o := override(m, "20261116T090000Z", utc(2026, 11, 16, 14, 0), 0)
	occ := Series{Master: m, Overrides: []*models.Event{o}}.Expand(novFrom, novTo, 0)
	if got := occ[2].End.Sub(occ[2].Start); got != time.Hour {
		t.Fatalf("override without DTEND must keep the master's 1h length, got %v", got)
	}
}

func TestAllDaySeriesOverrideKeyIsDate(t *testing.T) {
	m := &models.Event{CalendarID: "c", UID: "d", DTStart: utc(2026, 11, 2, 0, 0), AllDay: true, RecurrenceRule: "FREQ=WEEKLY;COUNT=3", Properties: json.RawMessage("{}")}
	o := &models.Event{CalendarID: "c", UID: "d", RecurrenceID: "20261109", DTStart: utc(2026, 11, 10, 0, 0), AllDay: true, Status: "CONFIRMED", Properties: json.RawMessage("{}")}
	occ := Series{Master: m, Overrides: []*models.Event{o}}.Expand(novFrom, novTo, 0)
	if len(occ) != 3 {
		t.Fatalf("want 3, got %d", len(occ))
	}
	for _, x := range occ {
		if x.Start.Equal(utc(2026, 11, 9, 0, 0)) {
			t.Fatal("the overridden all-day occurrence (Nov 9) must be replaced")
		}
	}
}

func TestZonedSeriesMatchesUTCRecurrenceIDAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata not available")
	}
	// 09:00 New York every Monday; DST starts Mar 8 2026, so Mar 9 09:00 EDT = 13:00Z (Mar 2 was 14:00Z)
	m := ev(time.Date(2026, 3, 2, 9, 0, 0, 0, loc), time.Hour, "FREQ=WEEKLY;BYDAY=MO", `{"tzid":"America/New_York"}`)
	m.CalendarID, m.UID = "c", "ny"
	o := override(m, "20260309T130000Z", time.Date(2026, 3, 9, 15, 0, 0, 0, loc), time.Hour)
	occ := Series{Master: m, Overrides: []*models.Event{o}}.Expand(utc(2026, 3, 1, 0, 0), utc(2026, 3, 20, 0, 0), 0)
	for _, x := range occ {
		if x.Start.In(loc).Format("2006-01-02 15:04") == "2026-03-09 09:00" {
			t.Fatal("override keyed in UTC must still replace the DST-shifted local occurrence")
		}
	}
	if len(occ) != 3 { // Mar 2, Mar 9 (moved to 15:00), Mar 16
		t.Fatalf("want 3 occurrences, got %d: %v", len(occ), startsOf(occ))
	}
}

func TestGroupBuildsSeriesAndKeepsOrphansStandalone(t *testing.T) {
	m := weeklyMaster()
	o := override(m, "20261116T090000Z", utc(2026, 11, 16, 14, 0), time.Hour)
	orphan := &models.Event{CalendarID: "cal", UID: "only-override", RecurrenceID: "20261120T090000Z", DTStart: utc(2026, 11, 20, 9, 0), Properties: json.RawMessage("{}")}
	other := &models.Event{CalendarID: "cal2", UID: "s1", DTStart: utc(2026, 11, 5, 9, 0), Properties: json.RawMessage("{}")} // same uid, different calendar

	got := Group([]*models.Event{o, m, orphan, other})
	if len(got) != 3 {
		t.Fatalf("want 3 series (s1 with override, orphan, s1 in another calendar), got %d", len(got))
	}
	if got[0].Master != m || len(got[0].Overrides) != 1 {
		t.Fatal("override must attach to its master even when listed before it")
	}
	if got[1].Master != orphan || len(got[1].Overrides) != 0 {
		t.Fatal("an override without a master stands alone")
	}
	if got[2].Master != other {
		t.Fatal("the same UID in another calendar is a different series")
	}
}
