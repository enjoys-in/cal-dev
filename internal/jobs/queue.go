package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Job types dispatched to the queue.
const (
	JobSendSchedulingEmail = "send_scheduling_email"
	JobProcessICS          = "process_ics"
	JobCleanupLocks        = "cleanup_locks"
	JobCleanupJobHistory   = "cleanup_job_history"
	JobSyncNotification    = "sync_notification"
	JobCalendarReminder    = "calendar_reminder"
	JobSweepReminders      = "sweep_reminders"
)

// Job represents a unit of work dispatched to Valkey.
type Job struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Retries  int             `json:"retries"`
	MaxRetry int             `json:"max_retry"`
	Created  time.Time       `json:"created"`

	raw string // exact JSON as dequeued; identifies the job in the processing set (see Ack)
}

// visibilityTimeout is how long a dequeued job may run before the reaper assumes its worker died
// and puts it back on its queue. It must stay well above any normal handler's run time.
const visibilityTimeout = 5 * time.Minute

// dequeuePoll is how often an idle Dequeue re-checks the queues.
const dequeuePoll = 250 * time.Millisecond

// Queue provides a Valkey-backed reliable job queue: LPUSH to enqueue; Dequeue atomically moves a
// job into a processing set until it is acked, and the promoter re-queues jobs whose worker died.
// Delivery is therefore at-least-once: a job can run twice if a worker crashes (or overruns the
// visibility timeout) after doing its work but before Ack. Handlers are expected to be idempotent
// (scheduling emails only send while still 'pending', reminders are deduplicated).
type Queue struct {
	client     *redis.Client
	prefix     string
	maxRetry   int
	retryDelay time.Duration
	schedules  map[string][]time.Duration // per job type waits between retries (see retry.go)
}

// NewQueue creates a new Valkey job queue.
func NewQueue(redisCfg *config.RedisConfig, workerCfg *config.WorkerConfig) (*Queue, error) {
	opts, err := redis.ParseURL(redisCfg.URL)
	if err != nil {
		return nil, fmt.Errorf("jobs: invalid redis URL: %w", err)
	}
	client := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("jobs: valkey ping failed: %w", err)
	}

	log.Printf("Connected to Valkey at %s", redisCfg.URL)

	return &Queue{
		client:     client,
		prefix:     workerCfg.QueuePrefix,
		maxRetry:   workerCfg.RetryMax,
		retryDelay: workerCfg.RetryDelay,
		schedules: map[string][]time.Duration{
			JobSendSchedulingEmail: SchedulingEmailRetrySchedule,
		},
	}, nil
}

// Close closes the Valkey connection.
func (q *Queue) Close() error {
	return q.client.Close()
}

// Client returns the underlying redis.Client for direct use.
func (q *Queue) Client() *redis.Client {
	return q.client
}

// queueKey returns the Valkey list key for a job type.
func (q *Queue) queueKey(jobType string) string {
	return fmt.Sprintf("%s:%s", q.prefix, jobType)
}

// deadLetterKey returns the dead-letter queue key.
func (q *Queue) deadLetterKey() string {
	return fmt.Sprintf("%s:dead", q.prefix)
}

// processingKey returns the sorted set of in-flight jobs (member = raw job JSON, score = unix
// deadline after which the job is re-queued).
func (q *Queue) processingKey() string {
	return fmt.Sprintf("%s:processing", q.prefix)
}

// Enqueue pushes a job onto the queue.
func (q *Queue) Enqueue(ctx context.Context, jobType string, payload interface{}) (string, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("jobs: marshal payload: %w", err)
	}

	job := &Job{
		ID:       uuid.New().String(),
		Type:     jobType,
		Payload:  json.RawMessage(payloadBytes),
		Retries:  0,
		MaxRetry: q.maxRetryFor(jobType),
		Created:  time.Now().UTC(),
	}

	jobBytes, err := json.Marshal(job)
	if err != nil {
		return "", fmt.Errorf("jobs: marshal job: %w", err)
	}

	if err := q.client.LPush(ctx, q.queueKey(jobType), jobBytes).Err(); err != nil {
		return "", fmt.Errorf("jobs: enqueue %s: %w", jobType, err)
	}

	return job.ID, nil
}

// EnqueueDelayed pushes a job that becomes available after a delay.
// Uses a sorted set with score = unix timestamp.
func (q *Queue) EnqueueDelayed(ctx context.Context, jobType string, payload interface{}, delay time.Duration) (string, error) {
	return q.enqueueDelayed(ctx, uuid.New().String(), time.Now().UTC(), jobType, payload, delay)
}

