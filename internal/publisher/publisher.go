package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis Pub/Sub channels for calendar events.
// The mail server repo subscribes to calendar:mail to send actual emails.
// The push gateway subscribes to calendar:push to dispatch Web Push / notifications.
const (
	ChannelCalendarMail = "calendar:mail"
	ChannelCalendarPush = "calendar:push"
)

// PushEvent represents a real-time calendar reminder notification dispatched to Redis.
type PushEvent struct {
	EventUID         string `json:"event_uid"`
	RecurrenceID     string `json:"recurrence_id,omitempty"`
	Title            string `json:"title"`
	Location         string `json:"location,omitempty"`
	StartUTC         string `json:"start_utc"`
	RecipientEmail   string `json:"recipient_email"`
	RecipientUserID  int    `json:"recipient_user_id,omitempty"`
	MilestoneMinutes int    `json:"milestone_minutes"` // 15, 10, 5, 0
	Body             string `json:"body"`
	Tag              string `json:"tag"`
	URL              string `json:"url"`
	Timestamp        string `json:"timestamp"`
}

// MailEvent is the payload published to Redis for the mail server to pick up.
// Contains everything needed to compose and send the calendar email.
type MailEvent struct {
	// Event metadata
	EventType string `json:"event_type"` // "invitation", "update", "cancellation", "reply"
	Method    string `json:"method"`     // iCalendar method: REQUEST, CANCEL, REPLY

	// Tenant / sender context
	TenantID    int    `json:"tenant_id"`
	SenderID    int    `json:"sender_id"`
	SenderEmail string `json:"sender_email"` // organizer or replying attendee

	// Recipient
	RecipientEmail string `json:"recipient_email"`

	// Event details (so mail server can compose a nice email body)
	EventUID    string     `json:"event_uid"`
	Summary     string     `json:"summary"`
	Description string     `json:"description,omitempty"`
	Location    string     `json:"location,omitempty"`
	DTStart     string     `json:"dtstart"`         // RFC3339
	DTEnd       string     `json:"dtend,omitempty"` // RFC3339
	AllDay      bool       `json:"all_day"`
	Organizer   string     `json:"organizer"`
	Attendees   []Attendee `json:"attendees,omitempty"`

	// The raw ICS attachment (text/calendar) for the email
	RawICS string `json:"raw_ics"`

	// One-click web RSVP links for this recipient (REQUEST only). They open CalDev's
	// public /api/public/rsvp page; the token inside is signed and tied to this guest.
	RSVPAcceptURL    string `json:"rsvp_accept_url,omitempty"`
	RSVPTentativeURL string `json:"rsvp_tentative_url,omitempty"`
	RSVPDeclineURL   string `json:"rsvp_decline_url,omitempty"`

	// Tracking
	MessageID string `json:"message_id"` // links to scheduling_messages.id
	Timestamp string `json:"timestamp"`  // ISO 8601
}

// Attendee is a lightweight attendee for the mail payload.
type Attendee struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
	Status      string `json:"status,omitempty"`
}

// Publisher publishes calendar mail events to Redis Pub/Sub.
type Publisher struct {
	client *redis.Client
}

// New creates a new Publisher using an existing redis.Client.
func New(client *redis.Client) *Publisher {
	return &Publisher{client: client}
}

// PublishMailEvent publishes a MailEvent to the calendar:mail channel.
func (p *Publisher) PublishMailEvent(ctx context.Context, evt *MailEvent) error {
	if evt.Timestamp == "" {
		evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("publisher: marshal mail event: %w", err)
	}

	receivers, err := p.client.Publish(ctx, ChannelCalendarMail, data).Result()
	if err != nil {
		return fmt.Errorf("publisher: publish to %s: %w", ChannelCalendarMail, err)
	}
	if receivers == 0 {
		// Pub/Sub does not buffer: with no mail service listening the email would be lost.
		// Returning an error keeps the message pending so the job is retried.
		return fmt.Errorf("publisher: no subscriber on %s, email not delivered", ChannelCalendarMail)
	}

	log.Printf("publisher: published %s event for %s → %s (msg=%s)",
		evt.EventType, evt.SenderEmail, evt.RecipientEmail, evt.MessageID)
	return nil
}

// PublishPushEvent publishes a PushEvent to the calendar:push channel.
func (p *Publisher) PublishPushEvent(ctx context.Context, evt *PushEvent) error {
	if evt.Timestamp == "" {
		evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("publisher: marshal push event: %w", err)
	}

	receivers, err := p.client.Publish(ctx, ChannelCalendarPush, data).Result()
	if err != nil {
		return fmt.Errorf("publisher: publish to %s: %w", ChannelCalendarPush, err)
	}
	if receivers == 0 {
		// Pub/Sub does not buffer: with no subscriber the reminder would be lost.
		return fmt.Errorf("publisher: no subscriber on %s, reminder not delivered", ChannelCalendarPush)
	}

	log.Printf("publisher: published reminder push for %s (event=%s, milestone=%dm)",
		evt.RecipientEmail, evt.EventUID, evt.MilestoneMinutes)
	return nil
}
