package models

import (
	"encoding/json"
	"time"
)

// User is an account in CalDev's own table (<schema>.mail_accounts). An internal account is a
// copy of the mail system's public.mail_accounts row (same id and email); an external one was
// created by CalDev's onboarding API and exists only in CalDev.
type User struct {
	ID            int       `json:"id" db:"id"`
	Email         string    `json:"email" db:"email"`
	Name          string    `json:"name" db:"name"`
	TenantName    string    `json:"tenant_name" db:"tenant_name"`
	Status        string    `json:"status" db:"status"` // ACTIVE | INACTIVE | SUSPENDED (CalDev's own)
	Type          string    `json:"type" db:"type"`
	AccountOrigin string    `json:"account_origin" db:"account_origin"` // internal | external
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
	UpdatedAt     time.Time `json:"updated_at" db:"updated_at"`
}

// APIKey lets an external service onboard users of one email domain (api_keys).
// Only the key's hash is stored; the key itself is returned once, at creation.
type APIKey struct {
	ID         string     `json:"id"`
	OwnerEmail string     `json:"owner_email"`
	Domain     string     `json:"domain"`
	Label      string     `json:"label"`
	KeyPrefix  string     `json:"key_prefix"`
	CreatedBy  *int       `json:"created_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// AppPassword is a CalDAV/JMAP app-specific password (like Nextcloud app passwords).
// Stored in app_passwords, linked to mail_accounts.
type AppPassword struct {
	ID           string     `json:"id" db:"id"`
	UserID       int        `json:"user_id" db:"user_id"`
	Label        string     `json:"label" db:"label"`
	PasswordHash string     `json:"-" db:"password_hash"`
	Scopes       []string   `json:"scopes" db:"scopes"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty" db:"last_used_at"`
	CreatedAt    time.Time  `json:"created_at" db:"created_at"`
}

// Job represents a background job dispatched to Valkey.
type Job struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	Retries int             `json:"retries"`
	Created time.Time       `json:"created"`
}

// JobResult is stored in job_history after processing.
type JobResult struct {
	ID           string          `json:"id" db:"id"`
	JobType      string          `json:"job_type" db:"job_type"`
	Payload      json.RawMessage `json:"payload" db:"payload"`
	Status       string          `json:"status" db:"status"`
	ErrorMessage string          `json:"error_message" db:"error_message"`
	DurationMs   int             `json:"duration_ms" db:"duration_ms"`
	CreatedAt    time.Time       `json:"created_at" db:"created_at"`
}

// Calendar represents a CalDAV calendar collection.
type Calendar struct {
	ID                  string          `json:"id" db:"id"`
	TenantID            int             `json:"tenant_id" db:"tenant_id"`
	UserID              int             `json:"user_id" db:"user_id"`
	Name                string          `json:"name" db:"name"`
	Description         string          `json:"description" db:"description"`
	Color               string          `json:"color" db:"color"`
	Timezone            string          `json:"timezone" db:"timezone"`
	SortOrder           int             `json:"sort_order" db:"sort_order"`
	CTag                string          `json:"ctag" db:"ctag"`
	SyncToken           string          `json:"sync_token" db:"sync_token"`
	IsDefault           bool            `json:"is_default" db:"is_default"`
	IsVisible           bool            `json:"is_visible" db:"is_visible"`
	IsReadOnly          bool            `json:"is_readonly" db:"is_readonly"`
	SupportedComponents []string        `json:"supported_components" db:"supported_components"`
	MaxResourceSize     int             `json:"max_resource_size" db:"max_resource_size"`
	Properties          json.RawMessage `json:"properties" db:"properties"`
	HasByoSmtp          bool            `json:"has_byo_smtp" db:"has_byo_smtp"`
	ByoSmtpFrom         string          `json:"byo_smtp_from" db:"byo_smtp_from"`
	CreatedAt           time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at" db:"updated_at"`
}

// CalendarSMTP represents the outbound SMTP configuration for a calendar.
type CalendarSMTP struct {
	ID                string     `json:"id" db:"id"`
	CalendarID        string     `json:"calendar_id" db:"calendar_id"`
	TenantID          int        `json:"tenant_id" db:"tenant_id"`
	UserID            int        `json:"user_id" db:"user_id"`
	Host              string     `json:"host" db:"host"`
	Port              int        `json:"port" db:"port"`
	Username          string     `json:"username" db:"username"`
	PasswordEncrypted string     `json:"-" db:"password_encrypted"`
	Password          string     `json:"password,omitempty" db:"-"` // Only used in API request payloads
	FromEmail         string     `json:"from_email" db:"from_email"`
	FromName          string     `json:"from_name" db:"from_name"`
	ReplyTo           string     `json:"reply_to" db:"reply_to"`
	Encryption        string     `json:"encryption" db:"encryption"` // 'none', 'ssl', 'starttls'
	Status            string     `json:"status" db:"status"`         // 'active', 'failing', 'disabled'
	LastTestedAt      *time.Time `json:"last_tested_at" db:"last_tested_at"`
	LastError         string     `json:"last_error" db:"last_error"`
	CreatedAt         time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at" db:"updated_at"`
}

