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
