package ics

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
)

// propTZID is the key in Event.Properties that records the IANA zone an event's wall-clock
// times are expressed in. ParseICSAll sets it from DTSTART;TZID=..., the recurrence expander
// reads it, and the writers below use it to emit TZID instead of a fixed UTC time.
const propTZID = "tzid"

// isUTCName reports whether a zone name means plain UTC (which is written with a "Z" suffix).
func isUTCName(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "", "UTC", "ETC/UTC", "ETC/GMT", "GMT", "Z", "ZULU", "ETC/ZULU":
		return true
	}
	return false
}

// NormalizeTZID resolves a zone name (IANA or common Windows name) to its canonical IANA name.
// UTC and its aliases normalize to "" because UTC needs no TZID. ok is false for unknown zones.
func NormalizeTZID(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if isUTCName(name) {
		return "", true
	}
	loc, ok := ResolveTZID(name)
	if !ok {
		return "", false
	}
	if isUTCName(loc.String()) {
		return "", true
	}
	return loc.String(), true
}

// EventTZID returns the IANA zone the event is anchored to, or "" for UTC, floating and all-day events.
func EventTZID(e *models.Event) string {
	if e == nil || e.AllDay || len(e.Properties) == 0 {
		return ""
	}
	var props map[string]interface{}
	if err := json.Unmarshal(e.Properties, &props); err != nil {
		return ""
	}
	name, _ := props[propTZID].(string)
	name, _ = NormalizeTZID(name)
	return name
}

// SetEventTZID anchors the event to an IANA zone, keeping the rest of its properties.
// An empty name (or UTC) clears the zone. It returns an error for an unknown zone.
//
// Changing the zone keeps every EXDATE/RDATE pointing at the same instant: values stored as
// local wall-clock time (which only mean something relative to the old zone) are pinned to UTC.
func SetEventTZID(e *models.Event, name string) error {
	canonical, ok := NormalizeTZID(name)
	if !ok {
		return fmt.Errorf("unknown time zone %q", name)
	}
	previous := EventTZID(e)
	props := map[string]interface{}{}
	if len(e.Properties) > 0 {
		_ = json.Unmarshal(e.Properties, &props)
		if props == nil {
			props = map[string]interface{}{}
		}
	}
	if canonical != previous {
		oldLoc := time.UTC
		if previous != "" {
			if l, ok := ResolveTZID(previous); ok {
				oldLoc = l
			}
		}
		for _, key := range []string{"exdates", "rdates"} {
			list, _ := props[key].([]interface{})
			for i, v := range list {
				if s, ok := v.(string); ok {
					if t, err := time.ParseInLocation(ICSDateTimeLocal, s, oldLoc); err == nil {
						list[i] = t.UTC().Format(ICSDateTimeUTC)
					}
				}
			}
		}
	}
	if canonical == "" {
		delete(props, propTZID)
	} else {
		props[propTZID] = canonical
	}
	b, err := json.Marshal(props)
	if err != nil {
		return err
	}
	e.Properties = b
	return nil
}

// eventLocation returns the zone to write wall-clock times in, or nil for UTC output.
func eventLocation(e *models.Event) *time.Location {
	name := EventTZID(e)
	if name == "" {
		return nil
	}
	loc, ok := ResolveTZID(name)
	if !ok {
		return nil
	}
	return loc
}

// writeDateTime writes a date-time property: "NAME;TZID=Zone:local" when loc is set,
// otherwise "NAME:...Z" in UTC.
func writeDateTime(b *strings.Builder, name string, t time.Time, loc *time.Location) {
	if loc == nil {
		b.WriteString(fmt.Sprintf("%s:%s\r\n", name, t.UTC().Format(ICSDateTimeUTC)))
		return
	}
	b.WriteString(fmt.Sprintf("%s;TZID=%s:%s\r\n", name, loc.String(), t.In(loc).Format(ICSDateTimeLocal)))
}

// writeDateValue writes one stored EXDATE/RDATE value. Values are kept as the client sent them
// minus the TZID parameter, so a local value ("20261116T090000") gets the series' TZID back;
// UTC ("...Z") and date-only values are written unchanged.
func writeDateValue(b *strings.Builder, name, value string, loc *time.Location) {
	if loc != nil {
		if _, err := time.Parse(ICSDateTimeLocal, value); err == nil {
			b.WriteString(fmt.Sprintf("%s;TZID=%s:%s\r\n", name, loc.String(), value))
			return
		}
	}
	b.WriteString(fmt.Sprintf("%s:%s\r\n", name, value))
}
