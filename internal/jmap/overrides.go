package jmap

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
	"github.com/google/uuid"
)

// Recurring events are exposed JSCalendar-style: one object per series, with each changed or
// cancelled occurrence in `recurrenceOverrides`, keyed by the ORIGINAL start of that occurrence
// (RFC 3339 UTC for timed series, YYYY-MM-DD for all-day series). A value is a patch:
//
//	{"excluded": true}                       this occurrence is cancelled
//	{"start": "...", "end": "...",           this occurrence was moved / retitled (only changed
//	 "title": "...", "location": "...", …}   properties are listed)
//
// Writing an override key with null removes the override (the occurrence reverts to the series).

// overrideKeyToID converts a JMAP override key to the stored RECURRENCE-ID key.
func overrideKeyToID(key string, allDay bool) (string, bool) {
	key = strings.TrimSpace(key)
	if allDay {
		t, err := time.Parse("2006-01-02", key)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, key); err != nil {
				return "", false
			}
		}
		return t.UTC().Format(ics.ICSDateOnly), true
	}
	t, err := time.Parse(time.RFC3339, key)
	if err != nil {
		return "", false
	}
	return t.UTC().Format(ics.ICSDateTimeUTC), true
}

// overrideIDToKey is the inverse of overrideKeyToID. It returns the original occurrence start too.
func overrideIDToKey(id string) (string, time.Time, bool) {
	if t, err := time.Parse(ics.ICSDateOnly, id); err == nil && len(id) == 8 {
		return t.Format("2006-01-02"), t, true
	}
	if t, err := time.Parse(ics.ICSDateTimeUTC, id); err == nil {
		return t.UTC().Format(time.RFC3339), t.UTC(), true
	}
	return "", time.Time{}, false
}

// overridePatch describes how an override row differs from its master.
func overridePatch(master, o *models.Event) map[string]interface{} {
	if strings.EqualFold(o.Status, "CANCELLED") {
		return map[string]interface{}{"excluded": true}
	}
	patch := map[string]interface{}{}
	if !o.DTStart.Equal(originalStart(master, o)) {
		patch["start"] = o.DTStart.UTC().Format(time.RFC3339)
	}
	if recurrence.Duration(o) != recurrence.Duration(master) {
		patch["end"] = o.DTStart.Add(recurrence.Duration(o)).UTC().Format(time.RFC3339)
	}
	if o.Summary != master.Summary {
		patch["title"] = o.Summary
	}
	if o.Description != master.Description {
		patch["description"] = o.Description
	}
	if o.Location != master.Location {
		patch["location"] = o.Location
	}
	if !strings.EqualFold(o.Status, master.Status) {
		patch["status"] = o.Status
	}
	return patch
}

// originalStart is where the overridden occurrence sat before it was changed.
func originalStart(master, o *models.Event) time.Time {
	if _, t, ok := overrideIDToKey(o.RecurrenceID); ok {
		return t
	}
	return o.DTStart
}

// foldedEvent is a series master (or standalone event) with its overrides.
type foldedEvent struct {
	event     *models.Event
	overrides []*models.Event
}

// foldResources turns the flat output of store.QueryResources into one entry per series.
func foldResources(events []*models.Event) []foldedEvent {
	groups := recurrence.Group(events)
	out := make([]foldedEvent, 0, len(groups))
	for _, g := range groups {
		out = append(out, foldedEvent{event: g.Master, overrides: g.Overrides})
	}
	return out
}

func recurrenceOverridesJMAP(master *models.Event, overrides []*models.Event) map[string]interface{} {
	if len(overrides) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(overrides))
	for _, o := range overrides {
		key, _, ok := overrideIDToKey(o.RecurrenceID)
		if !ok {
			continue
		}
		out[key] = overridePatch(master, o)
	}
	return out
}

// newOverrideRow creates the override row for the occurrence identified by id, starting as a
// copy of the master with the series-level properties removed.
func newOverrideRow(master *models.Event, id string) (*models.Event, error) {
	_, orig, ok := overrideIDToKey(id)
	if !ok {
		return nil, fmt.Errorf("invalid recurrence id %q", id)
	}
	o := *master
	o.ID = uuid.New().String()
	o.RecurrenceID = id
	o.RecurrenceRule = ""
	o.DTStart = orig
	dur := recurrence.Duration(master)
	if dur > 0 {
		end := orig.Add(dur)
		o.DTEnd = &end
	} else {
		o.DTEnd = nil
	}
	o.Duration = ""
	o.Sequence = master.Sequence
	o.Properties = overrideProperties(master.Properties)
	o.RawICS = ""
	return &o, nil
}

// overrideProperties keeps the master's alarms/timezone but drops series-only data (EXDATE/RDATE).
func overrideProperties(raw json.RawMessage) json.RawMessage {
	props := map[string]interface{}{}
	_ = json.Unmarshal(raw, &props)
	delete(props, "exdates")
	delete(props, "rdates")
	b, err := json.Marshal(props)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// applyOverridePatches applies a `recurrenceOverrides` update map to the resource's rows.
// rows[0] must be the master. It returns the new full row list (master first).
func applyOverridePatches(rows []*models.Event, patches map[string]interface{}) ([]*models.Event, error) {
	master := rows[0]
	byID := map[string]*models.Event{}
	for _, r := range rows[1:] {
		byID[r.RecurrenceID] = r
	}

	for key, raw := range patches {
		id, ok := overrideKeyToID(key, master.AllDay)
		if !ok {
			return nil, fmt.Errorf("invalid recurrenceOverrides key %q (use an RFC 3339 UTC time, or YYYY-MM-DD for all-day series)", key)
		}
		if raw == nil {
			delete(byID, id) // null: back to the series' own occurrence
			continue
		}
		patch, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("recurrenceOverrides[%q] must be an object or null", key)
		}
		row := byID[id]
		if row == nil {
			var err error
			if row, err = newOverrideRow(master, id); err != nil {
				return nil, err
			}
			byID[id] = row
		}
		if excluded, _ := patch["excluded"].(bool); excluded {
			row.Status = "CANCELLED"
			continue
		}
		if cancelled := strings.EqualFold(row.Status, "CANCELLED"); cancelled && patch["excluded"] != nil {
			row.Status = master.Status // "excluded": false revives it
		}
		if v, ok := patch["title"].(string); ok {
			row.Summary = v
		}
		if v, ok := patch["description"].(string); ok {
			row.Description = v
		}
		if v, ok := patch["location"].(string); ok {
			row.Location = v
		}
		if v, ok := patch["status"].(string); ok && v != "" {
			row.Status = strings.ToUpper(v)
		}
		dur := recurrence.Duration(row)
		if v, ok := patch["start"].(string); ok {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("recurrenceOverrides[%q].start must be RFC 3339", key)
			}
			row.DTStart = t
		}
		if v, ok := patch["end"].(string); ok {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil || !t.After(row.DTStart) {
				return nil, fmt.Errorf("recurrenceOverrides[%q].end must be RFC 3339 and after the start", key)
			}
			row.DTEnd = &t
		} else if dur > 0 {
			end := row.DTStart.Add(dur) // moved without an explicit end: keep the length
			row.DTEnd = &end
		}
	}

	out := make([]*models.Event, 0, 1+len(byID))
	out = append(out, master)
	for _, r := range byID {
		out = append(out, r)
	}
	return out, nil
}
