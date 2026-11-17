package scheduling

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
)

func TestAddToCalendarURLs_Timed(t *testing.T) {
	start := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	ev := &models.Event{Summary: "Design review", Description: "Notes + agenda", Location: "Room 4",
		DTStart: start, DTEnd: &end, RecurrenceRule: "FREQ=WEEKLY;BYDAY=MO"}

	g, err := url.Parse(googleCalendarURL(ev))
	if err != nil {
		t.Fatal(err)
	}
	q := g.Query()
	if q.Get("action") != "TEMPLATE" || q.Get("text") != "Design review" {
		t.Errorf("google: wrong action/text: %v", q)
	}
	if q.Get("dates") != "20261118T000000Z/20261118T010000Z" {
		t.Errorf("google: dates = %q", q.Get("dates"))
	}
	if q.Get("details") != "Notes + agenda" || q.Get("recur") != "RRULE:FREQ=WEEKLY;BYDAY=MO" {
		t.Errorf("google: details/recur = %q / %q", q.Get("details"), q.Get("recur"))
	}

	raw := outlookCalendarURL(ev)
	if strings.Contains(raw, "+") && !strings.Contains(raw, "%2B") {
		t.Errorf("outlook: spaces must be %%20, got %s", raw)
	}
	o, _ := url.Parse(raw)
	oq := o.Query()
	if oq.Get("subject") != "Design review" || oq.Get("startdt") != "2026-11-18T00:00:00Z" || oq.Get("enddt") != "2026-11-18T01:00:00Z" {
		t.Errorf("outlook: wrong fields: %v", oq)
	}
	if oq.Get("allday") != "" {
		t.Errorf("outlook: timed event marked all-day")
	}
}

func TestAddToCalendarURLs_AllDayWithoutEnd(t *testing.T) {
	ev := &models.Event{Summary: "Offsite", AllDay: true, DTStart: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}

	g, _ := url.Parse(googleCalendarURL(ev))
	if got := g.Query().Get("dates"); got != "20261201/20261202" {
		t.Errorf("google: all-day dates = %q", got)
	}
	o, _ := url.Parse(outlookCalendarURL(ev))
	oq := o.Query()
	if oq.Get("allday") != "true" || oq.Get("startdt") != "2026-12-01" || oq.Get("enddt") != "2026-12-02" {
		t.Errorf("outlook: all-day fields = %v", oq)
	}
}
