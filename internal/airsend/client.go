// Package airsend calls airsend-api's internal routes (service-to-service).
//
// Calendar emails of internal Airsend accounts are sent with
//
//	POST <AIRSEND_INTERNAL_URL>   (https://api.airsend.in/api/v1/internal/calendar/mail)
//	Authorization:      Bearer <token>
//	X-Caldev-Timestamp: <unix seconds>
//	X-Caldev-Signature: hex(HMAC_SHA256(secret, "<timestamp>.<body>"))
//
// The route is on the public domain, so the signature (exact body bytes) and the timestamp
// (±5 min on the receiver) stop a captured request from being changed or replayed.
package airsend

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client sends signed requests to airsend-api.
type Client struct {
	url    string
	token  string
	secret string
	http   *http.Client
}

// New returns a client, or nil when url is empty (feature off).
func New(url, token, secret string, timeout time.Duration) *Client {
	if url == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{url: url, token: token, secret: secret, http: &http.Client{Timeout: timeout}}
}

// Person is a name + email.
type Person struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// Sender is the Airsend account the email is sent from.
type Sender struct {
	AccountID int    `json:"account_id"`
	Email     string `json:"email"`
	Name      string `json:"name,omitempty"`
}

// Attendee is one guest of the event.
type Attendee struct {
	Email  string `json:"email"`
	Name   string `json:"name,omitempty"`
	Role   string `json:"role,omitempty"`
	Status string `json:"status,omitempty"`
}

// Event is the part of the event the email shows.
type Event struct {
	UID            string     `json:"uid"`
	Sequence       int        `json:"sequence"`
	Summary        string     `json:"summary"`
	Description    string     `json:"description,omitempty"`
	Location       string     `json:"location,omitempty"`
	MeetingURL     string     `json:"meeting_url,omitempty"`
	Start          string     `json:"start"`
	End            string     `json:"end,omitempty"`
	AllDay         bool       `json:"all_day"`
	TimeZone       string     `json:"time_zone,omitempty"`
	RecurrenceRule string     `json:"recurrence_rule,omitempty"`
	Organizer      string     `json:"organizer,omitempty"`
	Attendees      []Attendee `json:"attendees,omitempty"`
}

// RSVP holds the one-click answer links of CalDev's public RSVP page for this recipient.
type RSVP struct {
	AcceptURL    string `json:"accept_url"`
	TentativeURL string `json:"tentative_url"`
	DeclineURL   string `json:"decline_url"`
}

// AddToCalendar holds the "Add to calendar" links of an invitation: prefilled Google Calendar
// and Outlook.com pages, and a download of the event's .ics (Apple Calendar and other apps).
type AddToCalendar struct {
	GoogleURL  string `json:"google_url"`
	OutlookURL string `json:"outlook_url"`
	ICSURL     string `json:"ics_url"`
}

// CalendarMail is the v1 request body of POST /internal/calendar/mail.
type CalendarMail struct {
	Version   int    `json:"version"`
	MessageID string `json:"message_id"` // scheduling_messages.id — idempotency key
	Type      string `json:"type"`       // invitation | update | cancellation | reply
	Method    string `json:"method"`     // REQUEST | CANCEL | REPLY
	TenantID  int    `json:"tenant_id"`
	Sender    Sender `json:"sender"`
	Recipient Person `json:"recipient"`
	Event     Event  `json:"event"`
	ICS       string `json:"ics"`
	RSVP      *RSVP  `json:"rsvp,omitempty"`
	Attempt   int    `json:"attempt"`
	CreatedAt string `json:"created_at"`

	// REQUEST only
	AddToCalendar *AddToCalendar `json:"add_to_calendar,omitempty"`
}

// SendError is a failed call. Retryable says whether trying again later can help;
// RetryAfter is the receiver's Retry-After hint (0 = none).
type SendError struct {
	Status     int
	Retryable  bool
	RetryAfter time.Duration
	Message    string
}

func (e *SendError) Error() string {
	if e.Status == 0 {
		return "airsend: " + e.Message
	}
	return fmt.Sprintf("airsend: HTTP %d: %s", e.Status, e.Message)
}

// SendCalendarMail delivers one email request. nil means airsend-api accepted it
// (202) or had already handled this message id (409).
func (c *Client) SendCalendarMail(ctx context.Context, mail *CalendarMail) error {
	body, err := json.Marshal(mail)
	if err != nil {
		return &SendError{Message: "marshal: " + err.Error()}
	}

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return &SendError{Message: "request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Caldev-Timestamp", ts)
	req.Header.Set("X-Caldev-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-Caldev-Message-Id", mail.MessageID)
	req.Header.Set("User-Agent", "CalDev/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		// network error / timeout: airsend-api may be down or restarting
		return &SendError{Retryable: true, Message: err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusConflict:
		return nil // already handled on an earlier attempt
	}

	e := &SendError{Status: resp.StatusCode, Message: responseMessage(respBody)}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode >= 500:
		e.Retryable = true
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	default:
		// 400 / 401 / 403 / 404 / 422: a bad request, token or sender won't fix itself
		e.Retryable = false
	}
	return e
}

func responseMessage(b []byte) string {
	var r struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &r) == nil && r.Message != "" {
		return r.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// parseRetryAfter reads "120" (seconds) or an HTTP date; caps at 6 hours.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	if d < 0 {
		d = 0
	}
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return d
}
