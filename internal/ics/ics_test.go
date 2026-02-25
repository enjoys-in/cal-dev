package ics

import (
	"strings"
	"testing"
)

// ── ParseICS ────────────────────────────────────────────────

func TestParseICS_BasicEvent(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//Test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:test-uid-123@caldev\r\nSUMMARY:Team Meeting\r\n" +
		"DESCRIPTION:Weekly standup\r\nLOCATION:Room 42\r\n" +
		"DTSTART:20260226T140000Z\r\nDTEND:20260226T150000Z\r\n" +
		"STATUS:CONFIRMED\r\nSEQUENCE:0\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, err := ParseICS(raw, "tenant-1", "user-1", "cal-1")
	if err != nil {
		t.Fatalf("ParseICS failed: %v", err)
	}

	if event.UID != "test-uid-123@caldev" {
		t.Errorf("expected UID 'test-uid-123@caldev', got '%s'", event.UID)
	}
	if event.Summary != "Team Meeting" {
		t.Errorf("expected Summary 'Team Meeting', got '%s'", event.Summary)
	}
	if event.Description != "Weekly standup" {
		t.Errorf("expected Description 'Weekly standup', got '%s'", event.Description)
	}
	if event.Location != "Room 42" {
		t.Errorf("expected Location 'Room 42', got '%s'", event.Location)
	}
	if event.Status != "CONFIRMED" {
		t.Errorf("expected Status 'CONFIRMED', got '%s'", event.Status)
	}
	if event.TenantID != "tenant-1" {
		t.Errorf("expected TenantID 'tenant-1', got '%s'", event.TenantID)
	}
	if event.CalendarID != "cal-1" {
		t.Errorf("expected CalendarID 'cal-1', got '%s'", event.CalendarID)
	}
	if event.ComponentType != "VEVENT" {
		t.Errorf("expected ComponentType 'VEVENT', got '%s'", event.ComponentType)
	}
}

func TestParseICS_AllDayEvent(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:allday@caldev\r\nSUMMARY:Holiday\r\n" +
		"DTSTART;VALUE=DATE:20260226\r\nDTEND;VALUE=DATE:20260227\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, err := ParseICS(raw, "t", "u", "c")
	if err != nil {
		t.Fatalf("ParseICS failed: %v", err)
	}

	if !event.AllDay {
		t.Error("expected AllDay to be true")
	}
	if event.Summary != "Holiday" {
		t.Errorf("expected Summary 'Holiday', got '%s'", event.Summary)
	}
}

func TestParseICS_WithAttendees(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:att-event@caldev\r\nSUMMARY:Meeting\r\n" +
		"DTSTART:20260226T100000Z\r\nDTEND:20260226T110000Z\r\n" +
		"ORGANIZER;CN=Boss:mailto:boss@example.com\r\n" +
		"ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;CN=Jane:mailto:jane@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, err := ParseICS(raw, "t", "u", "c")
	if err != nil {
		t.Fatalf("ParseICS failed: %v", err)
	}

	if event.Organizer != "boss@example.com" {
		t.Errorf("expected Organizer 'boss@example.com', got '%s'", event.Organizer)
	}
}

func TestParseICS_WithRecurrence(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:recur@caldev\r\nSUMMARY:Weekly\r\n" +
		"DTSTART:20260226T090000Z\r\nDTEND:20260226T100000Z\r\n" +
		"RRULE:FREQ=WEEKLY;BYDAY=MO\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, err := ParseICS(raw, "t", "u", "c")
	if err != nil {
		t.Fatalf("ParseICS failed: %v", err)
	}

	if event.RecurrenceRule != "FREQ=WEEKLY;BYDAY=MO" {
		t.Errorf("expected RecurrenceRule 'FREQ=WEEKLY;BYDAY=MO', got '%s'", event.RecurrenceRule)
	}
}

func TestParseICS_WithCategories(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:cat@caldev\r\nSUMMARY:Tagged\r\n" +
		"DTSTART:20260226T090000Z\r\n" +
		"CATEGORIES:Work,Meeting\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, err := ParseICS(raw, "t", "u", "c")
	if err != nil {
		t.Fatalf("ParseICS failed: %v", err)
	}

	if len(event.Categories) != 2 {
		t.Errorf("expected 2 categories, got %d", len(event.Categories))
	}
}

// ── GenerateICS ─────────────────────────────────────────────

func TestGenerateICS_RoundTrip(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Test//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:roundtrip@caldev\r\nSUMMARY:Roundtrip Test\r\n" +
		"DTSTART:20260226T140000Z\r\nDTEND:20260226T150000Z\r\n" +
		"STATUS:CONFIRMED\r\nSEQUENCE:1\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, err := ParseICS(raw, "t", "u", "c")
	if err != nil {
		t.Fatalf("ParseICS failed: %v", err)
	}

	generated := GenerateICS(event)

	if !strings.Contains(generated, "BEGIN:VCALENDAR") {
		t.Error("generated ICS missing BEGIN:VCALENDAR")
	}
	if !strings.Contains(generated, "UID:roundtrip@caldev") {
		t.Error("generated ICS missing UID")
	}
	if !strings.Contains(generated, "SUMMARY:Roundtrip Test") {
		t.Error("generated ICS missing SUMMARY")
	}
	if !strings.Contains(generated, "END:VCALENDAR") {
		t.Error("generated ICS missing END:VCALENDAR")
	}
}

// ── ValidateICS ─────────────────────────────────────────────

func TestValidateICS_Valid(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if err := ValidateICS(raw); err != nil {
		t.Errorf("expected valid ICS, got error: %v", err)
	}
}

func TestValidateICS_MissingVCALENDAR(t *testing.T) {
	if err := ValidateICS("BEGIN:VEVENT\r\nEND:VEVENT"); err == nil {
		t.Error("expected error for missing BEGIN:VCALENDAR")
	}
}

