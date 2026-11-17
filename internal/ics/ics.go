package ics

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/google/uuid"
)

// ProdID identifies Airsend Calendar as the producer of every .ics it generates (RFC 5545 PRODID).
const ProdID = "-//Airsend//Calendar//EN"

// Common ICS date-time formats.
const (
	ICSDateTimeUTC   = "20060102T150405Z"
	ICSDateTimeLocal = "20060102T150405"
	ICSDateOnly      = "20060102"
)

// Alarm represents a VALARM component per RFC 5545 §3.6.6.
type Alarm struct {
	Action      string `json:"action"`                // DISPLAY, AUDIO, EMAIL
	Trigger     string `json:"trigger"`               // e.g. -PT15M, -PT1H
	Description string `json:"description,omitempty"` // Reminder text
	Duration    string `json:"duration,omitempty"`
	Repeat      int    `json:"repeat,omitempty"`
}

// ParseICS parses a raw ICS string and returns its primary component: the master event, or
// the first component when the resource has no master. Use ParseICSAll to get every
// component of a resource that also carries RECURRENCE-ID overrides.
func ParseICS(rawICS string, tenantID, userID int, calendarID string) (*models.Event, error) {
	events, err := ParseICSAll(rawICS, tenantID, userID, calendarID)
	if err != nil {
		return nil, err
	}
	return events[0], nil
}

// component is one VEVENT/VTODO block of a resource (unfolded lines, without BEGIN/END).
type component struct {
	kind  string
	lines []string
}

// splitComponents returns the VEVENT/VTODO blocks of a calendar object. VTIMEZONE and other
// components are skipped; nested VALARMs stay inside their parent block.
func splitComponents(lines []string) []component {
	var out []component
	var cur *component
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case cur == nil && (trimmed == "BEGIN:VEVENT" || trimmed == "BEGIN:VTODO"):
			cur = &component{kind: strings.TrimPrefix(trimmed, "BEGIN:")}
		case cur != nil && trimmed == "END:"+cur.kind:
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			cur.lines = append(cur.lines, line)
		}
	}
	return out
}

// wrapComponent renders a single component as a standalone calendar object.
func wrapComponent(c component) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:" + ProdID + "\r\n")
	b.WriteString("BEGIN:" + c.kind + "\r\n")
	for _, l := range c.lines {
		b.WriteString(l + "\r\n")
	}
	b.WriteString("END:" + c.kind + "\r\nEND:VCALENDAR\r\n")
	return b.String()
}

// ParseICSAll parses every VEVENT/VTODO component of one calendar object resource into its own
// event: the master series (RecurrenceID == "") and one event per RECURRENCE-ID override. The
// result is never empty and the primary row (master, else earliest override) comes first.
//
// Storage invariant for all rows of a resource: they share the resource's ETag and SizeBytes.
// The primary row's RawICS is the complete resource exactly as received; every other row's RawICS
// is just its own component wrapped in a VCALENDAR.
func ParseICSAll(rawICS string, tenantID, userID int, calendarID string) ([]*models.Event, error) {
	blocks := splitComponents(unfoldICS(rawICS))

	type parsed struct {
		e   *models.Event
		blk component
	}
	var items []parsed
	if len(blocks) == 0 {
		// No component at all: keep the historic lenient behaviour (one default event).
		e, _ := parseComponent(nil, "VEVENT", tenantID, userID, calendarID)
		items = append(items, parsed{e: e})
	}
	for _, blk := range blocks {
		e, err := parseComponent(blk.lines, blk.kind, tenantID, userID, calendarID)
		if err != nil {
			return nil, err
		}
		items = append(items, parsed{e: e, blk: blk})
	}

	// Every component of a resource must carry the same UID (RFC 4791 §4.1).
	uid := ""
	for _, it := range items {
		if it.e.UID == "" {
			continue
		}
		if uid == "" {
			uid = it.e.UID
		} else if it.e.UID != uid {
			return nil, fmt.Errorf("all components of a calendar object resource must share one UID (found %q and %q)", uid, it.e.UID)
		}
	}
	if uid == "" {
		uid = uuid.New().String()
	}

	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].e.RecurrenceID, items[j].e.RecurrenceID
		if (a == "") != (b == "") {
			return a == "" // master first
		}
		return a < b
	})

	etag := generateETag(rawICS)
	events := make([]*models.Event, 0, len(items))
	for i, it := range items {
		e := it.e
		e.UID = uid
		e.ETag = etag
		e.SizeBytes = len(rawICS)
		if i == 0 {
			e.RawICS = rawICS
		} else {
			e.RawICS = wrapComponent(it.blk)
		}
		events = append(events, e)
	}
	return events, nil
}

