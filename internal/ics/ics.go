package ics

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/google/uuid"
)

// Common ICS date-time formats.
const (
	ICSDateTimeUTC   = "20060102T150405Z"
	ICSDateTimeLocal = "20060102T150405"
	ICSDateOnly      = "20060102"
)

// ParseICS parses a raw ICS string into Event fields.
func ParseICS(rawICS string, tenantID, userID, calendarID string) (*models.Event, error) {
	e := &models.Event{
		ID:             uuid.New().String(),
		TenantID:       tenantID,
		UserID:         userID,
		CalendarID:     calendarID,
		RawICS:         rawICS,
		SizeBytes:      len(rawICS),
		Status:         "CONFIRMED",
		Transparency:   "OPAQUE",
		Classification: "PUBLIC",
		ComponentType:  "VEVENT",
		Attendees:      json.RawMessage("[]"),
		Categories:     []string{},
		Properties:     json.RawMessage("{}"),
		DTStamp:        time.Now().UTC(),
	}

	lines := unfoldICS(rawICS)

	var inVEvent bool
	var attendees []models.Attendee

	for _, line := range lines {
		if strings.TrimSpace(line) == "BEGIN:VEVENT" {
			inVEvent = true
			continue
		}
		if strings.TrimSpace(line) == "END:VEVENT" {
			inVEvent = false
			continue
		}
		if strings.TrimSpace(line) == "BEGIN:VTODO" {
			e.ComponentType = "VTODO"
			inVEvent = true
			continue
		}
		if strings.TrimSpace(line) == "END:VTODO" {
			inVEvent = false
			continue
		}
		if !inVEvent {
			continue
		}

		name, params, value := parseLine(line)

		switch strings.ToUpper(name) {
		case "UID":
			e.UID = value
		case "SUMMARY":
			e.Summary = value
		case "DESCRIPTION":
			e.Description = unescapeICS(value)
		case "LOCATION":
			e.Location = unescapeICS(value)
		case "DTSTART":
			dt, allDay := parseDateTime(value, params)
			e.DTStart = dt
			e.AllDay = allDay
		case "DTEND":
			dt, _ := parseDateTime(value, params)
			e.DTEnd = &dt
		case "DURATION":
			e.Duration = value
		case "RRULE":
			e.RecurrenceRule = value
		case "RECURRENCE-ID":
			e.RecurrenceID = value
		case "SEQUENCE":
			if n := parseInt(value); n >= 0 {
				e.Sequence = n
			}
		case "STATUS":
			e.Status = strings.ToUpper(value)
		case "TRANSP":
			e.Transparency = strings.ToUpper(value)
		case "CLASS":
			e.Classification = strings.ToUpper(value)
		case "ORGANIZER":
			e.Organizer = extractCalAddress(value)
		case "ATTENDEE":
			attendee := parseAttendee(value, params)
			attendees = append(attendees, attendee)
		case "CATEGORIES":
			e.Categories = strings.Split(value, ",")
		case "PRIORITY":
			if n := parseInt(value); n >= 0 {
				e.Priority = n
			}
		case "URL":
			e.URL = value
		case "GEO":
			e.Geo = value
		case "DTSTAMP":
			dt, _ := parseDateTime(value, params)
			e.DTStamp = dt
		}
	}

	if e.UID == "" {
		e.UID = uuid.New().String()
	}
	if e.DTStamp.IsZero() {
		e.DTStamp = time.Now().UTC()
	}

	if len(attendees) > 0 {
		b, _ := json.Marshal(attendees)
		e.Attendees = json.RawMessage(b)
	}

	e.ETag = generateETag(rawICS)

	return e, nil
}

