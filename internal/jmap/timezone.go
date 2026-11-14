package jmap

import (
	"context"
	"fmt"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
)

// calendarTimeZone returns the stored time zone of one of the caller's calendars ("" if unknown).
func (h *Handler) calendarTimeZone(ctx context.Context, tenantID int, calendarID string) string {
	cal, err := h.store.GetCalendar(ctx, tenantID, calendarID)
	if err != nil || cal == nil {
		return ""
	}
	return cal.Timezone
}

// utcZoneName is what a timed event anchored to plain UTC reports as its `timeZone`.
const utcZoneName = "Etc/UTC"

// timeZoneJMAP is the `timeZone` JSON value for an event: the IANA zone its wall-clock times
// belong to ("Etc/UTC" for a fixed UTC time). All-day events have no zone, so it returns nil.
func timeZoneJMAP(evt *models.Event) (string, bool) {
	if evt.AllDay {
		return "", false
	}
	if tz := ics.EventTZID(evt); tz != "" {
		return tz, true
	}
	return utcZoneName, true
}

// validateTimeZone checks the `timeZone` property of a create/update object, if present.
// It must be a known IANA zone name (Windows names are accepted), "" or null (both mean UTC).
func validateTimeZone(props map[string]interface{}) error {
	raw, present := props["timeZone"]
	if !present || raw == nil {
		return nil
	}
	name, ok := raw.(string)
	if !ok {
		return fmt.Errorf("timeZone must be a string or null")
	}
	if _, ok := ics.NormalizeTZID(name); !ok {
		return fmt.Errorf("unknown timeZone %q (use an IANA name such as America/New_York)", name)
	}
	return nil
}

// applyTimeZone anchors evt to the zone given in props["timeZone"].
//
// When the client sent no timeZone and the event recurs, the event is anchored to the zone of
// its calendar instead (calendarZone is only called then), so a weekly 09:00 meeting stays at
// 09:00 across daylight-saving changes. useFallback is false for updates that don't touch the
// recurrence, so an unrelated edit never silently changes how an existing series repeats.
// A calendar zone of UTC (the default) changes nothing.
func applyTimeZone(evt *models.Event, props map[string]interface{}, useFallback bool, calendarZone func() string) error {
	if err := validateTimeZone(props); err != nil {
		return err
	}
	if raw, present := props["timeZone"]; present {
		name, _ := raw.(string) // null and "" clear the zone
		return ics.SetEventTZID(evt, name)
	}
	if useFallback && !evt.AllDay && evt.RecurrenceRule != "" && ics.EventTZID(evt) == "" && calendarZone != nil {
		if zone := calendarZone(); zone != "" {
			// An unknown calendar zone is ignored: the event simply stays in UTC.
			_ = ics.SetEventTZID(evt, zone)
		}
	}
	return nil
}