// Event represents a calendar event (iCalendar resource).
type Event struct {
	ID             string          `json:"id" db:"id"`
	TenantID       int             `json:"tenant_id" db:"tenant_id"`
	UserID         int             `json:"user_id" db:"user_id"`
	CalendarID     string          `json:"calendar_id" db:"calendar_id"`
	UID            string          `json:"uid" db:"uid"`
	Summary        string          `json:"summary" db:"summary"`
	Description    string          `json:"description" db:"description"`
	Location       string          `json:"location" db:"location"`
	DTStart        time.Time       `json:"dtstart" db:"dtstart"`
	DTEnd          *time.Time      `json:"dtend,omitempty" db:"dtend"`
	Duration       string          `json:"duration,omitempty" db:"duration"`
	AllDay         bool            `json:"all_day" db:"all_day"`
	RecurrenceRule string          `json:"recurrence_rule,omitempty" db:"recurrence_rule"`
	RecurrenceID   string          `json:"recurrence_id,omitempty" db:"recurrence_id"`
	Sequence       int             `json:"sequence" db:"sequence"`
	Status         string          `json:"status" db:"status"`
	Transparency   string          `json:"transparency" db:"transparency"`
	Classification string          `json:"classification" db:"classification"`
	Organizer      string          `json:"organizer,omitempty" db:"organizer"`
	Attendees      json.RawMessage `json:"attendees" db:"attendees"`
	Categories     []string        `json:"categories" db:"categories"`
	Priority       int             `json:"priority" db:"priority"`
	URL            string          `json:"url,omitempty" db:"url"`
	Geo            string          `json:"geo,omitempty" db:"geo"`
	DTStamp        time.Time       `json:"dtstamp" db:"dtstamp"`
	ETag           string          `json:"etag" db:"etag"`
	RawICS         string          `json:"raw_ics" db:"raw_ics"`
	SizeBytes      int             `json:"size_bytes" db:"size_bytes"`
	ComponentType  string          `json:"component_type" db:"component_type"`
	Properties     json.RawMessage `json:"properties" db:"properties"`
	CreatedAt      time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at" db:"updated_at"`
}

// SyncChange represents a change tracked for sync tokens.
type SyncChange struct {
	ID         int64     `json:"id" db:"id"`
	TenantID   int       `json:"tenant_id" db:"tenant_id"`
	CalendarID string    `json:"calendar_id" db:"calendar_id"`
	EventUID   string    `json:"event_uid" db:"event_uid"`
	ChangeType string    `json:"change_type" db:"change_type"`
	SyncToken  string    `json:"sync_token" db:"sync_token"`
	ChangedAt  time.Time `json:"changed_at" db:"changed_at"`
}

