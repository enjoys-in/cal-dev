package recurrence

import (
	"testing"
	"time"
)

func TestSubMinuteRulesAreNotExpanded(t *testing.T) {
	for _, rule := range []string{
		"FREQ=HOURLY;BYMINUTE=0,1,2,3,4,5",
		"FREQ=DAILY;BYSECOND=0,1,2,3",
	} {
		e := ev(utc(2026, 1, 5, 9, 0), time.Minute, rule, "")
		got := Expand(e, utc(2026, 1, 5, 0, 0), utc(2026, 1, 6, 0, 0), 0)
		if len(got) != 1 {
			t.Errorf("%s: want only the master instance, got %d", rule, len(got))
		}
	}
}

func TestRuleIterationCap(t *testing.T) {
	// Hourly since 1900: far more than maxRuleIterations candidates precede the window, so the
	// expansion stops early instead of walking every hour up to 2026.
	e := ev(utc(1900, 1, 1, 0, 0), time.Minute, "FREQ=HOURLY", "")
	start := time.Now()
	got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2026, 1, 2, 0, 0), 0)
	if len(got) != 0 {
		t.Errorf("want no occurrences past the iteration cap, got %d", len(got))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("expansion took %v", d)
	}
}

func TestImpossibleRuleReturnsQuickly(t *testing.T) {
	// No February 30th ever exists: rrule-go alone would search day by day up to the year 9999.
	for _, rule := range []string{
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=DAILY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=DAILY;INTERVAL=3;BYMONTH=4;BYMONTHDAY=31;UNTIL=20300101T000000Z",
	} {
		e := ev(utc(2026, 1, 5, 9, 0), time.Hour, rule, "")
		start := time.Now()
		got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0), 0)
		has := HasOccurrenceIn(e, utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0))
		series := Series{Master: e}.Expand(utc(2026, 1, 1, 0, 0), utc(2027, 1, 1, 0, 0), 0)
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Errorf("%s: expansion took %v", rule, d)
		}
		if len(got) != 0 || has || len(series) != 0 {
			t.Errorf("%s: want no occurrences, got %d / %v / %d", rule, len(got), has, len(series))
		}
	}
}

func TestRareRuleStillMatches(t *testing.T) {
	// February 29th only exists in leap years; the impossible-rule check must not drop it.
	e := ev(utc(2026, 1, 5, 9, 0), time.Hour, "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29", "")
	got := Expand(e, utc(2026, 1, 1, 0, 0), utc(2030, 1, 1, 0, 0), 0)
	if len(got) != 1 || !got[0].Start.Equal(utc(2028, 2, 29, 9, 0)) {
		t.Errorf("want only 2028-02-29 09:00, got %v", startsOf(got))
	}
}
