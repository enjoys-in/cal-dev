package sync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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
// ValidateSubscriptionURL validates that a subscription URL uses a supported protocol
// and does not point to internal or private networks (SSRF prevention).
func ValidateSubscriptionURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" && scheme != "webcal" {
		return fmt.Errorf("unsupported protocol: %s", u.Scheme)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return fmt.Errorf("empty hostname")
	}
	if strings.EqualFold(hostname, "localhost") {
		return fmt.Errorf("access to localhost is forbidden")
	}

	ips, err := net.LookupIP(hostname)
	if err == nil {
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				return fmt.Errorf("access to internal address %s is forbidden", ip.String())
			}
		}
	}
	return nil
}

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
				return ValidateSubscriptionURL(req.URL.String())
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

// parseAllEvents extracts all calendar object resources from a multi-event ICS feed.
// In accordance with RFC 4791 §4.1, all components (master and recurrence overrides)
// sharing the same UID are stored together in a single calendar object resource.
func parseAllEvents(rawICS string, tenantID, userID int, calendarID string) ([]*models.Event, error) {
	if err := ics.ValidateICS(rawICS); err != nil {
		return nil, err
	}

	// Split the feed into individual component blocks
	blocks := extractComponentBlocks(rawICS)
	if len(blocks) == 0 {
		return nil, nil // empty calendar, not an error
	}

	// Group blocks by UID
	type uidGroup struct {
		masterBlock    string
		overrideBlocks []string
	}
	groups := make(map[string]*uidGroup)
	var uidOrder []string

	for _, block := range blocks {
		uid := extractUIDFromBlock(block)
		if uid == "" {
			uid = uuid.New().String()
		}
		isOverride := strings.Contains(block, "RECURRENCE-ID")

		g, exists := groups[uid]
		if !exists {
			g = &uidGroup{}
			groups[uid] = g
			uidOrder = append(uidOrder, uid)
		}

		if isOverride {
			g.overrideBlocks = append(g.overrideBlocks, block)
		} else {
			if g.masterBlock == "" {
				g.masterBlock = block
			} else {
				g.overrideBlocks = append(g.overrideBlocks, block)
			}
		}
	}

	var events []*models.Event
	for _, uid := range uidOrder {
		g := groups[uid]
		if g.masterBlock == "" && len(g.overrideBlocks) > 0 {
			g.masterBlock = g.overrideBlocks[0]
			g.overrideBlocks = g.overrideBlocks[1:]
		}

		// Assemble full raw_ics containing master + all overrides
		var fullICS strings.Builder
		fullICS.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//CalDev//CalDAV Server//EN\r\n")
		if g.masterBlock != "" {
			fullICS.WriteString(g.masterBlock)
		}
		for _, ob := range g.overrideBlocks {
			if ob != g.masterBlock {
				fullICS.WriteString(ob)
			}
		}
		fullICS.WriteString("END:VCALENDAR\r\n")

		wrapped := fullICS.String()
		event, err := ics.ParseICS(wrapped, tenantID, userID, calendarID)
		if err != nil {
			log.Printf("sync: failed to parse combined event %s: %v", uid, err)
			continue
		}

		event.ID = uuid.New().String()
		event.UID = uid
		event.RawICS = wrapped
		event.SizeBytes = len(wrapped)
		event.ETag = ics.GenerateETag(wrapped)

		events = append(events, event)
	}

	return events, nil
}

func extractUIDFromBlock(block string) string {
	lines := strings.Split(block, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(strings.ToUpper(l), "UID:") {
			return strings.TrimSpace(l[4:])
		}
	}
	return ""
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