var recurrenceIDRe = regexp.MustCompile(`^\d{8}(T\d{6}Z?)?$`)

// normalizeRecurrenceID turns a RECURRENCE-ID into a canonical key: a UTC instant
// (20261116T033000Z) for timed events or a plain date (20261116) for all-day ones, so the same
// occurrence always yields the same key whatever zone the client wrote it in.
func normalizeRecurrenceID(value string, params map[string]string) string {
	value = strings.TrimSpace(value)
	if !recurrenceIDRe.MatchString(value) {
		return value
	}
	t, allDay := parseDateTime(value, params)
	if allDay {
		return t.Format(ICSDateOnly)
	}
	return t.UTC().Format(ICSDateTimeUTC)
}

// parseComponent parses the property lines of one VEVENT/VTODO (without its BEGIN/END).
// It fails when DTSTART or DTEND holds a value that is not a valid date or date-time.
func parseComponent(lines []string, kind string, tenantID, userID int, calendarID string) (*models.Event, error) {
	e := &models.Event{
		ID:             uuid.New().String(),
		TenantID:       tenantID,
		UserID:         userID,
		CalendarID:     calendarID,
		Status:         "CONFIRMED",
		Transparency:   "OPAQUE",
		Classification: "PUBLIC",
		ComponentType:  kind,
		Attendees:      json.RawMessage("[]"),
		Categories:     []string{},
		Properties:     json.RawMessage("{}"),
		DTStamp:        time.Now().UTC(),
	}

	var inVAlarm bool
	var attendees []models.Attendee
	var alarms []Alarm
	var currentAlarm Alarm
	var exdates []string
	var exdateLocs []*time.Location // TZID of each EXDATE value, nil when it has none
	var rdates []string
	var tzidName string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "BEGIN:VALARM" {
			inVAlarm = true
			currentAlarm = Alarm{Action: "DISPLAY", Trigger: "-PT15M"}
			continue
		}
		if trimmed == "END:VALARM" {
			inVAlarm = false
			alarms = append(alarms, currentAlarm)
			continue
		}

		name, params, value := parseLine(line)
		upperName := strings.ToUpper(name)

		if inVAlarm {
			switch upperName {
			case "ACTION":
				currentAlarm.Action = strings.ToUpper(value)
			case "TRIGGER":
				currentAlarm.Trigger = value
			case "DESCRIPTION":
				currentAlarm.Description = unescapeICS(value)
			case "DURATION":
				currentAlarm.Duration = value
			case "REPEAT":
				if n := parseInt(value); n >= 0 {
					currentAlarm.Repeat = n
				}
			}
			continue
		}

		switch upperName {
		case "UID":
			e.UID = value
		case "SUMMARY":
			e.Summary = unescapeICS(value)
		case "DESCRIPTION":
			e.Description = unescapeICS(value)
		case "LOCATION":
			e.Location = unescapeICS(value)
		case "DTSTART":
			dt, allDay := parseDateTime(value, params)
			if dt.IsZero() {
				return nil, fmt.Errorf("invalid DTSTART value %q", value)
			}
			e.DTStart = dt
			e.AllDay = allDay
			if tz, ok := params["TZID"]; ok {
				if loc, ok := ResolveTZID(tz); ok {
					tzidName = loc.String()
				}
			}
		case "DTEND":
			dt, _ := parseDateTime(value, params)
			if dt.IsZero() {
				return nil, fmt.Errorf("invalid DTEND value %q", value)
			}
			e.DTEnd = &dt
		case "DURATION":
			e.Duration = value // DTEND is derived from it after all properties are read
		case "RRULE":
			e.RecurrenceRule = value
		case "RECURRENCE-ID":
			e.RecurrenceID = normalizeRecurrenceID(value, params)
		case "EXDATE":
			loc, _ := ResolveTZID(params["TZID"])
			for _, ed := range strings.Split(value, ",") {
				ed = strings.TrimSpace(ed)
				if ed != "" {
					exdates = append(exdates, ed)
					exdateLocs = append(exdateLocs, loc)
				}
			}
		case "RDATE":
			for _, rd := range strings.Split(value, ",") {
				rd = strings.TrimSpace(rd)
				if rd != "" {
					rdates = append(rdates, rd)
				}
			}
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
			if len(attendees) < models.MaxAttendeesPerEvent {
				attendee := parseAttendee(value, params)
				attendees = append(attendees, attendee)
			}
		case "CATEGORIES":
			e.Categories = splitTextList(value)
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

	if e.DTStamp.IsZero() {
		e.DTStamp = time.Now().UTC()
	}

	// Stored EXDATEs carry no TZID and local values are read in the series' zone, so a local value
	// in another zone than DTSTART's is pinned to UTC.
	for i, loc := range exdateLocs {
		if loc != nil && loc.String() != tzidName {
			if t, err := time.ParseInLocation(ICSDateTimeLocal, exdates[i], loc); err == nil {
				exdates[i] = t.UTC().Format(ICSDateTimeUTC)
			}
		}
	}

	if e.Duration != "" && e.DTEnd == nil && !e.DTStart.IsZero() {
		if d := parseDurationString(e.Duration); d > 0 {
			dt := e.DTStart.Add(d)
			e.DTEnd = &dt
		}
	}

	if len(attendees) > 0 {
		b, _ := json.Marshal(attendees)
		e.Attendees = json.RawMessage(b)
	}

	propsMap := make(map[string]interface{})
	if len(alarms) > 0 {
		propsMap["alarms"] = alarms
	}
	if len(exdates) > 0 {
		propsMap["exdates"] = exdates
	}
	if len(rdates) > 0 {
		propsMap["rdates"] = rdates
	}
	if tzidName != "" {
		propsMap["tzid"] = tzidName
	}
	if len(propsMap) > 0 {
		if b, err := json.Marshal(propsMap); err == nil {
			e.Properties = json.RawMessage(b)
		}
	}
	return e, nil
}

// GenerateICS generates a valid ICS string from an Event.
func GenerateICS(e *models.Event) string {
	if e.RawICS != "" {
		return e.RawICS
	}

	var b strings.Builder
	writeCalendarHeader(&b)
	writeComponent(&b, e)
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

func writeCalendarHeader(b *strings.Builder) {
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:" + ProdID + "\r\n")
	b.WriteString("CALSCALE:GREGORIAN\r\n")
}

// GenerateResourceICS renders all rows of one resource (master + RECURRENCE-ID overrides) as a
// single calendar object, master first.
func GenerateResourceICS(events []*models.Event) string {
	sorted := append([]*models.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].RecurrenceID, sorted[j].RecurrenceID
		if (a == "") != (b == "") {
			return a == ""
		}
		return a < b
	})
	var b strings.Builder
	writeCalendarHeader(&b)
	for _, e := range sorted {
		writeComponent(&b, e)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// FinalizeResource regenerates the raw ICS of every row from its fields and applies the
// storage invariant documented on ParseICSAll: the primary row (master, else earliest override)
// holds the whole resource, the others hold only their own component, and all rows share the
// resource's ETag and size. Call it whenever fields changed and the original raw text is stale.
// The rows are returned primary-first.
func FinalizeResource(events []*models.Event) []*models.Event {
	if len(events) == 0 {
		return events
	}
	sorted := append([]*models.Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].RecurrenceID, sorted[j].RecurrenceID
		if (a == "") != (b == "") {
			return a == ""
		}
		return a < b
	})
	full := GenerateResourceICS(sorted)
	etag := generateETag(full)
	for i, e := range sorted {
		if i == 0 {
			e.RawICS = full
		} else {
			e.RawICS = ""
			e.RawICS = GenerateICS(e)
		}
		e.ETag = etag
		e.SizeBytes = len(full)
	}
	return sorted
}

