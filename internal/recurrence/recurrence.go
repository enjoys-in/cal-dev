// Package recurrence expands iCalendar recurring events (RRULE/RDATE/EXDATE)
// into concrete occurrences inside a time window.
package recurrence

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/teambition/rrule-go"
)

// Occurrence is one concrete instance of an event.
type Occurrence struct {
	Start time.Time
	End   time.Time
}

// DefaultLimit caps the occurrences returned for a single event.
const DefaultLimit = 1000

type eventProps struct {
	ExDates []string `json:"exdates"`
	RDates  []string `json:"rdates"`
	TZID    string   `json:"tzid"`
}

// Duration returns the length of a single occurrence of e.
// All-day events without an explicit end last one day; timed events without
// an end or duration have zero length (RFC 5545 §3.6.1).
func Duration(e *models.Event) time.Duration {
	if e.DTEnd != nil && e.DTEnd.After(e.DTStart) {
		return e.DTEnd.Sub(e.DTStart)
	}
	if e.Duration != "" {
		if d := ics.ParseDuration(e.Duration); d > 0 {
			return d
		}
	}
	if e.AllDay {
		return 24 * time.Hour
	}
	return 0
}

// Expand returns the occurrences of e that overlap [from, to), ordered by start.
// Non-recurring events yield at most one occurrence. limit <= 0 means DefaultLimit.
func Expand(e *models.Event, from, to time.Time, limit int) []Occurrence {
	if limit <= 0 {
		limit = DefaultLimit
	}
	dur := Duration(e)

	if strings.TrimSpace(e.RecurrenceRule) == "" && !hasRDates(e) {
		if overlaps(e.DTStart, dur, from, to) {
			return []Occurrence{{Start: e.DTStart, End: e.DTStart.Add(dur)}}
		}
		return nil
	}

	props := loadProps(e)
	loc := time.UTC
	if !e.AllDay && props.TZID != "" {
		if l, ok := ics.ResolveTZID(props.TZID); ok {
			loc = l
		}
	}

	excluded := make(map[int64]bool, len(props.ExDates))
	for _, raw := range props.ExDates {
		if t, ok := parseICSTime(raw, loc); ok {
			excluded[t.Unix()] = true
		}
	}

	var starts []time.Time
	if rule := strings.TrimSpace(e.RecurrenceRule); rule != "" {
		// Ask for extra starts so EXDATE removals can't leave us short of `limit`.
		starts = append(starts, ruleStarts(e, rule, loc, from.Add(-dur), to, limit+len(excluded))...)
	} else {
		starts = append(starts, e.DTStart)
	}
	for _, raw := range props.RDates {
		if t, ok := parseICSTime(raw, loc); ok {
			starts = append(starts, t)
		}
	}

	seen := make(map[int64]bool, len(starts))
	var out []Occurrence
	for _, s := range starts {
		key := s.Unix()
		if seen[key] || excluded[key] {
			continue
		}
		seen[key] = true
		if !overlaps(s, dur, from, to) {
			continue
		}
		out = append(out, Occurrence{Start: s, End: s.Add(dur)})
	}
	sortOccurrences(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// HasOccurrenceIn reports whether e has at least one occurrence overlapping [from, to).
func HasOccurrenceIn(e *models.Event, from, to time.Time) bool {
	return len(Expand(e, from, to, 1)) > 0
}

// ruleStarts returns up to max start times of the series inside [after, before].
// It iterates lazily so unbounded rules never materialise more than needed.
func ruleStarts(e *models.Event, rule string, loc *time.Location, after, before time.Time, max int) []time.Time {
	rule = strings.TrimPrefix(strings.TrimSpace(rule), "RRULE:")
	opt, err := rrule.StrToROptionInLocation(rule, loc)
	if err != nil {
		// Unparseable rule: fall back to the master instance only.
		return []time.Time{e.DTStart}
	}
	switch opt.Freq {
	case rrule.SECONDLY, rrule.MINUTELY:
		// Refuse sub-hourly recurrence: expansion cost is unbounded for old series.
		return []time.Time{e.DTStart}
	}
	dtstart := e.DTStart.In(loc)
	if e.AllDay {
		dtstart = time.Date(e.DTStart.Year(), e.DTStart.Month(), e.DTStart.Day(), 0, 0, 0, 0, time.UTC)
	}
	opt.Dtstart = dtstart
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return []time.Time{e.DTStart}
	}
	var out []time.Time
	next := r.Iterator()
	for len(out) < max {
		t, ok := next()
		if !ok || t.After(before) {
			break
		}
		if t.Before(after) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func overlaps(start time.Time, dur time.Duration, from, to time.Time) bool {
	if dur <= 0 {
		return !start.Before(from) && start.Before(to)
	}
	return start.Before(to) && start.Add(dur).After(from)
}

func hasRDates(e *models.Event) bool {
	return len(loadProps(e).RDates) > 0
}

func loadProps(e *models.Event) eventProps {
	var p eventProps
	if len(e.Properties) > 0 && string(e.Properties) != "{}" {
		_ = json.Unmarshal(e.Properties, &p)
	}
	return p
}

// parseICSTime parses an EXDATE/RDATE value: UTC ("...Z"), floating/zoned, or date-only.
func parseICSTime(v string, loc *time.Location) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(ics.ICSDateTimeUTC, v); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation(ics.ICSDateTimeLocal, v, loc); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation(ics.ICSDateOnly, v, time.UTC); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func sortOccurrences(o []Occurrence) {
	sort.Slice(o, func(i, j int) bool { return o[i].Start.Before(o[j].Start) })
}
