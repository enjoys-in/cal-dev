package ics

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/enjoys-in/cal-dev/internal/models"
)

func TestUnescapeICSSinglePass(t *testing.T) {
	cases := map[string]string{
		`a\\nb`:     `a\nb`, // escaped backslash followed by "n", not a newline
		`a\nb`:      "a\nb",
		`a\Nb`:      "a\nb",
		`a\;b\,c`:   "a;b,c",
		`x\\\\y`:    `x\\y`,
		`trail\`:    `trail\`,
		`\\;`:       `\;`,
		`unknown\q`: `unknown\q`,
	}
	for in, want := range cases {
		if got := unescapeICS(in); got != want {
			t.Errorf("unescapeICS(%q) = %q, want %q", in, got, want)
		}
	}
	for _, s := range []string{`C:\new\dir`, "line1\r\nline2", "a;b,c\\"} {
		got := unescapeICS(escapeICS(s))
		want := strings.ReplaceAll(s, "\r\n", "\n")
		if got != want {
			t.Errorf("round trip of %q = %q, want %q", s, got, want)
		}
	}
	if strings.Contains(escapeICS("a\rb"), "\r") {
		t.Error("escapeICS left a CR in the value")
	}
}

func TestFoldLines(t *testing.T) {
	long := "SUMMARY:" + strings.Repeat("é", 100) + "\r\n" // 2-byte runes
	out := foldLines(long)
	if unfolded := strings.Join(unfoldICS(out), ""); unfolded != strings.TrimSuffix(long, "\r\n") {
		t.Fatalf("unfolding the folded line did not restore it: %q", unfolded)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line longer than 75 octets: %d", len(line))
		}
		if !utf8.ValidString(strings.TrimPrefix(line, " ")) {
			t.Errorf("fold split a UTF-8 sequence: %q", line)
		}
	}
	if short := "UID:x\r\n"; foldLines(short) != short {
		t.Error("short line changed")
	}
}

func TestGeneratedICSIsFoldedAndSafe(t *testing.T) {
	e := &models.Event{
		UID:        "u1",
		Summary:    strings.Repeat("long summary ", 20),
		DTStart:    time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
		URL:        "https://x.example/\r\nATTENDEE:mailto:evil@example.com",
		Categories: []string{"a,b", "c;d"},
		Attendees:  []byte(`[{"email":"bob@example.com","display_name":"Bob \"The\"\r\nBuilder","role":"REQ-PARTICIPANT","status":"NEEDS-ACTION"}]`),
	}
	for _, out := range []string{GenerateICS(e), GenerateSchedulingICS(e, "REQUEST")} {
		for _, line := range strings.Split(out, "\r\n") {
			if len(line) > 75 {
				t.Errorf("unfolded line of %d octets: %q", len(line), line)
			}
			if strings.HasPrefix(line, "ATTENDEE:mailto:evil") {
				t.Error("URL value injected a content line")
			}
		}
	}
	out := strings.Join(unfoldICS(GenerateICS(e)), "\n")
	if !strings.Contains(out, `CATEGORIES:a\,b,c\;d`) {
		t.Errorf("categories not escaped:\n%s", out)
	}
	parsed, err := ParseICS(GenerateICS(e), 1, 1, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Categories) != 2 || parsed.Categories[0] != "a,b" || parsed.Categories[1] != "c;d" {
		t.Errorf("categories round trip = %q", parsed.Categories)
	}
}

func TestDurationIndependentOfOrder(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:d1\r\nDURATION:PT1H\r\nDTSTART:20260101T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, err := ParseICS(raw, 1, 1, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if e.DTEnd == nil || !e.DTEnd.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("DTEND = %v, want 10:00Z", e.DTEnd)
	}
}

func TestInvalidDTStartRejected(t *testing.T) {
	for _, prop := range []string{"DTSTART:garbage", "DTSTART:20260101T090000Z\r\nDTEND:nope"} {
		raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:d1\r\n" + prop + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		if _, err := ParseICSAll(raw, 1, 1, "cal"); err == nil {
			t.Errorf("expected an error for %q", prop)
		}
	}
}

func TestValidateICSRejectsVJOURNAL(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VJOURNAL\r\nUID:j\r\nEND:VJOURNAL\r\nEND:VCALENDAR\r\n"
	if err := ValidateICS(raw); !errors.Is(err, ErrUnsupportedComponent) {
		t.Errorf("ValidateICS(VJOURNAL) = %v, want ErrUnsupportedComponent", err)
	}
}

func TestEXDATEKeepsItsOwnTZID(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:x1\r\n" +
		"DTSTART:20260105T140000Z\r\nRRULE:FREQ=DAILY\r\n" +
		"EXDATE;TZID=America/New_York:20260106T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	e, err := ParseICS(raw, 1, 1, "cal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(e.Properties), "20260106T140000Z") {
		t.Errorf("EXDATE not pinned to UTC: %s", e.Properties)
	}
}

func TestResolveTZIDVendorPrefixAndWindowsNames(t *testing.T) {
	for in, want := range map[string]string{
		"/mozilla.org/20050126_1/America/New_York":           "America/New_York",
		"/softwarestudio.org/Olson_20011030_5/Europe/Berlin": "Europe/Berlin",
		"Central Europe Standard Time":                       "Europe/Budapest",
	} {
		loc, ok := ResolveTZID(in)
		if !ok || loc.String() != want {
			t.Errorf("ResolveTZID(%q) = %v, %v; want %s", in, loc, ok, want)
		}
	}
	if _, ok := ResolveTZID("/vendor/Not_A/Zone"); ok {
		t.Error("unknown zone resolved")
	}
	for win, iana := range windowsTZ {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("windowsTZ[%q] = %q does not load: %v", win, iana, err)
		}
	}
}

func TestCalendarTimezoneName(t *testing.T) {
	v := "BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Paris\r\nEND:VTIMEZONE\r\nEND:VCALENDAR\r\n"
	if name, ok := CalendarTimezoneName(v); !ok || name != "Europe/Paris" {
		t.Errorf("CalendarTimezoneName = %q, %v", name, ok)
	}
	if name, ok := CalendarTimezoneName("Asia/Tokyo"); !ok || name != "Asia/Tokyo" {
		t.Errorf("bare name = %q, %v", name, ok)
	}
}
