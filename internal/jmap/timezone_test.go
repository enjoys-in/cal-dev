package jmap

import (
	"strings"
	"testing"

	"github.com/enjoys-in/cal-dev/internal/ics"
)

func TestTimeZoneIsValidatedBeforeAnythingHappens(t *testing.T) {
	for _, bad := range []interface{}{"Mars/Olympus_Mons", 42, true} {
		if err := validateTimeZone(map[string]interface{}{"timeZone": bad}); err == nil {
			t.Errorf("timeZone %v must be rejected", bad)
		}
	}
	for _, ok := range []interface{}{"America/New_York", "Eastern Standard Time", "UTC", "Etc/UTC", "", nil} {
		if err := validateTimeZone(map[string]interface{}{"timeZone": ok}); err != nil {
			t.Errorf("timeZone %v must be accepted: %v", ok, err)
		}
	}
	if err := validateTimeZone(map[string]interface{}{}); err != nil {
		t.Errorf("an absent timeZone is fine: %v", err)
	}
}

func TestCreateWithTimeZoneWritesTZIDAndReportsIt(t *testing.T) {
	evt := jmapToEvent(map[string]interface{}{
		"title":           "Standup",
		"start":           "2027-03-01T09:00:00-05:00",
		"end":             "2027-03-01T10:00:00-05:00",
		"recurrenceRules": []interface{}{map[string]interface{}{"rule": "FREQ=WEEKLY"}},
	}, 1, 1)
	if err := applyTimeZone(evt, map[string]interface{}{"timeZone": "America/New_York"}, true, nil); err != nil {
		t.Fatal(err)
	}
	out := ics.GenerateICS(evt)
	if !strings.Contains(out, "DTSTART;TZID=America/New_York:20270301T090000") || !strings.Contains(out, "RRULE:FREQ=WEEKLY") {
		t.Fatalf("a JMAP-created zoned series must be written with TZID:\n%s", out)
	}
	if got := eventToJMAP(evt, nil)["timeZone"]; got != "America/New_York" {
		t.Fatalf("timeZone in the JMAP object = %v", got)
	}
}

func TestRecurringEventWithoutTimeZoneUsesCalendarZone(t *testing.T) {
	recurring := func() map[string]interface{} {
		return map[string]interface{}{
			"start":           "2027-03-01T14:00:00Z",
			"recurrenceRules": []interface{}{map[string]interface{}{"rule": "FREQ=WEEKLY"}},
		}
	}
	calls := 0
	zone := func() string { calls++; return "America/New_York" }

	evt := jmapToEvent(recurring(), 1, 1)
	_ = applyTimeZone(evt, recurring(), true, zone)
	if ics.EventTZID(evt) != "America/New_York" {
		t.Fatalf("a recurring event with no timeZone should take its calendar's zone, got %q", ics.EventTZID(evt))
	}

	// not recurring: the calendar is not even consulted
	calls = 0
	single := jmapToEvent(map[string]interface{}{"start": "2027-03-01T14:00:00Z"}, 1, 1)
	_ = applyTimeZone(single, map[string]interface{}{}, true, zone)
	if ics.EventTZID(single) != "" || calls != 0 {
		t.Fatalf("a single event stays UTC without looking up the calendar (zone=%q calls=%d)", ics.EventTZID(single), calls)
	}

	// an unrelated edit of an existing series must not re-anchor it
	existing := jmapToEvent(recurring(), 1, 1)
	_ = applyTimeZone(existing, map[string]interface{}{"title": "renamed"}, false, zone)
	if ics.EventTZID(existing) != "" {
		t.Fatal("an edit that does not set the recurrence must not change the zone")
	}

	// an explicit UTC wins over the calendar's zone
	explicit := jmapToEvent(recurring(), 1, 1)
	_ = applyTimeZone(explicit, map[string]interface{}{"timeZone": "Etc/UTC"}, true, zone)
	if ics.EventTZID(explicit) != "" {
		t.Fatal("an explicit UTC timeZone must not be replaced by the calendar's zone")
	}

	// an unknown calendar zone is ignored rather than failing the request
	bad := jmapToEvent(recurring(), 1, 1)
	if err := applyTimeZone(bad, recurring(), true, func() string { return "Not/AZone" }); err != nil || ics.EventTZID(bad) != "" {
		t.Fatalf("unknown calendar zone: err=%v zone=%q", err, ics.EventTZID(bad))
	}
}

func TestTimeZoneNullClearsAndAllDayHasNone(t *testing.T) {
	evt := jmapToEvent(map[string]interface{}{"start": "2027-03-01T14:00:00Z"}, 1, 1)
	_ = applyTimeZone(evt, map[string]interface{}{"timeZone": "Asia/Kolkata"}, false, nil)
	if tz, _ := timeZoneJMAP(evt); tz != "Asia/Kolkata" {
		t.Fatalf("timeZone = %q", tz)
	}
	_ = applyTimeZone(evt, map[string]interface{}{"timeZone": nil}, false, nil)
	if tz, ok := timeZoneJMAP(evt); !ok || tz != "Etc/UTC" {
		t.Fatalf("null must clear the zone back to UTC, got %q", tz)
	}

	allDay := jmapToEvent(map[string]interface{}{"start": "2027-03-01T00:00:00Z", "isAllDay": true}, 1, 1)
	if _, ok := timeZoneJMAP(allDay); ok {
		t.Fatal("an all-day event has no timeZone")
	}
	if _, present := eventToJMAP(allDay, nil)["timeZone"]; present {
		t.Fatal("an all-day event must not report a timeZone")
	}
}
