package scheduling

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/recurrence"
)

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func TestResolveRecipients(t *testing.T) {
	svc := &Service{} // Nil store and queue for pure unit testing

	attendees := []models.Attendee{
		{Email: "attendee1@example.com", Status: "ACCEPTED"},
		{Email: "attendee2@example.com", Status: "DECLINED"},
		{Email: "attendee3@example.com", Status: "NEEDS-ACTION"},
	}
	rawAtts, _ := json.Marshal(attendees)

	event := &models.Event{
		Organizer: "organizer@example.com",
		Attendees: rawAtts,
	}

	recipients := svc.resolveRecipients(context.Background(), event)

	// Should contain organizer, attendee1, attendee3 (attendee2 was DECLINED)
	if len(recipients) != 3 {
		t.Fatalf("expected 3 recipients, got %d", len(recipients))
	}

	recMap := make(map[string]bool)
	for _, r := range recipients {
		recMap[r.Email] = true
	}

	if !recMap["organizer@example.com"] {
		t.Errorf("missing organizer")
	}
	if !recMap["attendee1@example.com"] {
		t.Errorf("missing attendee1")
	}
	if recMap["attendee2@example.com"] {
		t.Errorf("declined attendee2 should not be included")
	}
	if !recMap["attendee3@example.com"] {
		t.Errorf("missing attendee3")
	}
}

func TestSeriesOccurrencesExpansion(t *testing.T) {
	now := utc(2026, 11, 15, 10, 0)
	oneDayAhead := now.Add(24 * time.Hour)

	// Single event inside window
	e1 := &models.Event{
		UID:       "evt-single-1",
		DTStart:   utc(2026, 11, 15, 14, 0),
		Summary:   "Afternoon Standup",
		Properties: json.RawMessage("{}"),
	}
	occ1 := recurrence.Expand(e1, now, oneDayAhead, 10)
	if len(occ1) != 1 {
		t.Fatalf("expected 1 occurrence for single event, got %d", len(occ1))
	}

	// Single event outside 24h window
	e2 := &models.Event{
		UID:       "evt-single-2",
		DTStart:   utc(2026, 11, 17, 10, 0),
		Summary:   "Two days later",
		Properties: json.RawMessage("{}"),
	}
	occ2 := recurrence.Expand(e2, now, oneDayAhead, 10)
	if len(occ2) != 0 {
		t.Fatalf("expected 0 occurrences outside 24h window, got %d", len(occ2))
	}

	// Recurring Daily Series starting 10 days ago at 15:00 UTC
	eDaily := &models.Event{
		UID:            "evt-daily-series",
		DTStart:        utc(2026, 11, 5, 15, 0),
		RecurrenceRule: "FREQ=DAILY",
		Summary:        "Daily Sync",
		Properties:     json.RawMessage("{}"),
	}
	occDaily := recurrence.Expand(eDaily, now, oneDayAhead, 10)
	// Between Nov 15 10:00 and Nov 16 10:00, there is exactly one occurrence: Nov 15 15:00
	if len(occDaily) != 1 {
		t.Fatalf("expected 1 daily occurrence in 24h window, got %d", len(occDaily))
	}
	if !occDaily[0].Start.Equal(utc(2026, 11, 15, 15, 0)) {
		t.Errorf("unexpected occurrence time: %v", occDaily[0].Start)
	}

	// Series with RECURRENCE-ID override: Nov 15 15:00 moved to Nov 15 16:30
	overrideEvt := &models.Event{
		UID:          "evt-daily-series",
		RecurrenceID: "20261115T150000Z",
		DTStart:      utc(2026, 11, 15, 16, 30),
		Summary:      "Daily Sync (Moved)",
		Properties:   json.RawMessage("{}"),
	}
	series := recurrence.Series{
		Master:    eDaily,
		Overrides: []*models.Event{overrideEvt},
	}
	occSeries := series.Expand(now, oneDayAhead, 10)
	if len(occSeries) != 1 {
		t.Fatalf("expected 1 occurrence in series, got %d", len(occSeries))
	}
	if !occSeries[0].Start.Equal(utc(2026, 11, 15, 16, 30)) {
		t.Errorf("expected override time Nov 15 16:30, got %v", occSeries[0].Start)
	}
	if occSeries[0].Event.Summary != "Daily Sync (Moved)" {
		t.Errorf("expected override summary, got %s", occSeries[0].Event.Summary)
	}
}
