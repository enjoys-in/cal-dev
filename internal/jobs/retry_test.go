package jobs

import (
	"errors"
	"testing"
	"time"
)

func TestSchedulingEmailRetrySchedule(t *testing.T) {
	q := &Queue{maxRetry: 3, retryDelay: 5 * time.Second}
	q.SetRetrySchedule(JobSendSchedulingEmail, SchedulingEmailRetrySchedule)

	if got := q.maxRetryFor(JobSendSchedulingEmail); got != 5 {
		t.Fatalf("maxRetryFor = %d, want 5", got)
	}
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 25 * time.Minute, time.Hour}
	for i, w := range want {
		job := &Job{Type: JobSendSchedulingEmail, Retries: i + 1}
		if got := q.retryDelayFor(job); got != w {
			t.Errorf("retry %d: wait %v, want %v", i+1, got, w)
		}
	}
	// beyond the schedule the last wait is reused
	if got := q.retryDelayFor(&Job{Type: JobSendSchedulingEmail, Retries: 9}); got != time.Hour {
		t.Errorf("retry 9: wait %v, want 1h", got)
	}
}

func TestOtherJobsKeepLinearRetry(t *testing.T) {
	q := &Queue{maxRetry: 3, retryDelay: 5 * time.Second}
	q.SetRetrySchedule(JobSendSchedulingEmail, SchedulingEmailRetrySchedule)

	if got := q.maxRetryFor(JobProcessICS); got != 3 {
		t.Fatalf("maxRetryFor = %d, want 3", got)
	}
	if got := q.retryDelayFor(&Job{Type: JobProcessICS, Retries: 2}); got != 10*time.Second {
		t.Errorf("wait %v, want 10s", got)
	}
}

func TestRetryHint(t *testing.T) {
	base := errors.New("busy")
	if retryHint(base) != 0 {
		t.Fatal("plain error must carry no hint")
	}
	err := WithRetryAfter(base, 7*time.Minute)
	if retryHint(err) != 7*time.Minute || !errors.Is(err, base) {
		t.Fatalf("hint lost or error not wrapped: %v", err)
	}
	if WithRetryAfter(base, 0) != base {
		t.Fatal("zero wait must return the error unchanged")
	}
}