// SchedulingMessage represents a scheduling invitation/reply.
type SchedulingMessage struct {
	ID             string     `json:"id" db:"id"`
	TenantID       int        `json:"tenant_id" db:"tenant_id"`
	SenderID       int        `json:"sender_id" db:"sender_id"`
	RecipientEmail string     `json:"recipient_email" db:"recipient_email"`
	EventUID       string     `json:"event_uid" db:"event_uid"`
	Method         string     `json:"method" db:"method"`
	Status         string     `json:"status" db:"status"`
	RawICS         string     `json:"raw_ics" db:"raw_ics"`
	ErrorMessage   string     `json:"error_message" db:"error_message"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	SentAt         *time.Time `json:"sent_at,omitempty" db:"sent_at"`
}

// Lock represents a WebDAV lock.
type Lock struct {
	ID             string    `json:"id" db:"id"`
	TenantID       int       `json:"tenant_id" db:"tenant_id"`
	ResourcePath   string    `json:"resource_path" db:"resource_path"`
	LockToken      string    `json:"lock_token" db:"lock_token"`
	Owner          string    `json:"owner" db:"owner"`
	LockType       string    `json:"lock_type" db:"lock_type"`
	LockScope      string    `json:"lock_scope" db:"lock_scope"`
	Depth          string    `json:"depth" db:"depth"`
	TimeoutSeconds int       `json:"timeout_seconds" db:"timeout_seconds"`
	CreatedAt      time.Time `json:"created_at" db:"created_at"`
	ExpiresAt      time.Time `json:"expires_at" db:"expires_at"`
}

// JMAPState tracks sync state for JMAP.
type JMAPState struct {
	ID         string    `json:"id" db:"id"`
	TenantID   int       `json:"tenant_id" db:"tenant_id"`
	UserID     int       `json:"user_id" db:"user_id"`
	EntityType string    `json:"entity_type" db:"entity_type"`
	State      string    `json:"state" db:"state"`
	UpdatedAt  time.Time `json:"updated_at" db:"updated_at"`
}

// EventFilter is used for querying events (REPORT).
type EventFilter struct {
	CalendarID     string
	TenantID       int
	UserID         int
	ComponentType  string
	TimeRangeStart *time.Time
	TimeRangeEnd   *time.Time
	UpdatedSince   *time.Time
	UIDs           []string
	Limit          int
	Offset         int
}

// MaxAttendeesPerEvent defines the maximum supported attendees/recipients per calendar event.
const MaxAttendeesPerEvent = 3000

// Attendee represents a calendar event attendee.
type Attendee struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`   // REQ-PARTICIPANT, OPT-PARTICIPANT, etc.
	Status      string `json:"status"` // NEEDS-ACTION, ACCEPTED, DECLINED, TENTATIVE
	RSVP        bool   `json:"rsvp"`
	Type        string `json:"type"` // INDIVIDUAL, GROUP, RESOURCE, ROOM
}

// CalendarSubscription represents an external calendar feed subscription.
// Users can subscribe to ICS URLs (Google Calendar, Outlook, etc.)
// and events are periodically synced into a local calendar.
type CalendarSubscription struct {
	ID              string     `json:"id" db:"id"`
	TenantID        int        `json:"tenant_id" db:"tenant_id"`
	UserID          int        `json:"user_id" db:"user_id"`
	CalendarID      string     `json:"calendar_id" db:"calendar_id"`
	SourceType      string     `json:"source_type" db:"source_type"` // ics_url, google, outlook
	SourceURL       string     `json:"source_url" db:"source_url"`
	DisplayName     string     `json:"display_name" db:"display_name"`
	Color           string     `json:"color" db:"color"`
	AuthType        string     `json:"auth_type" db:"auth_type"` // none, basic, bearer, api_key
	AuthCredentials string     `json:"-" db:"auth_credentials"`  // never expose
	IsEnabled       bool       `json:"is_enabled" db:"is_enabled"`
	SyncInterval    int        `json:"sync_interval" db:"sync_interval"` // seconds
	LastSyncedAt    *time.Time `json:"last_synced_at,omitempty" db:"last_synced_at"`
	LastETag        string     `json:"last_etag" db:"last_etag"`
	LastError       string     `json:"last_error,omitempty" db:"last_error"`
	ErrorCount      int        `json:"error_count" db:"error_count"`
	EventsCount     int        `json:"events_count" db:"events_count"`
	CreatedAt       time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at" db:"updated_at"`
}

// Principal represents a WebDAV principal resource.
type Principal struct {
	Path            string
	DisplayName     string
	CalendarHomeSet string
}

// CalDAVResource represents a resource returned in PROPFIND/REPORT responses.
type CalDAVResource struct {
	Href       string
	Properties map[string]interface{}
	Status     int
}

// JMAPRequest represents an incoming JMAP request.
type JMAPRequest struct {
	Using       []string          `json:"using"`
	MethodCalls []json.RawMessage `json:"methodCalls"`
}

// JMAPResponse represents a JMAP response.
type JMAPResponse struct {
	MethodResponses []interface{} `json:"methodResponses"`
	SessionState    string        `json:"sessionState"`
}

// JMAPSession represents a JMAP session resource.
type JMAPSession struct {
	Capabilities    map[string]interface{} `json:"capabilities"`
	Accounts        map[string]interface{} `json:"accounts"`
	PrimaryAccounts map[string]string      `json:"primaryAccounts"`
	Username        string                 `json:"username"`
	APIURL          string                 `json:"apiUrl"`
	DownloadURL     string                 `json:"downloadUrl"`
	UploadURL       string                 `json:"uploadUrl"`
	EventSourceURL  string                 `json:"eventSourceUrl"`
	State           string                 `json:"state"`
}
