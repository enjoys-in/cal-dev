package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/google/uuid"
)

// HandlerFunc processes a single job. Return error to trigger retry.
type HandlerFunc func(ctx context.Context, job *Job) error

// Worker processes background jobs from the Valkey queue.
type Worker struct {
	queue       *Queue
	store       *store.Store
	handlers    map[string]HandlerFunc
	concurrency int
	stopTimeout time.Duration
	wg          sync.WaitGroup
	cancel      context.CancelFunc
	stopOnce    sync.Once
}

// NewWorker creates a new background job worker.
func NewWorker(q *Queue, s *store.Store, cfg *config.WorkerConfig) *Worker {
	return &Worker{
		queue:       q,
		store:       s,
		handlers:    make(map[string]HandlerFunc),
		concurrency: cfg.Concurrency,
		stopTimeout: cfg.ShutdownTimeout,
	}
}

// bookkeepingCtx is used for retry/dead-letter/history writes after a handler ran: the worker
// context may already be cancelled by Stop, and those writes must still happen.
func bookkeepingCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// Register adds a handler for a job type.
func (w *Worker) Register(jobType string, handler HandlerFunc) {
	w.handlers[jobType] = handler
}

// Start launches worker goroutines and the scheduled-job promoter.
func (w *Worker) Start(parentCtx context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	w.cancel = cancel

	// Collect all registered job types
	jobTypes := make([]string, 0, len(w.handlers))
	for jt := range w.handlers {
		jobTypes = append(jobTypes, jt)
	}

	if len(jobTypes) == 0 {
		log.Println("worker: no handlers registered, skipping start")
		return
	}

	log.Printf("worker: starting %d workers for %v", w.concurrency, jobTypes)

	// Launch worker goroutines
	for i := 0; i < w.concurrency; i++ {
		w.wg.Add(1)
		go w.processLoop(ctx, i, jobTypes)
	}

	// Launch scheduled-job promoter (moves delayed/retry jobs into queues)
	w.wg.Add(1)
	go w.promoterLoop(ctx)

	// Launch periodic maintenance jobs
	w.wg.Add(1)
	go w.maintenanceLoop(ctx)
}

// Stop gracefully shuts down all workers.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() {
		log.Println("worker: shutting down...")
		if w.cancel != nil {
			w.cancel()
		}
		done := make(chan struct{})
		go func() { w.wg.Wait(); close(done) }()
		if w.stopTimeout > 0 {
			select {
			case <-done:
			case <-time.After(w.stopTimeout):
				log.Printf("worker: shutdown timed out after %s, abandoning running jobs", w.stopTimeout)
				return
			}
		} else {
			<-done
		}
		log.Println("worker: shutdown complete")
	})
}

// processLoop is a single worker goroutine that dequeues and handles jobs.
func (w *Worker) processLoop(ctx context.Context, workerID int, jobTypes []string) {
	defer w.wg.Done()

	// Dequeue serves keys in the given order, so rotate it every poll: a flooded queue must not
	// starve the others.
	order := make([]string, len(jobTypes))
	for n := workerID; ; n++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		for i := range jobTypes {
			order[i] = jobTypes[(n+i)%len(jobTypes)]
		}
		job, err := w.queue.Dequeue(ctx, 2*time.Second, order...)
		if err != nil {
			if ctx.Err() != nil {
				return // context cancelled
			}
			log.Printf("worker[%d]: dequeue error: %v", workerID, err)
			time.Sleep(time.Second)
			continue
		}

		if job == nil {
			continue // timeout, try again
		}

		w.handleJob(ctx, workerID, job)
	}
}

// handleJob executes a job's handler and records the result.
func (w *Worker) handleJob(ctx context.Context, workerID int, job *Job) {
	handler, ok := w.handlers[job.Type]
	if !ok {
		log.Printf("ERROR worker[%d]: no handler for job type %q (id=%s), moved to dead-letter: %s", workerID, job.Type, job.ID, job.Payload)
		dctx, dcancel := bookkeepingCtx()
		defer dcancel()
		if err := w.queue.sendToDeadLetter(dctx, job); err != nil {
			log.Printf("ERROR worker[%d]: dead-letter push failed: %v", workerID, err)
			return // not acked: the reaper re-queues it
		}
		w.ack(dctx, workerID, job)
		return
	}

	start := time.Now()
	err := handler(ctx, job)
	duration := time.Since(start)

	result := &models.JobResult{
		ID:         uuid.New().String(),
		JobType:    job.Type,
		Payload:    job.Payload,
		DurationMs: int(duration.Milliseconds()),
	}

	bctx, bcancel := bookkeepingCtx()
	defer bcancel()

	if err != nil {
		log.Printf("worker[%d]: job %s/%s failed (attempt %d): %v",
			workerID, job.Type, job.ID, job.Retries+1, err)

		result.Status = "failed"
		result.ErrorMessage = err.Error()

		// Retry if possible. If even that fails the job is not acked, so the reaper re-queues it.
		if retryErr := w.queue.RetryAfter(bctx, job, retryHint(err)); retryErr != nil {
			log.Printf("worker[%d]: retry enqueue failed: %v", workerID, retryErr)
			result.Status = "dead"
		} else {
			w.ack(bctx, workerID, job)
		}
	} else {
		result.Status = "completed"
		w.ack(bctx, workerID, job)
	}

	// Record in job_history (best-effort)
	if recordErr := w.store.RecordJobResult(bctx, result); recordErr != nil {
		log.Printf("worker[%d]: failed to record job result: %v", workerID, recordErr)
	}
}