// GenerateICS generates a valid ICS string from an Event.
func GenerateICS(e *models.Event) string {
	if e.RawICS != "" {
		return e.RawICS
	}

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//CalDev//CalDAV Server//EN\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")

	compType := e.ComponentType
	if compType == "" {
		compType = "VEVENT"
	}

	b.WriteString(fmt.Sprintf("BEGIN:%s\r\n", compType))
	b.WriteString(fmt.Sprintf("UID:%s\r\n", e.UID))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", e.DTStamp.UTC().Format(ICSDateTimeUTC)))

	if e.AllDay {
		b.WriteString(fmt.Sprintf("DTSTART;VALUE=DATE:%s\r\n", e.DTStart.Format(ICSDateOnly)))
		if e.DTEnd != nil {
			b.WriteString(fmt.Sprintf("DTEND;VALUE=DATE:%s\r\n", e.DTEnd.Format(ICSDateOnly)))
		}
	} else {
		b.WriteString(fmt.Sprintf("DTSTART:%s\r\n", e.DTStart.UTC().Format(ICSDateTimeUTC)))
		if e.DTEnd != nil {
			b.WriteString(fmt.Sprintf("DTEND:%s\r\n", e.DTEnd.UTC().Format(ICSDateTimeUTC)))
		}
	}

	if e.Duration != "" {
		b.WriteString(fmt.Sprintf("DURATION:%s\r\n", e.Duration))
	}
	if e.Summary != "" {
		b.WriteString(fmt.Sprintf("SUMMARY:%s\r\n", escapeICS(e.Summary)))
	}
	if e.Description != "" {
		b.WriteString(fmt.Sprintf("DESCRIPTION:%s\r\n", escapeICS(e.Description)))
	}
	if e.Location != "" {
		b.WriteString(fmt.Sprintf("LOCATION:%s\r\n", escapeICS(e.Location)))
	}
	if e.RecurrenceRule != "" {
		b.WriteString(fmt.Sprintf("RRULE:%s\r\n", e.RecurrenceRule))
	}
	if e.RecurrenceID != "" {
		b.WriteString(fmt.Sprintf("RECURRENCE-ID:%s\r\n", e.RecurrenceID))
	}
	if e.Status != "" {
		b.WriteString(fmt.Sprintf("STATUS:%s\r\n", e.Status))
	}
	if e.Transparency != "" {
		b.WriteString(fmt.Sprintf("TRANSP:%s\r\n", e.Transparency))
	}
	if e.Classification != "" {
		b.WriteString(fmt.Sprintf("CLASS:%s\r\n", e.Classification))
	}
	if e.Organizer != "" {
		b.WriteString(fmt.Sprintf("ORGANIZER:mailto:%s\r\n", e.Organizer))
	}
	if e.Sequence > 0 {
		b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", e.Sequence))
	}
	if e.Priority > 0 {
		b.WriteString(fmt.Sprintf("PRIORITY:%d\r\n", e.Priority))
	}
	if e.URL != "" {
		b.WriteString(fmt.Sprintf("URL:%s\r\n", e.URL))
	}
	if e.Geo != "" {
		b.WriteString(fmt.Sprintf("GEO:%s\r\n", e.Geo))
	}
	if len(e.Categories) > 0 {
		b.WriteString(fmt.Sprintf("CATEGORIES:%s\r\n", strings.Join(e.Categories, ",")))
	}

	// Write attendees
	var attendees []models.Attendee
	if err := json.Unmarshal(e.Attendees, &attendees); err == nil {
		for _, a := range attendees {
			b.WriteString(formatAttendee(a))
		}
	}

	b.WriteString(fmt.Sprintf("END:%s\r\n", compType))
	b.WriteString("END:VCALENDAR\r\n")

	return b.String()
}

// GenerateSchedulingICS generates a METHOD-bearing ICS for scheduling.
func GenerateSchedulingICS(e *models.Event, method string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//CalDev//CalDAV Server//EN\r\n")
	b.WriteString(fmt.Sprintf("METHOD:%s\r\n", method))
	b.WriteString("CALSCALE:GREGORIAN\r\n")

	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString(fmt.Sprintf("UID:%s\r\n", e.UID))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", e.DTStamp.UTC().Format(ICSDateTimeUTC)))
	b.WriteString(fmt.Sprintf("DTSTART:%s\r\n", e.DTStart.UTC().Format(ICSDateTimeUTC)))
	if e.DTEnd != nil {
		b.WriteString(fmt.Sprintf("DTEND:%s\r\n", e.DTEnd.UTC().Format(ICSDateTimeUTC)))
	}
	if e.Summary != "" {
		b.WriteString(fmt.Sprintf("SUMMARY:%s\r\n", escapeICS(e.Summary)))
	}
	if e.Description != "" {
		b.WriteString(fmt.Sprintf("DESCRIPTION:%s\r\n", escapeICS(e.Description)))
	}
	if e.Location != "" {
		b.WriteString(fmt.Sprintf("LOCATION:%s\r\n", escapeICS(e.Location)))
	}
	if e.Organizer != "" {
		b.WriteString(fmt.Sprintf("ORGANIZER:mailto:%s\r\n", e.Organizer))
	}
	b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", e.Sequence))

	var attendees []models.Attendee
	if err := json.Unmarshal(e.Attendees, &attendees); err == nil {
		for _, a := range attendees {
			b.WriteString(formatAttendee(a))
		}
	}

	b.WriteString("END:VEVENT\r\n")
	b.WriteString("END:VCALENDAR\r\n")

	return b.String()
}

// generateETag creates a stable ETag from ICS content.
func generateETag(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf(`"%x"`, h[:16])
}

// GenerateETag is the exported version.
func GenerateETag(content string) string {
	return generateETag(content)
}

// unfoldICS handles line folding per RFC 5545.
func unfoldICS(raw string) []string {
	// Replace CRLF + space/tab with nothing (unfold)
	raw = strings.ReplaceAll(raw, "\r\n ", "")
	raw = strings.ReplaceAll(raw, "\r\n\t", "")
	raw = strings.ReplaceAll(raw, "\n ", "")
	raw = strings.ReplaceAll(raw, "\n\t", "")

	lines := strings.Split(raw, "\n")
	var result []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if l != "" {
			result = append(result, l)
		}
	}
	return result
}

