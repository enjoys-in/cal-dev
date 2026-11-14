package ics

import (
	"strings"
	"testing"
)

const seriesWithOverrides = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:Asia/Kolkata\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:+0530\r\nTZOFFSETTO:+0530\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
	// override listed BEFORE the master on purpose: order in the file must not matter
	"BEGIN:VEVENT\r\nUID:series-1\r\nRECURRENCE-ID:20261116T090000Z\r\nDTSTART:20261116T140000Z\r\nDTEND:20261116T150000Z\r\nSUMMARY:Standup (moved)\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series-1\r\nDTSTART:20261109T090000Z\r\nDTEND:20261109T100000Z\r\nRRULE:FREQ=WEEKLY\r\nSUMMARY:Standup\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT10M\r\nDESCRIPTION:soon\r\nEND:VALARM\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series-1\r\nRECURRENCE-ID:20261123T090000Z\r\nDTSTART:20261123T090000Z\r\nDTEND:20261123T100000Z\r\nSTATUS:CANCELLED\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestParseICSAllSplitsMasterAndOverrides(t *testing.T) {
	events, err := ParseICSAll(seriesWithOverrides, 1, 1, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 rows (master + 2 overrides), got %d", len(events))
	}
	master := events[0]
	if master.RecurrenceID != "" || master.RecurrenceRule != "FREQ=WEEKLY" || master.Summary != "Standup" {
		t.Fatalf("master must come first and keep its own fields, got %+v", master)
	}
	// The override listed first in the file must NOT have leaked its fields into the master (the old flattening bug).
	if master.DTStart.Hour() != 9 {
		t.Fatalf("master DTSTART was overwritten by an override: %v", master.DTStart)
	}
	if events[1].RecurrenceID != "20261116T090000Z" || events[1].Summary != "Standup (moved)" || events[1].DTStart.Hour() != 14 {
		t.Fatalf("moved override wrong: %+v", events[1])
	}
	if events[2].RecurrenceID != "20261123T090000Z" || events[2].Status != "CANCELLED" {
		t.Fatalf("cancelled override wrong: %+v", events[2])
	}
	for _, e := range events {
		if e.UID != "series-1" {
			t.Fatalf("uid = %q", e.UID)
		}
	}
}

func TestResourceStorageInvariant(t *testing.T) {
	events, _ := ParseICSAll(seriesWithOverrides, 1, 1, "cal")
	etag, size := events[0].ETag, events[0].SizeBytes
	for _, e := range events {
		if e.ETag != etag || e.SizeBytes != size {
			t.Fatal("every row of a resource must share the resource ETag and size")
		}
	}
	if events[0].RawICS != seriesWithOverrides {
		t.Fatal("the primary row must hold the complete resource exactly as received (incl. VTIMEZONE)")
	}
	for _, e := range events[1:] {
		if strings.Count(e.RawICS, "BEGIN:VEVENT") != 1 {
			t.Fatalf("non-primary rows hold only their own component:\n%s", e.RawICS)
		}
	}
	// the master's VALARM is part of the master component only
	if !strings.Contains(string(events[0].Properties), `"alarms"`) || strings.Contains(string(events[1].Properties), `"alarms"`) {
		t.Fatalf("alarm must belong to the master only: %s / %s", events[0].Properties, events[1].Properties)
	}
}

func TestRecurrenceIDNormalizedAcrossZones(t *testing.T) {
	zoned := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:z\r\nRECURRENCE-ID;TZID=Asia/Kolkata:20261116T143000\r\nDTSTART:20261116T150000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	utc := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:z\r\nRECURRENCE-ID:20261116T090000Z\r\nDTSTART:20261116T150000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	a, _ := ParseICS(zoned, 1, 1, "c")
	b, _ := ParseICS(utc, 1, 1, "c")
	if a.RecurrenceID != "20261116T090000Z" || a.RecurrenceID != b.RecurrenceID {
		t.Fatalf("14:30 IST and 09:00Z are the same instant; got %q and %q", a.RecurrenceID, b.RecurrenceID)
	}
	allDay := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:d\r\nRECURRENCE-ID;VALUE=DATE:20261116\r\nDTSTART;VALUE=DATE:20261117\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	c, _ := ParseICS(allDay, 1, 1, "c")
	if c.RecurrenceID != "20261116" {
		t.Fatalf("all-day recurrence id = %q", c.RecurrenceID)
	}
}

func TestMixedUIDsRejected(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nDTSTART:20261116T090000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:b\r\nDTSTART:20261117T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, err := ParseICSAll(raw, 1, 1, "c"); err == nil {
		t.Fatal("a resource mixing UIDs must be rejected (it used to be silently merged into one event)")
	}
}

func TestFinalizeResourceKeepsOverridesWhenRegenerating(t *testing.T) {
	events, _ := ParseICSAll(seriesWithOverrides, 1, 1, "cal")
	events[0].Summary = "Daily sync" // a JMAP edit of the master
	final := FinalizeResource(events)

	full := final[0].RawICS
	if strings.Count(full, "BEGIN:VEVENT") != 3 {
		t.Fatalf("regenerated resource must still contain master + both overrides:\n%s", full)
	}
	if !strings.Contains(full, "SUMMARY:Daily sync") || !strings.Contains(full, "RECURRENCE-ID:20261116T090000Z") ||
		!strings.Contains(full, "RECURRENCE-ID:20261123T090000Z") || !strings.Contains(full, "STATUS:CANCELLED") {
		t.Fatalf("overrides lost on regeneration:\n%s", full)
	}
	if !strings.Contains(full, "BEGIN:VALARM") {
		t.Fatal("master alarm lost on regeneration")
	}
	// and the regenerated resource parses back to the same three components
	back, err := ParseICSAll(full, 1, 1, "cal")
	if err != nil || len(back) != 3 {
		t.Fatalf("round trip failed: %v, %d rows", err, len(back))
	}
	for _, e := range final {
		if e.ETag != final[0].ETag {
			t.Fatal("rows must share the new ETag")
		}
	}
}

func TestAllDayOverrideGeneratesDateRecurrenceID(t *testing.T) {
	events, _ := ParseICSAll("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:d\r\nRECURRENCE-ID;VALUE=DATE:20261116\r\nDTSTART;VALUE=DATE:20261117\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n", 1, 1, "c")
	events[0].RawICS = ""
	if out := GenerateICS(events[0]); !strings.Contains(out, "RECURRENCE-ID;VALUE=DATE:20261116") {
		t.Fatalf("all-day override must keep VALUE=DATE:\n%s", out)
	}
}