func TestValidateICS_MissingComponent(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"
	if err := ValidateICS(raw); err == nil {
		t.Error("expected error for missing component")
	}
}

// ── ParseSchedulingICS ──────────────────────────────────────

func TestParseSchedulingICS_Reply(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nUID:invite-123@caldev\r\nSUMMARY:Team Meeting\r\n" +
		"ATTENDEE;PARTSTAT=ACCEPTED;CN=Jane:mailto:jane@example.com\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	reply, err := ParseSchedulingICS(raw)
	if err != nil {
		t.Fatalf("ParseSchedulingICS failed: %v", err)
	}

	if reply.Method != "REPLY" {
		t.Errorf("expected Method 'REPLY', got '%s'", reply.Method)
	}
	if reply.EventUID != "invite-123@caldev" {
		t.Errorf("expected EventUID 'invite-123@caldev', got '%s'", reply.EventUID)
	}
	if reply.AttendeeEmail != "jane@example.com" {
		t.Errorf("expected AttendeeEmail 'jane@example.com', got '%s'", reply.AttendeeEmail)
	}
	if reply.Partstat != "ACCEPTED" {
		t.Errorf("expected Partstat 'ACCEPTED', got '%s'", reply.Partstat)
	}
	if reply.Summary != "Team Meeting" {
		t.Errorf("expected Summary 'Team Meeting', got '%s'", reply.Summary)
	}
}

func TestParseSchedulingICS_Decline(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nUID:decline-1@caldev\r\n" +
		"ATTENDEE;PARTSTAT=DECLINED:mailto:bob@example.com\r\n" +
		"COMMENT:Sorry\\, can't make it\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	reply, err := ParseSchedulingICS(raw)
	if err != nil {
		t.Fatalf("ParseSchedulingICS failed: %v", err)
	}

	if reply.Partstat != "DECLINED" {
		t.Errorf("expected Partstat 'DECLINED', got '%s'", reply.Partstat)
	}
	if reply.AttendeeEmail != "bob@example.com" {
		t.Errorf("expected AttendeeEmail 'bob@example.com', got '%s'", reply.AttendeeEmail)
	}
	if reply.Comment != "Sorry, can't make it" {
		t.Errorf("expected unescaped comment, got '%s'", reply.Comment)
	}
}

func TestParseSchedulingICS_Cancel(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nMETHOD:CANCEL\r\n" +
		"BEGIN:VEVENT\r\nUID:cancel-1@caldev\r\nSUMMARY:Cancelled Meeting\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	reply, err := ParseSchedulingICS(raw)
	if err != nil {
		t.Fatalf("ParseSchedulingICS failed: %v", err)
	}

	if reply.Method != "CANCEL" {
		t.Errorf("expected Method 'CANCEL', got '%s'", reply.Method)
	}
}

func TestParseSchedulingICS_NoMethod(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:nomethod@caldev\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	_, err := ParseSchedulingICS(raw)
	if err == nil {
		t.Error("expected error for missing METHOD")
	}
	if !strings.Contains(err.Error(), "no METHOD") {
		t.Errorf("expected 'no METHOD' error, got: %v", err)
	}
}

func TestParseSchedulingICS_NoUID(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nSUMMARY:No UID\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	_, err := ParseSchedulingICS(raw)
	if err == nil {
		t.Error("expected error for missing UID")
	}
}

func TestParseSchedulingICS_Invalid(t *testing.T) {
	_, err := ParseSchedulingICS("not an ICS file")
	if err == nil {
		t.Error("expected error for invalid ICS")
	}
}

// ── GenerateETag ────────────────────────────────────────────

func TestGenerateETag_Deterministic(t *testing.T) {
	etag1 := GenerateETag("hello world")
	etag2 := GenerateETag("hello world")

	if etag1 != etag2 {
		t.Errorf("expected same ETag for same input, got '%s' vs '%s'", etag1, etag2)
	}

	etag3 := GenerateETag("different content")
	if etag1 == etag3 {
		t.Error("expected different ETag for different input")
	}
}

// ── GenerateSchedulingICS ───────────────────────────────────

func TestGenerateSchedulingICS_Request(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nUID:sched@caldev\r\nSUMMARY:Invite\r\n" +
		"DTSTART:20260226T140000Z\r\nDTEND:20260226T150000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	event, _ := ParseICS(raw, "t", "u", "c")
	result := GenerateSchedulingICS(event, "REQUEST")

	if !strings.Contains(result, "METHOD:REQUEST") {
		t.Error("generated scheduling ICS missing METHOD:REQUEST")
	}
	if !strings.Contains(result, "UID:sched@caldev") {
		t.Error("generated scheduling ICS missing UID")
	}
}

// ── Helpers ─────────────────────────────────────────────────

func TestEscapeUnescapeICS(t *testing.T) {
	original := "Hello; World, with\nnewlines"
	escaped := escapeICS(original)
	unescaped := unescapeICS(escaped)

	if unescaped != original {
		t.Errorf("round-trip failed: got '%s'", unescaped)
	}
}

func TestUnfoldICS(t *testing.T) {
	// RFC 5545 line folding: continuation lines start with a space
	folded := "DESCRIPTION:This is a long\r\n  description that continues\r\nSUMMARY:Short\r\n"
	lines := unfoldICS(folded)

	found := false
	for _, l := range lines {
		if strings.HasPrefix(l, "DESCRIPTION:") {
			found = true
			if !strings.Contains(l, "continues") {
				t.Errorf("unfolding failed, got: '%s'", l)
			}
		}
	}
	if !found {
		t.Error("DESCRIPTION line not found after unfolding")
	}
}
