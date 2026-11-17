package jobs

import (
	"errors"
	"time"
)

// SchedulingEmailRetrySchedule is the wait before each retry of a calendar email
// (send_scheduling_email): +5m, +10m, +15m, +25m, +1h — 6 attempts over about 2 hours.
// airsend-api or a BYO SMTP server that is down for a while still gets the email later.
var SchedulingEmailRetrySchedule = []time.Duration{
	5 * time.Minute,
	10 * time.Minute,
	15 * time.Minute,
	25 * time.Minute,
	1 * time.Hour,
}

// RetryAfterError asks the worker to wait at least After before the next attempt
// (e.g. the receiver answered 429 with Retry-After). The retry schedule still applies;
// the longer of the two waits wins.
type RetryAfterError struct {
	Err   error
	After time.Duration
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// WithRetryAfter wraps err with a minimum wait before the next attempt.
func WithRetryAfter(err error, after time.Duration) error {
	if err == nil || after <= 0 {
		return err
	}
	return &RetryAfterError{Err: err, After: after}
}

// retryHint returns the minimum wait carried by err, if any.
func retryHint(err error) time.Duration {
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		return ra.After
	}
	return 0
}

// SetRetrySchedule gives one job type its own waits between attempts; the job then gets
// len(schedule) retries instead of WORKER_RETRY_MAX. Call before the worker starts.
func (q *Queue) SetRetrySchedule(jobType string, schedule []time.Duration) {
	if q.schedules == nil {
		q.schedules = make(map[string][]time.Duration)
	}
	q.schedules[jobType] = schedule
}

// maxRetryFor is the number of retries a new job of this type gets.
func (q *Queue) maxRetryFor(jobType string) int {
	if s, ok := q.schedules[jobType]; ok && len(s) > 0 {
		return len(s)
	}
	return q.maxRetry
}

// retryDelayFor is the wait before retry number `retries` (1-based) of job.
func (q *Queue) retryDelayFor(job *Job) time.Duration {
	if s, ok := q.schedules[job.Type]; ok && len(s) > 0 {
		i := job.Retries - 1
		if i < 0 {
			i = 0
		}
		if i >= len(s) {
			i = len(s) - 1
		}
		return s[i]
	}
	return q.retryDelay * time.Duration(job.Retries)
}