// ack removes a finished job from the processing set. A failed ack only means the job may run
// once more after the visibility timeout.
func (w *Worker) ack(ctx context.Context, workerID int, job *Job) {
	if err := w.queue.Ack(ctx, job); err != nil {
		log.Printf("worker[%d]: ack of job %s/%s failed: %v", workerID, job.Type, job.ID, err)
	}
}

// promoterLoop periodically moves scheduled/delayed jobs into their queues and re-queues jobs
// whose worker died before acking them.
func (w *Worker) promoterLoop(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.queue.PromoteScheduled(ctx); err != nil {
				if ctx.Err() == nil {
					log.Printf("worker: promoter error: %v", err)
				}
			}
		}
	}
}

// maintenanceLoop dispatches periodic maintenance jobs.
func (w *Worker) maintenanceLoop(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// 1-hour ticker for rolling 24-hour reminder sweep [NOW(), NOW() + 24h]
	hourlyTicker := time.NewTicker(1 * time.Hour)
	defer hourlyTicker.Stop()

	// Initial sweep on startup
	if _, ok := w.handlers[JobSweepReminders]; ok {
		if _, err := w.queue.Enqueue(ctx, JobSweepReminders, nil); err != nil {
			log.Printf("worker: failed to enqueue initial reminder sweep: %v", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-hourlyTicker.C:
			// Enqueue 1-hour sweep for calendar reminders
			if _, ok := w.handlers[JobSweepReminders]; ok {
				if _, err := w.queue.Enqueue(ctx, JobSweepReminders, nil); err != nil {
					log.Printf("worker: failed to enqueue hourly reminder sweep: %v", err)
				}
			}
		case <-ticker.C:
			// Cleanup expired locks
			if _, ok := w.handlers[JobCleanupLocks]; ok {
				if _, err := w.queue.Enqueue(ctx, JobCleanupLocks, nil); err != nil {
					log.Printf("worker: failed to enqueue lock cleanup: %v", err)
				}
			}

			// Cleanup old job history
			if _, ok := w.handlers[JobCleanupJobHistory]; ok {
				if _, err := w.queue.Enqueue(ctx, JobCleanupJobHistory, nil); err != nil {
					log.Printf("worker: failed to enqueue job history cleanup: %v", err)
				}
			}

			// Enqueue due calendar subscription syncs
			if _, ok := w.handlers["enqueue_due_syncs"]; ok {
				if _, err := w.queue.Enqueue(ctx, "enqueue_due_syncs", nil); err != nil {
					log.Printf("worker: failed to enqueue due syncs: %v", err)
				}
			}
		}
	}
}

// ============================================================
// Job Payload Types
// ============================================================

// SchedulingEmailPayload is the payload for send_scheduling_email jobs.
type SchedulingEmailPayload struct {
	MessageID string `json:"message_id"`
	TenantID  int    `json:"tenant_id"`
}

// ProcessICSPayload is the payload for process_ics jobs.
type ProcessICSPayload struct {
	TenantID   int    `json:"tenant_id"`
	CalendarID string `json:"calendar_id"`
	EventUID   string `json:"event_uid"`
	RawICS     string `json:"raw_ics"`
	UserID     int    `json:"user_id"`
}

// SyncNotificationPayload is the payload for sync_notification jobs.
type SyncNotificationPayload struct {
	TenantID   int    `json:"tenant_id"`
	CalendarID string `json:"calendar_id"`
	ChangeType string `json:"change_type"`
	EventUID   string `json:"event_uid"`
}

// ParsePayload unmarshals a job's payload into the given struct.
func ParsePayload[T any](job *Job) (*T, error) {
	var payload T
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return nil, fmt.Errorf("parse payload: %w", err)
	}
	return &payload, nil
}
