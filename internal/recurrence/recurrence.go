// Package recurrence expands iCalendar recurring events (RRULE/RDATE/EXDATE)
// into concrete occurrences inside a time window.
package recurrence

import (
	"encoding/json"
	"fmt"
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
	// Event is the component that defines this instance: the master series, or the
	// RECURRENCE-ID override that replaced it. Callers read status, transparency, title
	// and attendee state from here, not from the master.
	Event *models.Event
}

// DefaultLimit caps the occurrences returned for a single event.
const DefaultLimit = 1000

// maxRuleIterations caps the candidate starts read from one RRULE per expansion (including the
// ones before the window), so old or pathological series cannot iterate for ever.
const maxRuleIterations = 100000

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
			return []Occurrence{{Start: e.DTStart, End: e.DTStart.Add(dur), Event: e}}
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
		out = append(out, Occurrence{Start: s, End: s.Add(dur), Event: e})
	}
	sortOccurrences(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Series is a master event together with its RECURRENCE-ID overrides. A resource that has
// overrides but no master (e.g. an invitation to a single instance) is represented as one
// Series per override, each standing alone.
type Series struct {
	Master    *models.Event
	Overrides []*models.Event
}

// Group collects rows into series by (calendar, uid), in order of first appearance.
func Group(events []*models.Event) []Series {
	type bucket struct {
		master    *models.Event
		overrides []*models.Event
	}
	buckets := map[string]*bucket{}
	var order []string
	for _, e := range events {
		key := e.CalendarID + "\x00" + e.UID
		if e.UID == "" {
			key = fmt.Sprintf("%p", e) // no UID: cannot be part of a series, never group
		}
		b := buckets[key]
		if b == nil {
			b = &bucket{}
			buckets[key] = b
			order = append(order, key)
		}
		if e.RecurrenceID == "" {
			b.master = e
		} else {
			b.overrides = append(b.overrides, e)
		}
	}
	var out []Series
	for _, key := range order {
		b := buckets[key]
		if b.master != nil {
			out = append(out, Series{Master: b.master, Overrides: b.overrides})
			continue
		}
		for _, o := range b.overrides {
			out = append(out, Series{Master: o})
		}
	}
	return out
}

// occurrenceKey is the RECURRENCE-ID-style key of a master occurrence starting at t.
func occurrenceKey(master *models.Event, t time.Time) string {
	if master.AllDay {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Format(ics.ICSDateOnly)
	}
	return t.UTC().Format(ics.ICSDateTimeUTC)
}

// Expand returns the series' occurrences overlapping [from, to), ordered by start, with
// overrides applied: an overridden occurrence is replaced by the override (which may have been
// moved or retimed, even into or out of the window), and a CANCELLED override removes it.
func (s Series) Expand(from, to time.Time, limit int) []Occurrence {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(s.Overrides) == 0 {
		return Expand(s.Master, from, to, limit)
	}

	replaced := make(map[string]bool, len(s.Overrides))
	for _, o := range s.Overrides {
		replaced[o.RecurrenceID] = true
	}

	var out []Occurrence
	for _, oc := range Expand(s.Master, from, to, limit+len(s.Overrides)) {
		if !replaced[occurrenceKey(s.Master, oc.Start)] {
			out = append(out, oc)
		}
	}
	for _, o := range s.Overrides {
		if strings.EqualFold(o.Status, "CANCELLED") {
			continue
		}
		dur := Duration(o)
		if dur == 0 && o.DTEnd == nil && o.Duration == "" {
			dur = Duration(s.Master) // an override without DTEND/DURATION keeps the series' length
		}
		if overlaps(o.DTStart, dur, from, to) {
			out = append(out, Occurrence{Start: o.DTStart, End: o.DTStart.Add(dur), Event: o})
		}
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
	switch {
	case opt.Freq == rrule.SECONDLY, opt.Freq == rrule.MINUTELY,
		opt.Freq == rrule.HOURLY && len(opt.Byminute) > 1, len(opt.Bysecond) > 1:
		// Refuse sub-hourly recurrence (also via BYMINUTE/BYSECOND lists): expansion cost is
		// unbounded for old series.
		return []time.Time{e.DTStart}
	}
	dtstart := e.DTStart.In(loc)
	if e.AllDay {
		dtstart = time.Date(e.DTStart.Year(), e.DTStart.Month(), e.DTStart.Day(), 0, 0, 0, 0, time.UTC)
	}
	opt.Dtstart = dtstart
	if neverMatches(*opt) {
		return nil
	}
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return []time.Time{e.DTStart}
	}
	var out []time.Time
	next := r.Iterator()
	for i := 0; len(out) < max && i < maxRuleIterations; i++ {
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

// neverMatches reports whether the rule, ignoring COUNT and UNTIL, has no occurrence at all (e.g.
// FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30). rrule-go looks for the next match up to the year 9999
// before giving up, and UNTIL does not stop that search (it is only compared with candidates
// that matched), so such a rule cost ~200ms per expansion. The Gregorian calendar repeats every
// 400 years, so the rule's pattern repeats every 400*INTERVAL years: a probe moved forward by a
// multiple of that period, to within two periods of 9999, searches at most two periods.
func neverMatches(opt rrule.ROption) bool {
	if len(opt.Byeaster) > 0 {
		return false // Easter does not follow the 400-year cycle
	}
	period := 400 * max(opt.Interval, 1)
	shift := (rrule.MAXYEAR - period - opt.Dtstart.Year()) / period * period
	if shift <= 0 {
		return false // the real search is already within about one period of its end
	}
	opt.Dtstart = opt.Dtstart.AddDate(shift, 0, 0)
	opt.Count = 0
	opt.Until = time.Time{}
	r, err := rrule.NewRRule(opt)
	if err != nil {
		return false
	}
	_, ok := r.Iterator()()
	return !ok
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
