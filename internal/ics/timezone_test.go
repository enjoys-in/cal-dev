package ics

import (
	"strings"
	"testing"
)

// A weekly 09:00 New York series as a CalDAV client sends it: TZID on DTSTART/DTEND/EXDATE/RECURRENCE-ID.
const zonedSeries = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
	"BEGIN:VEVENT\r\nUID:ny-1\r\nDTSTART;TZID=America/New_York:20270301T090000\r\nDTEND;TZID=America/New_York:20270301T100000\r\n" +
	"RRULE:FREQ=WEEKLY\r\nEXDATE;TZID=America/New_York:20270308T090000\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:ny-1\r\nRECURRENCE-ID;TZID=America/New_York:20270322T090000\r\n" +
	"DTSTART;TZID=America/New_York:20270322T110000\r\nDTEND;TZID=America/New_York:20270322T120000\r\nSUMMARY:Standup (moved)\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestRegeneratedSeriesKeepsTZID(t *testing.T) {
	rows, err := ParseICSAll(zonedSeries, 1, 1, "cal")
	if err != nil || len(rows) != 2 {
		t.Fatalf("parse: %v, %d rows", err, len(rows))
	}
	final := FinalizeResource(rows) // what an RSVP, a local invitation or a JMAP edit triggers
	out := final[0].RawICS

	for _, want := range []string{
		"DTSTART;TZID=America/New_York:20270301T090000",
		"DTEND;TZID=America/New_York:20270301T100000",
		"EXDATE;TZID=America/New_York:20270308T090000",
		"RECURRENCE-ID;TZID=America/New_York:20270322T090000",
		"DTSTART;TZID=America/New_York:20270322T110000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("regenerated ICS is missing %q:\n%s", want, out)
		}
	}
	// 09:00 New York must never be written as a fixed UTC time: that is what drifts at the DST change.
	if strings.Contains(out, "DTSTART:2027") {
		t.Errorf("zoned series was written with a UTC DTSTART:\n%s", out)
	}

	back, err := ParseICSAll(out, 1, 1, "cal")
	if err != nil || len(back) != 2 {
		t.Fatalf("round trip: %v, %d rows", err, len(back))
	}
	for i := range rows {
		if !back[i].DTStart.Equal(rows[i].DTStart) || back[i].RecurrenceID != rows[i].RecurrenceID {
			t.Errorf("row %d changed in the round trip: %v/%q -> %v/%q", i,
				rows[i].DTStart, rows[i].RecurrenceID, back[i].DTStart, back[i].RecurrenceID)
		}
		if EventTZID(back[i]) != "America/New_York" {
			t.Errorf("row %d lost its zone: %s", i, back[i].Properties)
		}
	}
	if string(back[0].Properties) != string(rows[0].Properties) {
		t.Errorf("master properties changed: %s -> %s", rows[0].Properties, back[0].Properties)
	}
}

func TestUTCEventIsStillWrittenAsUTC(t *testing.T) {
	rows, _ := ParseICSAll("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTART:20270301T140000Z\r\nDTEND:20270301T150000Z\r\nRRULE:FREQ=WEEKLY\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", 1, 1, "c")
	out := FinalizeResource(rows)[0].RawICS
	if !strings.Contains(out, "DTSTART:20270301T140000Z") || strings.Contains(out, "TZID") {
		t.Fatalf("a UTC event must not gain a TZID:\n%s", out)
	}
}

func TestAllDayEventsHaveNoZone(t *testing.T) {
	rows, _ := ParseICSAll("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:d\r\nDTSTART;VALUE=DATE:20270301\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", 1, 1, "c")
	if err := SetEventTZID(rows[0], "America/New_York"); err != nil {
		t.Fatal(err)
	}
	out := FinalizeResource(rows)[0].RawICS
	if !strings.Contains(out, "DTSTART;VALUE=DATE:20270301") || strings.Contains(out, "TZID") {
		t.Fatalf("an all-day event is a date, not a zoned time:\n%s", out)
	}
	if EventTZID(rows[0]) != "" {
		t.Fatal("EventTZID must be empty for an all-day event")
	}
}

func TestSetEventTZID(t *testing.T) {
	rows, _ := ParseICSAll("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:20270301T140000Z\r\n"+
		"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT5M\r\nEND:VALARM\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", 1, 1, "c")
	e := rows[0]

	if err := SetEventTZID(e, "Not/AZone"); err == nil {
		t.Fatal("an unknown zone must be rejected")
	}
	if err := SetEventTZID(e, "Eastern Standard Time"); err != nil {
		t.Fatal(err)
	}
	if got := EventTZID(e); got != "America/New_York" {
		t.Fatalf("a Windows zone name must normalize to IANA, got %q", got)
	}
	if !strings.Contains(string(e.Properties), `"alarms"`) {
		t.Fatalf("setting the zone must keep other properties: %s", e.Properties)
	}
	for _, utc := range []string{"UTC", "Etc/UTC", "", "Z"} {
		_ = SetEventTZID(e, "America/New_York")
		if err := SetEventTZID(e, utc); err != nil || EventTZID(e) != "" {
			t.Fatalf("%q must clear the zone, got %q (%v)", utc, EventTZID(e), err)
		}
	}
}

// Changing a series' zone must not move its exceptions: a local EXDATE only means something
// relative to the zone it was written in.
func TestChangingZoneKeepsExdateInstants(t *testing.T) {
	rows, _ := ParseICSAll(zonedSeries, 1, 1, "cal")
	master := rows[0]
	if err := SetEventTZID(master, "Asia/Kolkata"); err != nil {
		t.Fatal(err)
	}
	// 09:00 New York on 2027-03-08 (still EST) is 14:00Z.
	if !strings.Contains(string(master.Properties), "20270308T140000Z") {
		t.Fatalf("EXDATE must be pinned to its original instant: %s", master.Properties)
	}
	out := GenerateResourceICS(rows)
	if !strings.Contains(out, "EXDATE:20270308T140000Z") {
		t.Fatalf("pinned EXDATE should be written as UTC:\n%s", out)
	}
}

func TestSchedulingICSCarriesZone(t *testing.T) {
	rows, _ := ParseICSAll(zonedSeries, 1, 1, "cal")
	out := GenerateSchedulingICS(rows[0], "REQUEST")
	if !strings.Contains(out, "DTSTART;TZID=America/New_York:20270301T090000") ||
		!strings.Contains(out, "DTEND;TZID=America/New_York:20270301T100000") {
		t.Fatalf("iMIP invitation must keep the organizer's zone:\n%s", out)
	}
}
