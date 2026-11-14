package availability

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
)

func at(day, h, m int) time.Time { return time.Date(2026, 1, day, h, m, 0, 0, time.UTC) }

func ev(start, end time.Time, mods ...func(*models.Event)) *models.Event {
	e := &models.Event{
		DTStart: start, DTEnd: &end, ComponentType: "VEVENT", Status: "CONFIRMED",
		Transparency: "OPAQUE", Attendees: json.RawMessage("[]"), Properties: json.RawMessage("{}"),
	}
	for _, m := range mods {
		m(e)
	}
	return e
}

var winFrom, winTo = at(1, 0, 0), at(31, 0, 0)

func TestOverlappingEventsMerge(t *testing.T) {
	got := Compute([]*models.Event{
		ev(at(5, 9, 0), at(5, 10, 0)),
		ev(at(5, 9, 30), at(5, 11, 0)),
		ev(at(5, 11, 0), at(5, 12, 0)), // adjacent
	}, "me@x.com", winFrom, winTo)
	if len(got) != 1 || !got[0].Start.Equal(at(5, 9, 0)) || !got[0].End.Equal(at(5, 12, 0)) {
		t.Fatalf("want one 09:00-12:00 block, got %+v", got)
	}
}

func TestTransparentAndCancelledAreFree(t *testing.T) {
	got := Compute([]*models.Event{
		ev(at(5, 9, 0), at(5, 10, 0), func(e *models.Event) { e.Transparency = "TRANSPARENT" }),
		ev(at(5, 11, 0), at(5, 12, 0), func(e *models.Event) { e.Status = "CANCELLED" }),
	}, "me@x.com", winFrom, winTo)
	if len(got) != 0 {
		t.Fatalf("want no busy time, got %+v", got)
	}
}

func TestDeclinedInvitationIsFreeUnansweredIsTentative(t *testing.T) {
	mk := func(status string) *models.Event {
		return ev(at(5, 9, 0), at(5, 10, 0), func(e *models.Event) {
			e.Organizer = "boss@x.com"
			b, _ := json.Marshal([]models.Attendee{{Email: "ME@x.com", Status: status}})
			e.Attendees = b
		})
	}
	if got := Compute([]*models.Event{mk("DECLINED")}, "me@x.com", winFrom, winTo); len(got) != 0 {
		t.Fatalf("declined must be free, got %+v", got)
	}
	got := Compute([]*models.Event{mk("NEEDS-ACTION")}, "me@x.com", winFrom, winTo)
	if len(got) != 1 || got[0].Status != StatusTentative {
		t.Fatalf("unanswered must be tentative, got %+v", got)
	}
	got = Compute([]*models.Event{mk("ACCEPTED")}, "me@x.com", winFrom, winTo)
	if len(got) != 1 || got[0].Status != StatusConfirmed {
		t.Fatalf("accepted must be confirmed, got %+v", got)
	}
}

func TestOrganizerIsNotTreatedAsInvitee(t *testing.T) {
	e := ev(at(5, 9, 0), at(5, 10, 0), func(e *models.Event) {
		e.Organizer = "mailto:me@x.com"
		b, _ := json.Marshal([]models.Attendee{{Email: "me@x.com", Status: "NEEDS-ACTION"}})
		e.Attendees = b
	})
	got := Compute([]*models.Event{e}, "me@x.com", winFrom, winTo)
	if len(got) != 1 || got[0].Status != StatusConfirmed {
		t.Fatalf("organizer's own event must be confirmed busy, got %+v", got)
	}
}

func TestConfirmedWinsOverTentativeOverlap(t *testing.T) {
	got := Compute([]*models.Event{
		ev(at(5, 9, 0), at(5, 11, 0), func(e *models.Event) { e.Status = "TENTATIVE" }),
		ev(at(5, 10, 0), at(5, 12, 0)),
	}, "me@x.com", winFrom, winTo)
	// 09-10 tentative, 10-12 confirmed
	if len(got) != 2 || got[0].Status != StatusTentative || got[1].Status != StatusConfirmed ||
		!got[1].Start.Equal(at(5, 10, 0)) || !got[1].End.Equal(at(5, 12, 0)) {
		t.Fatalf("unexpected merge: %+v", got)
	}
}

func TestRecurringSeriesCountsAsBusy(t *testing.T) {
	e := ev(at(5, 9, 0), at(5, 10, 0), func(e *models.Event) { e.RecurrenceRule = "FREQ=WEEKLY;COUNT=3" })
	got := Compute([]*models.Event{e}, "me@x.com", winFrom, winTo)
	if len(got) != 3 {
		t.Fatalf("want 3 weekly busy blocks, got %+v", got)
	}
}

func TestClippedToWindow(t *testing.T) {
	got := Compute([]*models.Event{ev(at(5, 9, 0), at(5, 20, 0))}, "me@x.com", at(5, 10, 0), at(5, 12, 0))
	if len(got) != 1 || !got[0].Start.Equal(at(5, 10, 0)) || !got[0].End.Equal(at(5, 12, 0)) {
		t.Fatalf("busy block not clipped: %+v", got)
	}
}

func TestStatusAt(t *testing.T) {
	ivs := []Interval{{Start: at(5, 9, 0), End: at(5, 10, 0), Status: StatusConfirmed}, {Start: at(5, 14, 0), End: at(5, 15, 0), Status: StatusTentative}}
	if c := StatusAt(ivs, at(5, 9, 30)); !c.Busy || c.Until == nil || !c.Until.Equal(at(5, 10, 0)) {
		t.Fatalf("expected busy until 10:00, got %+v", c)
	}
	if c := StatusAt(ivs, at(5, 11, 0)); c.Busy || c.NextBusyAt == nil || !c.NextBusyAt.Equal(at(5, 14, 0)) {
		t.Fatalf("expected free, next busy 14:00, got %+v", c)
	}
	if c := StatusAt(ivs, at(6, 0, 0)); c.Busy || c.NextBusyAt != nil {
		t.Fatalf("expected free with nothing next, got %+v", c)
	}
	// block end is exclusive
	if c := StatusAt(ivs, at(5, 10, 0)); c.Busy {
		t.Fatalf("end instant must be free, got %+v", c)
	}
}

func TestZeroLengthEventNotBusy(t *testing.T) {
	e := &models.Event{DTStart: at(5, 9, 0), ComponentType: "VEVENT", Attendees: json.RawMessage("[]"), Properties: json.RawMessage("{}")}
	if got := Compute([]*models.Event{e}, "me@x.com", winFrom, winTo); len(got) != 0 {
		t.Fatalf("zero-length event must not be busy, got %+v", got)
	}
}