// EnqueueDelayedWithID is EnqueueDelayed with a caller-chosen deterministic id. The scheduled-set
// member is then built only from id, type and payload, so enqueueing the same job again replaces
// the pending one (and its run time) instead of adding a duplicate.
func (q *Queue) EnqueueDelayedWithID(ctx context.Context, id, jobType string, payload interface{}, delay time.Duration) (string, error) {
	return q.enqueueDelayed(ctx, id, time.Time{}, jobType, payload, delay)
}

func (q *Queue) enqueueDelayed(ctx context.Context, id string, created time.Time, jobType string, payload interface{}, delay time.Duration) (string, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("jobs: marshal payload: %w", err)
	}

	job := &Job{
		ID:       id,
		Type:     jobType,
		Payload:  json.RawMessage(payloadBytes),
		Retries:  0,
		MaxRetry: q.maxRetryFor(jobType),
		Created:  created,
	}

	jobBytes, err := json.Marshal(job)
	if err != nil {
		return "", fmt.Errorf("jobs: marshal job: %w", err)
	}

	scheduledKey := fmt.Sprintf("%s:scheduled", q.prefix)
	score := float64(time.Now().Add(delay).Unix())

	if err := q.client.ZAdd(ctx, scheduledKey, redis.Z{
		Score:  score,
		Member: string(jobBytes),
	}).Err(); err != nil {
		return "", fmt.Errorf("jobs: enqueue delayed %s: %w", jobType, err)
	}

	return job.ID, nil
}

// dequeueScript pops one job from the first non-empty queue list in KEYS[2..] (in the given order,
// like BRPOP) and, in the same atomic step, records it in the processing set KEYS[1] with score
// ARGV[1] (its deadline). Returns {queueKey, raw} or nil when every queue is empty.
var dequeueScript = redis.NewScript(`
for i = 2, #KEYS do
  local raw = redis.call('RPOP', KEYS[i])
  if raw then
    redis.call('ZADD', KEYS[1], ARGV[1], raw)
    return {KEYS[i], raw}
  end
end
return false
`)

// Dequeue waits up to timeout for a job on any of the given queues (checked in order) and moves it
// into the processing set; the caller must Ack it once its outcome is recorded.
// Returns nil if no job arrived or the context is cancelled.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration, jobTypes ...string) (*Job, error) {
	keys := make([]string, 0, len(jobTypes)+1)
	keys = append(keys, q.processingKey())
	for _, jt := range jobTypes {
		keys = append(keys, q.queueKey(jt))
	}

	// BLMOVE only takes one source list, so poll a non-blocking pop across all queues instead.
	end := time.Now().Add(timeout)
	for {
		deadline := time.Now().Add(visibilityTimeout).Unix()
		result, err := dequeueScript.Run(ctx, q.client, keys, deadline).StringSlice()
		if err == nil && len(result) == 2 {
			return q.decodeJob(ctx, result[0], result[1]), nil
		}
		if err != nil && err != redis.Nil {
			return nil, fmt.Errorf("jobs: dequeue: %w", err)
		}

		wait := time.Until(end)
		if wait <= 0 {
			return nil, nil // timeout, no job
		}
		if wait > dequeuePoll {
			wait = dequeuePoll
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(wait):
		}
	}
}

// decodeJob parses a dequeued job. An unparseable one is moved to the dead-letter list (and out of
// the processing set) and nil is returned.
func (q *Queue) decodeJob(ctx context.Context, queueKey, raw string) *Job {
	var job Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		// Keep the raw job for inspection instead of dropping it silently.
		log.Printf("ERROR jobs: unparseable job from %s moved to dead-letter: %v: %s", queueKey, err, raw)
		bctx := context.WithoutCancel(ctx)
		if dlErr := q.client.LPush(bctx, q.deadLetterKey(), raw).Err(); dlErr != nil {
			log.Printf("ERROR jobs: dead-letter push failed: %v", dlErr)
			return nil // left in processing; the reaper will re-queue it
		}
		if ackErr := q.client.ZRem(bctx, q.processingKey(), raw).Err(); ackErr != nil {
			log.Printf("ERROR jobs: ack of dead-lettered job failed: %v", ackErr)
		}
		return nil
	}
	job.raw = raw
	return &job
}