// parseLine splits an ICS line into name, params, and value.
func parseLine(line string) (name string, params map[string]string, value string) {
	params = make(map[string]string)

	colonIdx := strings.Index(line, ":")
	if colonIdx == -1 {
		return line, params, ""
	}

	prefix := line[:colonIdx]
	value = line[colonIdx+1:]

	semicolonIdx := strings.Index(prefix, ";")
	if semicolonIdx == -1 {
		name = prefix
	} else {
		name = prefix[:semicolonIdx]
		paramStr := prefix[semicolonIdx+1:]
		for _, p := range strings.Split(paramStr, ";") {
			eqIdx := strings.Index(p, "=")
			if eqIdx != -1 {
				params[strings.ToUpper(p[:eqIdx])] = p[eqIdx+1:]
			}
		}
	}

	return name, params, value
}

// parseDateTime parses ICS date/datetime values.
func parseDateTime(value string, params map[string]string) (time.Time, bool) {
	value = strings.TrimSpace(value)

	// Check VALUE=DATE for all-day events
	if v, ok := params["VALUE"]; ok && strings.ToUpper(v) == "DATE" {
		t, err := time.Parse(ICSDateOnly, value)
		if err == nil {
			return t, true
		}
	}

	// Try UTC format
	t, err := time.Parse(ICSDateTimeUTC, value)
	if err == nil {
		return t, false
	}

	// Try local format
	t, err = time.Parse(ICSDateTimeLocal, value)
	if err == nil {
		// Apply TZID if present
		if tzid, ok := params["TZID"]; ok {
			loc, err := time.LoadLocation(tzid)
			if err == nil {
				t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc)
			}
		}
		return t, false
	}

	// Try date-only format
	t, err = time.Parse(ICSDateOnly, value)
	if err == nil {
		return t, true
	}

	return time.Now().UTC(), false
}

// parseInt parses a string to int, returning -1 on error.
func parseInt(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			return -1
		}
	}
	return n
}

var mailtoRe = regexp.MustCompile(`(?i)mailto:(.+)`)

// extractCalAddress extracts email from a cal-address value.
func extractCalAddress(value string) string {
	m := mailtoRe.FindStringSubmatch(value)
	if len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return strings.TrimSpace(value)
}

// parseAttendee creates an Attendee from ICS ATTENDEE line.
func parseAttendee(value string, params map[string]string) models.Attendee {
	a := models.Attendee{
		Email:  extractCalAddress(value),
		Role:   "REQ-PARTICIPANT",
		Status: "NEEDS-ACTION",
		Type:   "INDIVIDUAL",
	}
	if cn, ok := params["CN"]; ok {
		a.DisplayName = strings.Trim(cn, "\"")
	}
	if role, ok := params["ROLE"]; ok {
		a.Role = role
	}
	if partstat, ok := params["PARTSTAT"]; ok {
		a.Status = partstat
	}
	if rsvp, ok := params["RSVP"]; ok {
		a.RSVP = strings.ToUpper(rsvp) == "TRUE"
	}
	if cutype, ok := params["CUTYPE"]; ok {
		a.Type = cutype
	}
	return a
}

// formatAttendee formats an Attendee back to ICS format.
func formatAttendee(a models.Attendee) string {
	var parts []string
	if a.DisplayName != "" {
		parts = append(parts, fmt.Sprintf("CN=\"%s\"", a.DisplayName))
	}
	parts = append(parts, fmt.Sprintf("ROLE=%s", a.Role))
	parts = append(parts, fmt.Sprintf("PARTSTAT=%s", a.Status))
	if a.RSVP {
		parts = append(parts, "RSVP=TRUE")
	}
	if a.Type != "" {
		parts = append(parts, fmt.Sprintf("CUTYPE=%s", a.Type))
	}

	paramStr := strings.Join(parts, ";")
	return fmt.Sprintf("ATTENDEE;%s:mailto:%s\r\n", paramStr, a.Email)
}

// escapeICS escapes special characters for ICS text.
func escapeICS(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	return s
}

// unescapeICS reverses ICS escaping.
func unescapeICS(s string) string {
	s = strings.ReplaceAll(s, "\\n", "\n")
	s = strings.ReplaceAll(s, "\\N", "\n")
	s = strings.ReplaceAll(s, "\\;", ";")
	s = strings.ReplaceAll(s, "\\,", ",")
	s = strings.ReplaceAll(s, "\\\\", "\\")
	return s
}

// ValidateICS performs basic validation of an ICS string.
func ValidateICS(raw string) error {
	if !strings.Contains(raw, "BEGIN:VCALENDAR") {
		return fmt.Errorf("missing BEGIN:VCALENDAR")
	}
	if !strings.Contains(raw, "END:VCALENDAR") {
		return fmt.Errorf("missing END:VCALENDAR")
	}
	if !strings.Contains(raw, "BEGIN:VEVENT") && !strings.Contains(raw, "BEGIN:VTODO") && !strings.Contains(raw, "BEGIN:VJOURNAL") {
		return fmt.Errorf("no calendar component found (VEVENT/VTODO/VJOURNAL)")
	}
	return nil
}
