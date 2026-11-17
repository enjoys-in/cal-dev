package scheduling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
)

// AddToCalendarLinks are the "Add to calendar" buttons of an invitation email: prefilled
// Google Calendar and Outlook.com pages, and a download of the event's .ics (Apple / other apps).
type AddToCalendarLinks struct {
	Google  string
	Outlook string
	ICS     string
}

// addToCalendarLinks builds the links for one recipient of event. The .ics link is signed
// for that recipient with the same token as the RSVP links.
func (svc *Service) addToCalendarLinks(msg *models.SchedulingMessage, event *models.Event) AddToCalendarLinks {
	if event == nil {
		return AddToCalendarLinks{}
	}
	return AddToCalendarLinks{
		Google:  googleCalendarURL(event),
		Outlook: outlookCalendarURL(event),
		ICS:     fmt.Sprintf("%s/api/public/ics?token=%s", svc.rsvpBaseURL(), svc.rsvpToken(msg, event)),
	}
}

// eventSpan returns the event's start and end; a missing end is one hour (one day when all-day).
func eventSpan(e *models.Event) (time.Time, time.Time) {
	start := e.DTStart
	if e.DTEnd != nil && e.DTEnd.After(start) {
		return start, *e.DTEnd
	}
	if e.AllDay {
		return start, start.AddDate(0, 0, 1)
	}
	return start, start.Add(time.Hour)
}

// clipText shortens s to at most n runes so prefilled links stay a reasonable length.
func clipText(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

// encodeQuery encodes spaces as %20: Outlook shows a "+" from form encoding literally.
func encodeQuery(q url.Values) string {
	return strings.ReplaceAll(q.Encode(), "+", "%20")
}

// googleCalendarURL opens Google Calendar's "new event" page prefilled with event.
func googleCalendarURL(e *models.Event) string {
	start, end := eventSpan(e)
	q := url.Values{}
	q.Set("action", "TEMPLATE")
	q.Set("text", e.Summary)
	if e.AllDay {
		// all-day dates are stored at UTC midnight; the end date is exclusive, as Google expects
		q.Set("dates", start.UTC().Format("20060102")+"/"+end.UTC().Format("20060102"))
	} else {
		q.Set("dates", start.UTC().Format("20060102T150405Z")+"/"+end.UTC().Format("20060102T150405Z"))
		if tz := ics.EventTZID(e); tz != "" {
			q.Set("ctz", tz)
		}
	}
	if d := clipText(e.Description, 1000); d != "" {
		q.Set("details", d)
	}
	if e.Location != "" {
		q.Set("location", e.Location)
	}
	if e.RecurrenceRule != "" {
		q.Set("recur", "RRULE:"+e.RecurrenceRule)
	}
	return "https://calendar.google.com/calendar/render?" + encodeQuery(q)
}

// outlookCalendarURL opens Outlook on the web's "new event" page prefilled with event.
// Outlook's link has no repeat rule; the .ics download keeps it.
func outlookCalendarURL(e *models.Event) string {
	start, end := eventSpan(e)
	q := url.Values{}
	q.Set("path", "/calendar/action/compose")
	q.Set("rru", "addevent")
	q.Set("subject", e.Summary)
	if e.AllDay {
		q.Set("allday", "true")
		q.Set("startdt", start.UTC().Format("2006-01-02"))
		q.Set("enddt", end.UTC().Format("2006-01-02"))
	} else {
		q.Set("startdt", start.UTC().Format("2006-01-02T15:04:05Z"))
		q.Set("enddt", end.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if d := clipText(e.Description, 1000); d != "" {
		q.Set("body", d)
	}
	if e.Location != "" {
		q.Set("location", e.Location)
	}
	return "https://outlook.live.com/calendar/0/deeplink/compose?" + encodeQuery(q)
}

// errNotInvited means the token's guest is no longer on the event.
var errNotInvited = errors.New("not invited to this event")

// GuestEventICS returns the event of an RSVP token as a .ics file, for the guest named in the
// token while they are still invited.
func (svc *Service) GuestEventICS(ctx context.Context, p *RSVPTokenPayload) (string, error) {
	rows, err := svc.store.FindResourceByUID(ctx, p.TenantID, p.EventUID)
	if err != nil || len(rows) == 0 {
		return "", fmt.Errorf("event not found: %s", p.EventUID)
	}
	invited := false
	for _, ev := range rows {
		var attendees []models.Attendee
		if json.Unmarshal(ev.Attendees, &attendees) != nil {
			continue
		}
		for _, a := range attendees {
			if strings.EqualFold(a.Email, p.AttendeeEmail) {
				invited = true
				break
			}
		}
		if invited {
			break
		}
	}
	if !invited {
		return "", errNotInvited
	}
	return ics.GenerateResourceICS(rows), nil
}