// Ack removes a dequeued job from the processing set. Call it only after the job's outcome
// (completed, retry scheduled or dead-lettered) is stored, so a crash in between re-runs the job
// instead of losing it.
func (q *Queue) Ack(ctx context.Context, job *Job) error {
	if job.raw == "" {
		return nil
	}
	return q.client.ZRem(ctx, q.processingKey(), job.raw).Err()
}

// Retry re-enqueues a failed job with incremented retry count.
// If max retries exceeded, sends to dead-letter queue.
func (q *Queue) Retry(ctx context.Context, job *Job) error {
	return q.RetryAfter(ctx, job, 0)
}

// RetryAfter is Retry with a minimum wait (e.g. the receiver's Retry-After). The wait is the
// longer of minWait and the job type's schedule (or WORKER_RETRY_DELAY × attempt).
func (q *Queue) RetryAfter(ctx context.Context, job *Job, minWait time.Duration) error {
	job.Retries++

	if job.Retries > job.MaxRetry {
		return q.sendToDeadLetter(ctx, job)
	}

	jobBytes, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("jobs: marshal retry: %w", err)
	}

	// Re-enqueue with delay using sorted set
	scheduledKey := fmt.Sprintf("%s:scheduled", q.prefix)
	wait := q.retryDelayFor(job)
	if minWait > wait {
		wait = minWait
	}
	score := float64(time.Now().Add(wait).Unix())

	return q.client.ZAdd(ctx, scheduledKey, redis.Z{
		Score:  score,
		Member: string(jobBytes),
	}).Err()
}

// sendToDeadLetter moves a job to the dead-letter queue.
func (q *Queue) sendToDeadLetter(ctx context.Context, job *Job) error {
	jobBytes, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return q.client.LPush(ctx, q.deadLetterKey(), jobBytes).Err()
}

// promoteBatch caps how many due jobs one promote script call moves.
const promoteBatch = 100

// promoteScript atomically moves up to ARGV[2] due members (score <= ARGV[1]) of the scheduled
// or processing set KEYS[1] to their queue list ARGV[3]..":"..type, so two instances can never promote the same
// job twice. Members that are not valid job JSON go to the dead-letter list KEYS[2].
var promoteScript = redis.NewScript(`
local items = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, tonumber(ARGV[2]))
for _, raw in ipairs(items) do
  local ok, job = pcall(cjson.decode, raw)
  if ok and type(job) == 'table' and type(job['type']) == 'string' and job['type'] ~= '' then
    redis.call('LPUSH', ARGV[3] .. ':' .. job['type'], raw)
  else
    redis.call('LPUSH', KEYS[2], raw)
  end
  redis.call('ZREM', KEYS[1], raw)
end
return #items
`)

// PromoteScheduled moves scheduled/delayed jobs that are ready into their queues, and re-queues
// in-flight jobs whose visibility timeout expired (their worker crashed or was cut off at
// shutdown). Should be called periodically.
func (q *Queue) PromoteScheduled(ctx context.Context) error {
	if _, err := q.promoteDue(ctx, fmt.Sprintf("%s:scheduled", q.prefix)); err != nil {
		return fmt.Errorf("jobs: promote scheduled: %w", err)
	}
	n, err := q.promoteDue(ctx, q.processingKey())
	if err != nil {
		return fmt.Errorf("jobs: requeue expired: %w", err)
	}
	if n > 0 {
		log.Printf("jobs: re-queued %d job(s) not acked within %s", n, visibilityTimeout)
	}
	return nil
}

// promoteDue moves all members of the sorted set zkey with score <= now onto their queues
// (atomically per batch, so concurrent instances never move the same member twice).
func (q *Queue) promoteDue(ctx context.Context, zkey string) (int, error) {
	keys := []string{zkey, q.deadLetterKey()}
	total := 0
	for {
		n, err := promoteScript.Run(ctx, q.client, keys, time.Now().Unix(), promoteBatch, q.prefix).Int()
		if err != nil {
			return total, err
		}
		total += n
		if n < promoteBatch {
			return total, nil
		}
	}
}

// QueueLength returns the number of pending jobs for a given type.
func (q *Queue) QueueLength(ctx context.Context, jobType string) (int64, error) {
	return q.client.LLen(ctx, q.queueKey(jobType)).Result()
}

// DeadLetterLength returns the number of dead-lettered jobs.
func (q *Queue) DeadLetterLength(ctx context.Context) (int64, error) {
	return q.client.LLen(ctx, q.deadLetterKey()).Result()
}
