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
)

// Job represents a unit of work dispatched to Valkey.
type Job struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Retries  int             `json:"retries"`
	MaxRetry int             `json:"max_retry"`
	Created  time.Time       `json:"created"`
}

// Queue provides a Valkey-backed job queue using LPUSH/BRPOP.
type Queue struct {
	client     *redis.Client
	prefix     string
	maxRetry   int
	retryDelay time.Duration
}

// NewQueue creates a new Valkey job queue.
func NewQueue(redisCfg *config.RedisConfig, workerCfg *config.WorkerConfig) (*Queue, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     redisCfg.RedisAddr(),
		Password: redisCfg.Password,
		DB:       redisCfg.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("jobs: valkey ping failed: %w", err)
	}

	log.Printf("Connected to Valkey at %s", redisCfg.RedisAddr())

	return &Queue{
		client:     client,
		prefix:     workerCfg.QueuePrefix,
		maxRetry:   workerCfg.RetryMax,
		retryDelay: workerCfg.RetryDelay,
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
		MaxRetry: q.maxRetry,
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
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("jobs: marshal payload: %w", err)
	}

	job := &Job{
		ID:       uuid.New().String(),
		Type:     jobType,
		Payload:  json.RawMessage(payloadBytes),
		Retries:  0,
		MaxRetry: q.maxRetry,
		Created:  time.Now().UTC(),
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

// Dequeue blocks until a job is available on any of the given queues.
// Returns nil if context is cancelled.
func (q *Queue) Dequeue(ctx context.Context, timeout time.Duration, jobTypes ...string) (*Job, error) {
	keys := make([]string, len(jobTypes))
	for i, jt := range jobTypes {
		keys[i] = q.queueKey(jt)
	}

	result, err := q.client.BRPop(ctx, timeout, keys...).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // timeout, no job
		}
		return nil, fmt.Errorf("jobs: dequeue: %w", err)
	}

	if len(result) < 2 {
		return nil, nil
	}

	var job Job
	if err := json.Unmarshal([]byte(result[1]), &job); err != nil {
		return nil, fmt.Errorf("jobs: unmarshal job: %w", err)
	}

	return &job, nil
}

// Retry re-enqueues a failed job with incremented retry count.
// If max retries exceeded, sends to dead-letter queue.
func (q *Queue) Retry(ctx context.Context, job *Job) error {
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
	score := float64(time.Now().Add(q.retryDelay * time.Duration(job.Retries)).Unix())

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

// PromoteScheduled moves scheduled/delayed jobs that are ready into their queues.
// Should be called periodically.
func (q *Queue) PromoteScheduled(ctx context.Context) error {
	scheduledKey := fmt.Sprintf("%s:scheduled", q.prefix)
	now := float64(time.Now().Unix())

	// Get all jobs scored <= now
	results, err := q.client.ZRangeByScore(ctx, scheduledKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: fmt.Sprintf("%f", now),
	}).Result()
	if err != nil {
		return fmt.Errorf("jobs: promote scheduled: %w", err)
	}

	for _, raw := range results {
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			log.Printf("jobs: skip invalid scheduled job: %v", err)
			q.client.ZRem(ctx, scheduledKey, raw)
			continue
		}

		// Push to correct queue
		if err := q.client.LPush(ctx, q.queueKey(job.Type), raw).Err(); err != nil {
			log.Printf("jobs: failed to promote job %s: %v", job.ID, err)
			continue
		}

		// Remove from scheduled set
		q.client.ZRem(ctx, scheduledKey, raw)
	}

	return nil
}

// QueueLength returns the number of pending jobs for a given type.
func (q *Queue) QueueLength(ctx context.Context, jobType string) (int64, error) {
	return q.client.LLen(ctx, q.queueKey(jobType)).Result()
}

// DeadLetterLength returns the number of dead-lettered jobs.
func (q *Queue) DeadLetterLength(ctx context.Context) (int64, error) {
	return q.client.LLen(ctx, q.deadLetterKey()).Result()
}
