package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/google/uuid"
)

const (
	// MaxFeedSize is the maximum ICS feed size we'll download (20MB).
	MaxFeedSize = 20 * 1024 * 1024

	// FetchTimeout is the HTTP timeout for fetching external feeds.
	FetchTimeout = 30 * time.Second

	// UserAgent sent when fetching feeds.
	UserAgent = "CalDev/1.0 (ICS Sync)"
)

// Service handles syncing external calendar subscriptions.
type Service struct {
	store  *store.Store
	queue  *jobs.Queue
	client *http.Client
}

// NewService creates a new sync service.
func NewService(s *store.Store, q *jobs.Queue) *Service {
	return &Service{
		store: s,
		queue: q,
		client: &http.Client{
			Timeout: FetchTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

// HandleSyncJob processes a single subscription sync job dispatched from the queue.
func (s *Service) HandleSyncJob(ctx context.Context, job *jobs.Job) error {
	var payload struct {
		SubscriptionID string `json:"subscription_id"`
		TenantID       int    `json:"tenant_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("sync: invalid payload: %w", err)
	}

	sub, err := s.store.GetSubscription(ctx, payload.TenantID, payload.SubscriptionID)
	if err != nil {
		return fmt.Errorf("sync: get subscription: %w", err)
	}
	if sub == nil {
		return fmt.Errorf("sync: subscription %s not found", payload.SubscriptionID)
	}
	if !sub.IsEnabled {
		return nil // disabled, skip
	}

	return s.SyncSubscription(ctx, sub)
}

// SyncSubscription fetches an external ICS feed and imports events.
func (s *Service) SyncSubscription(ctx context.Context, sub *models.CalendarSubscription) error {
	log.Printf("sync: starting sync for subscription %s (%s)", sub.ID, sub.DisplayName)

	// Fetch the ICS feed
	icsData, newETag, err := s.fetchFeed(ctx, sub)
	if err != nil {
		syncErr := fmt.Sprintf("fetch failed: %v", err)
		log.Printf("sync: %s for subscription %s", syncErr, sub.ID)
		_ = s.store.UpdateSubscriptionSyncState(ctx, sub.ID, sub.LastETag, sub.EventsCount, syncErr)
		return err
	}

	// If ETag matches, nothing changed
	if newETag != "" && newETag == sub.LastETag {
		log.Printf("sync: subscription %s unchanged (ETag match)", sub.ID)
		_ = s.store.UpdateSubscriptionSyncState(ctx, sub.ID, newETag, sub.EventsCount, "")
		return nil
	}

	// Parse all events from the feed
	events, err := parseAllEvents(icsData, sub.TenantID, sub.UserID, sub.CalendarID)
	if err != nil {
		syncErr := fmt.Sprintf("parse failed: %v", err)
		log.Printf("sync: %s for subscription %s", syncErr, sub.ID)
		_ = s.store.UpdateSubscriptionSyncState(ctx, sub.ID, sub.LastETag, sub.EventsCount, syncErr)
		return err
	}

	// Replace all events: delete old ones, insert new ones
	// This is the simplest and most reliable approach for external subscriptions
	if err := s.store.DeleteEventsByCalendar(ctx, sub.TenantID, sub.CalendarID); err != nil {
		syncErr := fmt.Sprintf("delete old events: %v", err)
		_ = s.store.UpdateSubscriptionSyncState(ctx, sub.ID, sub.LastETag, sub.EventsCount, syncErr)
		return err
	}

	imported := 0
	for _, event := range events {
		if err := s.store.CreateEvent(ctx, event); err != nil {
			log.Printf("sync: failed to import event %s: %v", event.UID, err)
			continue
		}
		imported++
	}

	log.Printf("sync: subscription %s synced — %d events imported", sub.ID, imported)
	_ = s.store.UpdateSubscriptionSyncState(ctx, sub.ID, newETag, imported, "")
	return nil
}

// SyncOneSingle syncs a single subscription by ID (for manual "sync now" API).
func (s *Service) SyncOne(ctx context.Context, tenantID int, subID string) error {
	sub, err := s.store.GetSubscription(ctx, tenantID, subID)
	if err != nil {
		return err
	}
	if sub == nil {
		return fmt.Errorf("subscription not found")
	}
	return s.SyncSubscription(ctx, sub)
}

// EnqueueDueSyncs checks for subscriptions due for sync and enqueues jobs.
// Called periodically by the scheduler.
func (s *Service) EnqueueDueSyncs(ctx context.Context) error {
	subs, err := s.store.GetDueSubscriptions(ctx, 50)
	if err != nil {
		return fmt.Errorf("sync: get due subscriptions: %w", err)
	}

	for _, sub := range subs {
		payload, _ := json.Marshal(map[string]interface{}{
			"subscription_id": sub.ID,
			"tenant_id":       sub.TenantID,
		})
		if _, err := s.queue.Enqueue(ctx, JobSyncExternalCalendar, payload); err != nil {
			log.Printf("sync: failed to enqueue sync job for %s: %v", sub.ID, err)
		}
	}

	if len(subs) > 0 {
		log.Printf("sync: enqueued %d subscription sync jobs", len(subs))
	}
	return nil
}

// HandleEnqueueDueSyncsJob is the job handler for the periodic scheduler.
func (s *Service) HandleEnqueueDueSyncsJob(ctx context.Context, job *jobs.Job) error {
	return s.EnqueueDueSyncs(ctx)
}

// Job type constants.
const (
	JobSyncExternalCalendar = "sync_external_calendar"
	JobEnqueueDueSyncs      = "enqueue_due_syncs"
)

// ── Feed Fetching ────────────────────────────────────────────

func (s *Service) fetchFeed(ctx context.Context, sub *models.CalendarSubscription) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", sub.SourceURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/calendar, application/calendar+json, */*")

	// Conditional GET
	if sub.LastETag != "" {
		req.Header.Set("If-None-Match", sub.LastETag)
	}

	// Auth
	switch sub.AuthType {
	case "basic":
		parts := strings.SplitN(sub.AuthCredentials, ":", 2)
		if len(parts) == 2 {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(sub.AuthCredentials)))
		}
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+sub.AuthCredentials)
	case "api_key":
		// Append as query param
		q := req.URL.Query()
		q.Set("key", sub.AuthCredentials)
		req.URL.RawQuery = q.Encode()
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("http fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return "", sub.LastETag, nil
	}

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("http %d from %s", resp.StatusCode, sub.SourceURL)
	}

	// Read body with size limit
	lr := io.LimitReader(resp.Body, MaxFeedSize+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}
	if len(body) > MaxFeedSize {
		return "", "", fmt.Errorf("feed too large (>%d bytes)", MaxFeedSize)
	}

	etag := resp.Header.Get("ETag")
	return string(body), etag, nil
}

// ── ICS Parsing ──────────────────────────────────────────────

// parseAllEvents extracts all VEVENT/VTODO components from a multi-event ICS feed.
func parseAllEvents(rawICS string, tenantID, userID int, calendarID string) ([]*models.Event, error) {
	if err := ics.ValidateICS(rawICS); err != nil {
		return nil, err
	}

	// Split the feed into individual VEVENT blocks
	blocks := extractComponentBlocks(rawICS)
	if len(blocks) == 0 {
		return nil, nil // empty calendar, not an error
	}

	var events []*models.Event
	seen := make(map[string]bool)

	for _, block := range blocks {
		// Wrap each block in VCALENDAR for ParseICS
		wrapped := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + block + "\r\nEND:VCALENDAR\r\n"
		event, err := ics.ParseICS(wrapped, tenantID, userID, calendarID)
		if err != nil {
			log.Printf("sync: failed to parse event block: %v", err)
			continue
		}

		// Deduplicate by UID (recurrence instances may share UID — keep all)
		key := event.UID
		if event.RecurrenceID != "" {
			key = event.UID + ":" + event.RecurrenceID
		}
		if seen[key] {
			continue
		}
		seen[key] = true

		// Assign a fresh ID
		event.ID = uuid.New().String()

		// Keep the raw block as the event's RawICS (for faithful re-export)
		event.RawICS = wrapped

		events = append(events, event)
	}

	return events, nil
}

// extractComponentBlocks splits an ICS feed into VEVENT/VTODO blocks.
func extractComponentBlocks(rawICS string) []string {
	var blocks []string
	lines := strings.Split(rawICS, "\n")

	var current strings.Builder
	inComponent := false

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")

		if strings.HasPrefix(line, "BEGIN:VEVENT") || strings.HasPrefix(line, "BEGIN:VTODO") {
			inComponent = true
			current.Reset()
			current.WriteString(line)
			current.WriteString("\r\n")
			continue
		}

		if inComponent {
			current.WriteString(line)
			current.WriteString("\r\n")

			if strings.HasPrefix(line, "END:VEVENT") || strings.HasPrefix(line, "END:VTODO") {
				blocks = append(blocks, current.String())
				inComponent = false
			}
		}
	}

	return blocks
}
