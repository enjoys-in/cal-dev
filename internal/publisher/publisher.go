package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis Pub/Sub channel for calendar mail events.
// The mail server repo subscribes to this channel to send actual emails.
const (
	ChannelCalendarMail = "calendar:mail"
)

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

	// Tracking
	MessageID string `json:"message_id"` // links to caldev_scheduling_messages.id
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

	if err := p.client.Publish(ctx, ChannelCalendarMail, data).Err(); err != nil {
		return fmt.Errorf("publisher: publish to %s: %w", ChannelCalendarMail, err)
	}

	log.Printf("publisher: published %s event for %s → %s (msg=%s)",
		evt.EventType, evt.SenderEmail, evt.RecipientEmail, evt.MessageID)
	return nil
}
