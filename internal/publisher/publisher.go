package publisher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// ChannelCalendarPush is the Redis Pub/Sub channel for reminder notifications: the push gateway
// subscribes to it to dispatch Web Push / notifications. (Invitation emails do not use Redis:
// they go to airsend-api over HTTP, see internal/airsend.)
const ChannelCalendarPush = "calendar:push"

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

// Publisher publishes reminder push events to Redis Pub/Sub (calendar:push).
type Publisher struct {
	client *redis.Client
}

// New creates a new Publisher using an existing redis.Client.
func New(client *redis.Client) *Publisher {
	return &Publisher{client: client}
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