// writeComponent writes one VEVENT/VTODO (without the VCALENDAR wrapper).
func writeComponent(out *strings.Builder, e *models.Event) {
	b := &strings.Builder{}
	defer func() { out.WriteString(foldLines(b.String())) }()

	compType := e.ComponentType
	if compType == "" {
		compType = "VEVENT"
	}

	loc := eventLocation(e)

	b.WriteString(fmt.Sprintf("BEGIN:%s\r\n", compType))
	b.WriteString(fmt.Sprintf("UID:%s\r\n", e.UID))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", e.DTStamp.UTC().Format(ICSDateTimeUTC)))

	if e.AllDay {
		b.WriteString(fmt.Sprintf("DTSTART;VALUE=DATE:%s\r\n", e.DTStart.Format(ICSDateOnly)))
		if e.DTEnd != nil {
			b.WriteString(fmt.Sprintf("DTEND;VALUE=DATE:%s\r\n", e.DTEnd.Format(ICSDateOnly)))
		}
	} else {
		writeDateTime(b, "DTSTART", e.DTStart, loc)
		if e.DTEnd != nil {
			writeDateTime(b, "DTEND", *e.DTEnd, loc)
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
		switch {
		case len(e.RecurrenceID) == 8: // date-only key of an all-day occurrence
			b.WriteString(fmt.Sprintf("RECURRENCE-ID;VALUE=DATE:%s\r\n", e.RecurrenceID))
		case loc != nil:
			// Keys are stored as UTC instants; write them back in the series' zone so the
			// value type matches DTSTART, the way CalDAV clients send them.
			if t, err := time.Parse(ICSDateTimeUTC, e.RecurrenceID); err == nil {
				writeDateTime(b, "RECURRENCE-ID", t, loc)
			} else {
				b.WriteString(fmt.Sprintf("RECURRENCE-ID:%s\r\n", e.RecurrenceID))
			}
		default:
			b.WriteString(fmt.Sprintf("RECURRENCE-ID:%s\r\n", e.RecurrenceID))
		}
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
		org := e.Organizer
		if !strings.HasPrefix(strings.ToLower(org), "mailto:") {
			org = "mailto:" + org
		}
		b.WriteString(fmt.Sprintf("ORGANIZER:%s\r\n", stripLineBreaks(org)))
	}
	if e.Sequence > 0 {
		b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", e.Sequence))
	}
	if e.Priority > 0 {
		b.WriteString(fmt.Sprintf("PRIORITY:%d\r\n", e.Priority))
	}
	if e.URL != "" {
		b.WriteString(fmt.Sprintf("URL:%s\r\n", stripLineBreaks(e.URL)))
	}
	if e.Geo != "" {
		b.WriteString(fmt.Sprintf("GEO:%s\r\n", e.Geo))
	}
	if len(e.Categories) > 0 {
		cats := make([]string, len(e.Categories))
		for i, c := range e.Categories {
			cats[i] = escapeICS(c)
		}
		b.WriteString(fmt.Sprintf("CATEGORIES:%s\r\n", strings.Join(cats, ",")))
	}

	// Write attendees
	var attendees []models.Attendee
	if err := json.Unmarshal(e.Attendees, &attendees); err == nil {
		for _, a := range attendees {
			b.WriteString(formatAttendee(a))
		}
	}

	// Write EXDATE, RDATE and VALARM if in Properties
	if len(e.Properties) > 0 && string(e.Properties) != "{}" {
		var propsMap map[string]interface{}
		if err := json.Unmarshal(e.Properties, &propsMap); err == nil && propsMap != nil {
			if exdates, ok := propsMap["exdates"].([]interface{}); ok {
				for _, ed := range exdates {
					if edStr, ok := ed.(string); ok && edStr != "" {
						writeDateValue(b, "EXDATE", edStr, loc)
					}
				}
			}
			if rdates, ok := propsMap["rdates"].([]interface{}); ok {
				for _, rd := range rdates {
					if rdStr, ok := rd.(string); ok && rdStr != "" {
						writeDateValue(b, "RDATE", rdStr, loc)
					}
				}
			}
			if alarms, ok := propsMap["alarms"].([]interface{}); ok {
				for _, al := range alarms {
					if alMap, ok := al.(map[string]interface{}); ok {
						action := "DISPLAY"
						if a, ok := alMap["action"].(string); ok && a != "" {
							action = a
						}
						trigger := "-PT15M"
						if tr, ok := alMap["trigger"].(string); ok && tr != "" {
							trigger = tr
						}
						desc := "Reminder"
						if d, ok := alMap["description"].(string); ok && d != "" {
							desc = d
						}
						b.WriteString("BEGIN:VALARM\r\n")
						b.WriteString(fmt.Sprintf("ACTION:%s\r\n", action))
						b.WriteString(fmt.Sprintf("TRIGGER:%s\r\n", trigger))
						b.WriteString(fmt.Sprintf("DESCRIPTION:%s\r\n", escapeICS(desc)))
						b.WriteString("END:VALARM\r\n")
					}
				}
			}
		}
	}

	b.WriteString(fmt.Sprintf("END:%s\r\n", compType))
}

// GenerateSchedulingICS generates a METHOD-bearing ICS for scheduling.
func GenerateSchedulingICS(e *models.Event, method string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:" + ProdID + "\r\n")
	b.WriteString(fmt.Sprintf("METHOD:%s\r\n", method))
	b.WriteString("CALSCALE:GREGORIAN\r\n")

	b.WriteString("BEGIN:VEVENT\r\n")
	b.WriteString(fmt.Sprintf("UID:%s\r\n", e.UID))
	b.WriteString(fmt.Sprintf("DTSTAMP:%s\r\n", e.DTStamp.UTC().Format(ICSDateTimeUTC)))
	loc := eventLocation(e)
	if e.AllDay {
		b.WriteString(fmt.Sprintf("DTSTART;VALUE=DATE:%s\r\n", e.DTStart.Format(ICSDateOnly)))
		if e.DTEnd != nil {
			b.WriteString(fmt.Sprintf("DTEND;VALUE=DATE:%s\r\n", e.DTEnd.Format(ICSDateOnly)))
		}
	} else {
		writeDateTime(&b, "DTSTART", e.DTStart, loc)
		if e.DTEnd != nil {
			writeDateTime(&b, "DTEND", *e.DTEnd, loc)
		}
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
		org := e.Organizer
		if !strings.HasPrefix(strings.ToLower(org), "mailto:") {
			org = "mailto:" + org
		}
		b.WriteString(fmt.Sprintf("ORGANIZER:%s\r\n", stripLineBreaks(org)))
	}
	b.WriteString(fmt.Sprintf("SEQUENCE:%d\r\n", e.Sequence))
	status := e.Status
	if strings.ToUpper(method) == "CANCEL" {
		status = "CANCELLED"
	}
	b.WriteString(fmt.Sprintf("STATUS:%s\r\n", status))

	var attendees []models.Attendee
	if err := json.Unmarshal(e.Attendees, &attendees); err == nil {
		for _, a := range attendees {
			// Force RSVP=TRUE in scheduling ICS so mail clients show Accept/Decline
			a.RSVP = true
			b.WriteString(formatAttendee(a))
		}
	}

	b.WriteString("END:VEVENT\r\n")
	b.WriteString("END:VCALENDAR\r\n")

	return foldLines(b.String())
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

// parseLine splits an ICS line into name, params, and value respecting quoted strings (RFC 5545 §3.1).
func parseLine(line string) (name string, params map[string]string, value string) {
	params = make(map[string]string)

	inQuote := false
	colonIdx := -1
	for i := 0; i < len(line); i++ {
		if line[i] == '"' {
			inQuote = !inQuote
		} else if line[i] == ':' && !inQuote {
			colonIdx = i
			break
		}
	}

	if colonIdx == -1 {
		return line, params, ""
	}

	prefix := line[:colonIdx]
	value = line[colonIdx+1:]

	semicolonIdx := -1
	inQuote = false
	for i := 0; i < len(prefix); i++ {
		if prefix[i] == '"' {
			inQuote = !inQuote
		} else if prefix[i] == ';' && !inQuote {
			semicolonIdx = i
			break
		}
	}

	if semicolonIdx == -1 {
		name = prefix
	} else {
		name = prefix[:semicolonIdx]
		paramStr := prefix[semicolonIdx+1:]

		var paramParts []string
		start := 0
		inQuote = false
		for i := 0; i < len(paramStr); i++ {
			if paramStr[i] == '"' {
				inQuote = !inQuote
			} else if paramStr[i] == ';' && !inQuote {
				paramParts = append(paramParts, paramStr[start:i])
				start = i + 1
			}
		}
		paramParts = append(paramParts, paramStr[start:])

		for _, p := range paramParts {
			eqIdx := strings.Index(p, "=")
			if eqIdx != -1 {
				params[strings.ToUpper(p[:eqIdx])] = strings.Trim(p[eqIdx+1:], "\"")
			}
		}
	}

	return name, params, value
}

// ParseDuration parses an RFC 5545 / ISO 8601 duration (e.g. PT1H, P1D) into a time.Duration.
// It returns 0 when the value is not a valid duration.
func ParseDuration(s string) time.Duration {
	return parseDurationString(s)
}

// parseDurationString parses an ISO 8601 duration string into time.Duration.
func parseDurationString(durStr string) time.Duration {
	durStr = strings.ToUpper(durStr)
	if !strings.HasPrefix(durStr, "P") {
		return 0
	}
	durStr = durStr[1:]

	var total time.Duration
	inTime := false
	num := 0

	for _, c := range durStr {
		if c == 'T' {
			inTime = true
			continue
		}
		if c >= '0' && c <= '9' {
			num = num*10 + int(c-'0')
			continue
		}
		switch c {
		case 'D':
			total += time.Duration(num) * 24 * time.Hour
		case 'W':
			total += time.Duration(num) * 7 * 24 * time.Hour
		case 'H':
			if inTime {
				total += time.Duration(num) * time.Hour
			}
		case 'M':
			if inTime {
				total += time.Duration(num) * time.Minute
			}
		case 'S':
			if inTime {
				total += time.Duration(num) * time.Second
			}
		}
		num = 0
	}
	return total
}

// windowsTZ maps common Windows time zone names to IANA names.
var windowsTZ = map[string]string{
	"Dateline Standard Time":          "Etc/GMT+12",
	"UTC-11":                          "Etc/GMT+11",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Alaskan Standard Time":           "America/Anchorage",
	"Pacific Standard Time (Mexico)":  "America/Tijuana",
	"Pacific Standard Time":           "America/Los_Angeles",
	"US Mountain Standard Time":       "America/Phoenix",
	"Mountain Standard Time (Mexico)": "America/Mazatlan",
	"Mountain Standard Time":          "America/Denver",
	"Central America Standard Time":   "America/Guatemala",
	"Central Standard Time":           "America/Chicago",
	"Central Standard Time (Mexico)":  "America/Mexico_City",
	"Canada Central Standard Time":    "America/Regina",
	"SA Pacific Standard Time":        "America/Bogota",
	"Eastern Standard Time":           "America/New_York",
	"US Eastern Standard Time":        "America/Indiana/Indianapolis",
	"Venezuela Standard Time":         "America/Caracas",
	"Atlantic Standard Time":          "America/Halifax",
	"SA Western Standard Time":        "America/La_Paz",
	"Pacific SA Standard Time":        "America/Santiago",
	"Newfoundland Standard Time":      "America/St_Johns",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"Argentina Standard Time":         "America/Argentina/Buenos_Aires",
	"SA Eastern Standard Time":        "America/Cayenne",
	"Montevideo Standard Time":        "America/Montevideo",
	"Paraguay Standard Time":          "America/Asuncion",
	"UTC-02":                          "Etc/GMT+2",
	"Azores Standard Time":            "Atlantic/Azores",
	"Cape Verde Standard Time":        "Atlantic/Cape_Verde",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"Morocco Standard Time":           "Africa/Casablanca",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Romance Standard Time":           "Europe/Paris",
	"Central European Standard Time":  "Europe/Warsaw",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"Namibia Standard Time":           "Africa/Windhoek",
	"GTB Standard Time":               "Europe/Bucharest",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"FLE Standard Time":               "Europe/Kiev",
	"Middle East Standard Time":       "Asia/Beirut",
	"Egypt Standard Time":             "Africa/Cairo",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"Israel Standard Time":            "Asia/Jerusalem",
	"Kaliningrad Standard Time":       "Europe/Kaliningrad",
	"Jordan Standard Time":            "Asia/Amman",
	"Turkey Standard Time":            "Europe/Istanbul",
	"Arab Standard Time":              "Asia/Riyadh",
	"Arabic Standard Time":            "Asia/Baghdad",
	"Belarus Standard Time":           "Europe/Minsk",
	"Russian Standard Time":           "Europe/Moscow",
	"E. Africa Standard Time":         "Africa/Nairobi",
	"Iran Standard Time":              "Asia/Tehran",
	"Arabian Standard Time":           "Asia/Dubai",
	"Azerbaijan Standard Time":        "Asia/Baku",
	"Georgian Standard Time":          "Asia/Tbilisi",
	"Caucasus Standard Time":          "Asia/Yerevan",
	"Afghanistan Standard Time":       "Asia/Kabul",
	"West Asia Standard Time":         "Asia/Tashkent",
	"Ekaterinburg Standard Time":      "Asia/Yekaterinburg",
	"Pakistan Standard Time":          "Asia/Karachi",
	"India Standard Time":             "Asia/Kolkata",
	"Sri Lanka Standard Time":         "Asia/Colombo",
	"Nepal Standard Time":             "Asia/Kathmandu",
	"Central Asia Standard Time":      "Asia/Almaty",
	"Bangladesh Standard Time":        "Asia/Dhaka",
	"Myanmar Standard Time":           "Asia/Yangon",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"N. Central Asia Standard Time":   "Asia/Novosibirsk",
	"North Asia Standard Time":        "Asia/Krasnoyarsk",
	"China Standard Time":             "Asia/Shanghai",
	"North Asia East Standard Time":   "Asia/Irkutsk",
	"Singapore Standard Time":         "Asia/Singapore",
	"W. Australia Standard Time":      "Australia/Perth",
	"Taipei Standard Time":            "Asia/Taipei",
	"Ulaanbaatar Standard Time":       "Asia/Ulaanbaatar",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"Korea Standard Time":             "Asia/Seoul",
	"Yakutsk Standard Time":           "Asia/Yakutsk",
	"Cen. Australia Standard Time":    "Australia/Adelaide",
	"AUS Central Standard Time":       "Australia/Darwin",
	"E. Australia Standard Time":      "Australia/Brisbane",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"West Pacific Standard Time":      "Pacific/Port_Moresby",
	"Tasmania Standard Time":          "Australia/Hobart",
	"Vladivostok Standard Time":       "Asia/Vladivostok",
	"UTC+12":                          "Etc/GMT-12",
	"New Zealand Standard Time":       "Pacific/Auckland",
	"Fiji Standard Time":              "Pacific/Fiji",
	"Tonga Standard Time":             "Pacific/Tongatapu",
	"Samoa Standard Time":             "Pacific/Apia",
	"Line Islands Standard Time":      "Pacific/Kiritimati",
}

// ResolveTZID resolves an iCalendar TZID value (IANA or common Windows name) to a location.
// Vendor-prefixed names such as "/mozilla.org/20050126_1/America/New_York" resolve to their
// trailing IANA name.
func ResolveTZID(tzid string) (*time.Location, bool) {
	tzid = strings.Trim(strings.TrimSpace(tzid), "\"")
	tzid = strings.TrimPrefix(tzid, "/")
	if tzid == "" {
		return nil, false
	}
	if mapped, ok := windowsTZ[tzid]; ok {
		tzid = mapped
	}
	loc, err := time.LoadLocation(tzid)
	if err == nil {
		return loc, true
	}
	// Try ever shorter "/"-separated suffixes, longest first.
	for i := strings.Index(tzid, "/"); i >= 0; {
		tzid = tzid[i+1:]
		if loc, err := time.LoadLocation(tzid); err == nil && strings.Contains(tzid, "/") {
			return loc, true
		}
		i = strings.Index(tzid, "/")
	}
	return nil, false
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
			if loc, ok := ResolveTZID(tzid); ok {
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

	return time.Time{}, false // not a date or date-time: callers treat the zero time as invalid
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
		// A quoted parameter value may not contain DQUOTE, CR or LF (RFC 5545 §3.1).
		cn := strings.Map(func(r rune) rune {
			if r == '"' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, a.DisplayName)
		parts = append(parts, fmt.Sprintf("CN=\"%s\"", cn))
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
	return fmt.Sprintf("ATTENDEE;%s:mailto:%s\r\n", paramStr, stripLineBreaks(a.Email))
}

// stripLineBreaks removes CR and LF from a non-TEXT value (a URI or cal-address) so it cannot
// break out of its content line.
func stripLineBreaks(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

// foldLines folds every CRLF-terminated content line longer than 75 octets (RFC 5545 §3.1),
// never splitting a UTF-8 sequence.
func foldLines(s string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(s, "\r\n") {
		body := strings.TrimSuffix(line, "\r\n")
		eol := line[len(body):]
		limit := 75
		for len(body) > limit {
			cut := limit
			for cut > 0 && !utf8.RuneStart(body[cut]) {
				cut--
			}
			b.WriteString(body[:cut])
			b.WriteString("\r\n ")
			body = body[cut:]
			limit = 74 // continuation lines start with a space
		}
		b.WriteString(body)
		b.WriteString(eol)
	}
	return b.String()
}

// escapeICS escapes special characters for ICS text.
func escapeICS(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, ";", "\\;")
	s = strings.ReplaceAll(s, ",", "\\,")
	return s
}

// unescapeICS reverses ICS escaping in a single left-to-right pass, so "\\n" is a backslash
// followed by "n", not a newline.
func unescapeICS(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n', 'N':
			b.WriteByte('\n')
		case '\\', ';', ',':
			b.WriteByte(s[i])
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// splitTextList splits a multi-valued TEXT property (e.g. CATEGORIES) on unescaped commas and
// unescapes each value.
func splitTextList(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == ',' {
			out = append(out, unescapeICS(s[start:i]))
			start = i + 1
		}
	}
	return append(out, unescapeICS(s[start:]))
}

// ErrUnsupportedComponent is returned by ValidateICS for a calendar object whose component type
// is not stored (VJOURNAL): CalDAV answers it with the supported-calendar-component precondition.
var ErrUnsupportedComponent = errors.New("unsupported calendar component (only VEVENT and VTODO are supported)")

// ValidateICS performs basic validation of an ICS string.
func ValidateICS(raw string) error {
	if !strings.Contains(raw, "BEGIN:VCALENDAR") {
		return fmt.Errorf("missing BEGIN:VCALENDAR")
	}
	if !strings.Contains(raw, "END:VCALENDAR") {
		return fmt.Errorf("missing END:VCALENDAR")
	}
	if strings.Contains(raw, "BEGIN:VJOURNAL") {
		return ErrUnsupportedComponent
	}
	if !strings.Contains(raw, "BEGIN:VEVENT") && !strings.Contains(raw, "BEGIN:VTODO") {
		return fmt.Errorf("no calendar component found (VEVENT/VTODO)")
	}
	if strings.Count(raw, "\nATTENDEE") > models.MaxAttendeesPerEvent {
		return fmt.Errorf("event exceeds maximum limit of %d attendees", models.MaxAttendeesPerEvent)
	}
	return nil
}

// FreeBusyRequest is a parsed iTIP VFREEBUSY request (RFC 6638 §5).
type FreeBusyRequest struct {
	Organizer string
	Attendees []string
	Start     time.Time
	End       time.Time
}

// ParseFreeBusyRequest extracts organizer, attendees and the requested window from a
// METHOD:REQUEST calendar object containing a VFREEBUSY component.
func ParseFreeBusyRequest(raw string) (*FreeBusyRequest, error) {
	if !strings.Contains(raw, "BEGIN:VFREEBUSY") {
		return nil, fmt.Errorf("no VFREEBUSY component")
	}
	req := &FreeBusyRequest{}
	in := false
	for _, line := range unfoldICS(raw) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "BEGIN:VFREEBUSY" {
			in = true
			continue
		}
		if trimmed == "END:VFREEBUSY" {
			in = false
			continue
		}
		if !in {
			continue
		}
		name, params, value := parseLine(line)
		switch strings.ToUpper(name) {
		case "ORGANIZER":
			req.Organizer = extractCalAddress(value)
		case "ATTENDEE":
			if a := extractCalAddress(value); a != "" {
				req.Attendees = append(req.Attendees, a)
			}
		case "DTSTART":
			req.Start, _ = parseDateTime(value, params)
		case "DTEND":
			req.End, _ = parseDateTime(value, params)
		}
	}
	if req.Start.IsZero() || req.End.IsZero() || !req.End.After(req.Start) {
		return nil, fmt.Errorf("VFREEBUSY needs DTSTART and a later DTEND")
	}
	if len(req.Attendees) == 0 {
		return nil, fmt.Errorf("VFREEBUSY has no ATTENDEE")
	}
	return req, nil
}

// SchedulingReply holds the parsed data from a METHOD:REPLY ICS.
type SchedulingReply struct {
	Method        string // REPLY, CANCEL, REQUEST, etc.
	EventUID      string
	AttendeeEmail string
	Partstat      string // ACCEPTED, DECLINED, TENTATIVE
	Summary       string
	Comment       string // optional reply comment
}

// ParseSchedulingICS parses an iMIP ICS (METHOD:REPLY, METHOD:CANCEL, etc.)
// and extracts the event UID, attendee email, and participation status.
func ParseSchedulingICS(rawICS string) (*SchedulingReply, error) {
	if !strings.Contains(rawICS, "BEGIN:VCALENDAR") {
		return nil, fmt.Errorf("invalid ICS: missing BEGIN:VCALENDAR")
	}

	reply := &SchedulingReply{}
	lines := unfoldICS(rawICS)

	var inVEvent bool

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if trimmed == "BEGIN:VEVENT" {
			inVEvent = true
			continue
		}
		if trimmed == "END:VEVENT" {
			inVEvent = false
			continue
		}

		name, params, value := parseLine(line)
		upperName := strings.ToUpper(name)

		// Calendar-level METHOD
		if !inVEvent && upperName == "METHOD" {
			reply.Method = strings.ToUpper(strings.TrimSpace(value))
			continue
		}

		if !inVEvent {
			continue
		}

		switch upperName {
		case "UID":
			reply.EventUID = value
		case "SUMMARY":
			reply.Summary = unescapeICS(value)
		case "COMMENT":
			reply.Comment = unescapeICS(value)
		case "ATTENDEE":
			// In a REPLY, there's typically one ATTENDEE — the one responding
			email := extractCalAddress(value)
			partstat := "NEEDS-ACTION"
			if ps, ok := params["PARTSTAT"]; ok {
				partstat = strings.ToUpper(ps)
			}
			// Only take the first attendee (the replier)
			if reply.AttendeeEmail == "" {
				reply.AttendeeEmail = email
				reply.Partstat = partstat
			}
		}
	}

	if reply.EventUID == "" {
		return nil, fmt.Errorf("no UID found in scheduling ICS")
	}
	if reply.Method == "" {
		return nil, fmt.Errorf("no METHOD found in scheduling ICS")
	}

	return reply, nil
}
