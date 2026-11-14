package sync

import "testing"

const feedWithOverrides = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Google//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:series-1@google.com\r\nDTSTART:20261102T090000Z\r\nDTEND:20261102T100000Z\r\nRRULE:FREQ=WEEKLY\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:single-1@google.com\r\nDTSTART:20261110T120000Z\r\nDTEND:20261110T130000Z\r\nSUMMARY:Lunch\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:series-1@google.com\r\nRECURRENCE-ID:20261109T090000Z\r\nDTSTART:20261109T140000Z\r\nDTEND:20261109T150000Z\r\nSUMMARY:Standup (moved)\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestParseAllEventsKeepsOverridesAsSeparateRows(t *testing.T) {
	rows, err := parseAllEvents(feedWithOverrides, 1, 1, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (series master, its override, one single event), got %d", len(rows))
	}

	type k struct{ uid, rec string }
	have := map[k]string{}
	for _, r := range rows {
		have[k{r.UID, r.RecurrenceID}] = r.Summary
	}
	if have[k{"series-1@google.com", ""}] != "Standup" {
		t.Fatalf("master row missing or overwritten by its override: %v", have)
	}
	if have[k{"series-1@google.com", "20261109T090000Z"}] != "Standup (moved)" {
		t.Fatalf("override row missing (it used to be dropped by UNIQUE(calendar_id, uid)): %v", have)
	}
	if have[k{"single-1@google.com", ""}] != "Lunch" {
		t.Fatalf("single event missing: %v", have)
	}
	// (calendar, uid, recurrence_id) must be unique across the batch or the INSERT would fail
	if len(have) != len(rows) {
		t.Fatalf("duplicate (uid, recurrence_id) keys in the batch: %v", have)
	}
}
