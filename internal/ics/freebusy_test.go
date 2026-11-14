package ics

import (
	"strings"
	"testing"
)

func fbReq(body string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VFREEBUSY\r\n" + body + "END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
}

func TestParseFreeBusyRequest(t *testing.T) {
	req, err := ParseFreeBusyRequest(fbReq(
		"ORGANIZER:mailto:bob@x.com\r\nDTSTART:20261114T000000Z\r\nDTEND:20261121T000000Z\r\n" +
			"ATTENDEE:mailto:alice@x.com\r\nATTENDEE;CN=Carol:mailto:carol@x.com\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Organizer != "bob@x.com" || len(req.Attendees) != 2 || req.Attendees[1] != "carol@x.com" {
		t.Fatalf("unexpected parse: %+v", req)
	}
	if !req.End.After(req.Start) {
		t.Fatalf("bad window: %v .. %v", req.Start, req.End)
	}
}

func TestParseFreeBusyRequestRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"no component":     "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n",
		"no attendee":      fbReq("DTSTART:20261114T000000Z\r\nDTEND:20261121T000000Z\r\n"),
		"end before start": fbReq("DTSTART:20261121T000000Z\r\nDTEND:20261114T000000Z\r\nATTENDEE:mailto:a@x.com\r\n"),
		"missing end":      fbReq("DTSTART:20261114T000000Z\r\nATTENDEE:mailto:a@x.com\r\n"),
	}
	for name, raw := range cases {
		if _, err := ParseFreeBusyRequest(raw); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseICSRecordsTZIDForRecurrence(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:tz1\r\nDTSTART;TZID=India Standard Time:20261102T090000\r\n" +
		"RRULE:FREQ=WEEKLY\r\nSUMMARY:x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, err := ParseICS(raw, 1, 1, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.Properties), `"tzid":"Asia/Kolkata"`) {
		t.Fatalf("tzid not recorded for recurrence expansion: %s", e.Properties)
	}
}
