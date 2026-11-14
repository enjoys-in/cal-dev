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
	}
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
		w.wg.Wait()
		log.Println("worker: shutdown complete")
	})
}

// processLoop is a single worker goroutine that dequeues and handles jobs.
func (w *Worker) processLoop(ctx context.Context, workerID int, jobTypes []string) {
	defer w.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := w.queue.Dequeue(ctx, 2*time.Second, jobTypes...)
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
		log.Printf("worker[%d]: no handler for job type %q (id=%s)", workerID, job.Type, job.ID)
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

	if err != nil {
		log.Printf("worker[%d]: job %s/%s failed (attempt %d): %v",
			workerID, job.Type, job.ID, job.Retries+1, err)

		result.Status = "failed"
		result.ErrorMessage = err.Error()

		// Retry if possible
		if retryErr := w.queue.Retry(ctx, job); retryErr != nil {
			log.Printf("worker[%d]: retry enqueue failed: %v", workerID, retryErr)
			result.Status = "dead"
		}
	} else {
		result.Status = "completed"
	}

	// Record in job_history (best-effort)
	if recordErr := w.store.RecordJobResult(ctx, result); recordErr != nil {
		log.Printf("worker[%d]: failed to record job result: %v", workerID, recordErr)
	}
}

// promoterLoop periodically moves scheduled/delayed jobs into their queues.
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

	for {
		select {
		case <-ctx.Done():
			return
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
