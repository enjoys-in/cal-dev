# CalDev — CalDAV + JMAP Calendar Server

Production-grade calendar server written in Go with [Fiber](https://gofiber.io), PostgreSQL, and Valkey.

Supports Apple Calendar, Thunderbird, Google Calendar interop, and modern JMAP clients.

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                     CalDev Server (Fiber)                     │
├─────────────────────┬────────────────────┬───────────────────┤
│   CalDAV (RFC 4791) │  JMAP (RFC 8620)   │   App Passwords   │
│   /dav/...          │  /jmap/...         │   /api/...        │
├─────────────────────┴────────────────────┴───────────────────┤
│                      Auth Middleware                          │
│          JWT (shared secret/issuer) + Basic Auth              │
├──────────────────────────────────────────────────────────────┤
│  Store (PostgreSQL)      │      Jobs (Valkey Queue)          │
│  caldev schema           │      Background Workers           │
│  public.mail_accounts    │      Scheduling / Cleanup         │
└──────────────────────────┴───────────────────────────────────┘
```

## Key Features

- **CalDAV** — Full RFC 4791/4918 (PROPFIND, PROPPATCH, REPORT, MKCALENDAR, PUT, DELETE, MOVE, COPY, LOCK/UNLOCK)
- **JMAP** — RFC 8620 session + API (CalendarEvent/get, set, query, changes; Calendar/get, set)
- **ICS** — RFC 5545 iCalendar parser & generator
- **Scheduling** — RFC 6638 invitations/replies/cancellations via SMTP (background jobs)
- **Multi-tenant** — Full tenant isolation; `mail_accounts.id` serves as both user ID and tenant ID
- **Shared Users** — Reads from `public.mail_accounts` (same table as your mail system)
- **App Passwords** — CalDAV clients use `email + app-password` for Basic Auth
- **JWT** — Shared secret & issuer with your other services
- **Background Jobs** — Valkey-backed queue with retry, dead-letter, delayed scheduling
- **Sync Tokens** — Efficient delta sync for CalDAV clients
- **ICS Feeds** — Read-only `.ics` feed endpoints for Outlook, Google Calendar, Apple subscription
- **External Calendar Import** — Subscribe to Google Calendar, Outlook, or any ICS URL — events synced automatically

## Project Structure

```
cal-dev/
├── cmd/
│   ├── server/main.go           # Entry point, routing, graceful shutdown
│   └── demo/demo.go             # Standalone demo (no external deps)
├── internal/
│   ├── auth/auth.go             # JWT (shared issuer) + app password auth + tenant resolution
│   ├── caldav/handler.go        # All WebDAV/CalDAV method handlers
│   ├── config/config.go         # Environment-based configuration
│   ├── feeds/handler.go         # ICS feed endpoints for Outlook/Google/Apple
│   ├── ics/ics.go               # RFC 5545 ICS parser & generator
│   ├── jmap/handler.go          # JMAP session + API handlers
│   ├── jobs/
│   │   ├── queue.go             # Valkey-backed job queue (LPUSH/BRPOP)
│   │   └── worker.go            # Worker pool with retry & dead-letter
│   ├── middleware/middleware.go  # Security, rate limiting, CORS, auth
│   ├── models/models.go         # Domain models
│   ├── scheduling/scheduling.go # RFC 6638 scheduling via job queue
│   ├── store/store.go           # PostgreSQL repository layer
│   ├── sync/
│   │   ├── handler.go           # REST API for external calendar subscriptions
│   │   └── service.go           # Sync engine (fetch, parse, import ICS feeds)
│   └── xmlutil/xmlutil.go       # WebDAV XML parsing & response generation
├── migrations/
│   ├── 000_public_users.sql     # Seed public.mail_accounts table (development)
│   ├── 001_init.sql             # CalDev schema (caldev.*)
│   ├── 002_seed.sql             # Test data seed
│   └── 003_calendar_subscriptions.sql # External subscription table
├── docker-compose.yml           # PostgreSQL + Valkey + CalDev
├── Dockerfile                   # Multi-stage build
└── .env.example                 # All environment variables
```

## Prerequisites

- Go 1.22+
- PostgreSQL 16+
- Valkey 8+ (or Redis 7+ — wire-compatible)
- Your existing `public.mail_accounts` table with columns: `id (UUID PK)`, `email`, `name`

## Quick Start

### 1. Docker Compose (recommended)

```bash
# Set your shared JWT secret
export AUTH_JWT_SECRET="your-shared-secret"
export AUTH_JWT_ISSUER="your-issuer"

# Start everything
docker-compose up --build
```

### 2. Manual Setup

```bash
# Clone and build
git clone https://github.com/enjoys-in/cal-dev.git
cd cal-dev
go build -o caldev ./cmd/server

# Configure (copy and edit)
cp .env.example .env

# Run migrations
psql -U caldev -d caldev -f migrations/001_init.sql

# Start
./caldev
```

### 3. Demo Mode (no external dependencies)

```bash
go run cmd/demo/demo.go
```

Runs an in-memory demo that tests the ICS parser, auth, and prints sample CalDAV/JMAP responses.

## Configuration

All configuration via environment variables. See `.env.example` for the full list.

| Variable | Default | Description |
|---|---|---|
| `SERVER_PORT` | `8443` | HTTP listen port |
| `DB_HOST` | `localhost` | PostgreSQL host |
| `DB_SCHEMA` | `caldev` | PostgreSQL schema name |
| `REDIS_HOST` | `localhost` | Valkey/Redis host |
| `AUTH_JWT_SECRET` | — | **Required.** Same secret as your mail/auth services |
| `AUTH_JWT_ISSUER` | — | JWT issuer to enforce (shared with other services) |
| `AUTH_BASIC_ENABLED` | `true` | Enable Basic Auth (email + app password) |
| `WORKER_CONCURRENCY` | `4` | Background job worker threads |
| `WORKER_RETRY_MAX` | `3` | Max retry attempts for failed jobs |

## API Endpoints

### Health
```
GET /health
```

### CalDAV (RFC 4791)
```
/.well-known/caldav                              → Discovery redirect
/dav/{tenant}/{user}/                            → Principal
/dav/{tenant}/{user}/calendars/                  → Calendar home
/dav/{tenant}/{user}/calendars/{calendar}/       → Calendar collection
/dav/{tenant}/{user}/calendars/{calendar}/{uid}.ics → Event resource
```

Supported methods: `OPTIONS`, `PROPFIND`, `PROPPATCH`, `REPORT`, `GET`, `PUT`, `DELETE`, `MKCALENDAR`, `MOVE`, `COPY`, `LOCK`, `UNLOCK`

### JMAP (RFC 8620)
```
GET  /jmap/session    → Session resource
POST /jmap/api        → Method calls (CalendarEvent/*, Calendar/*)
```

### App Passwords
```
POST   /api/app-passwords/     → Create app password
DELETE /api/app-passwords/:id  → Delete app password
```

### ICS Feeds (Outlook, Google, Apple subscription)
```
GET /feeds/{tenant}/{user}/                        → JSON index of available feeds
GET /feeds/{tenant}/{user}/{calendar}.ics           → Single calendar ICS feed
GET /feeds/{tenant}/{user}/all.ics                  → Merged feed of all calendars
```

These are read-only polling feeds — clients fetch the full `.ics` file periodically. Supports `ETag` / `If-None-Match` for conditional GET (304 Not Modified).

### External Calendar Subscriptions (Import from Google/Outlook/any ICS)
```
GET    /api/subscriptions/          → List all subscriptions
POST   /api/subscriptions/          → Create new subscription
GET    /api/subscriptions/:id       → Get subscription detail
PUT    /api/subscriptions/:id       → Update subscription
DELETE /api/subscriptions/:id       → Delete subscription (?delete_calendar=true)
POST   /api/subscriptions/:id/sync  → Trigger immediate sync
```

## Authentication

### JWT (Bearer)
Tokens issued by your existing auth system work automatically — CalDev validates with the same `AUTH_JWT_SECRET` and `AUTH_JWT_ISSUER`.

The JWT payload should include the `mid` claim (the `mail_accounts.id` UUID), which is used as the tenant ID.

```
Authorization: Bearer <jwt-token>
```

### Tenant ID Resolution

CalDev resolves the tenant ID in this priority order:

1. **`X-Tenant-ID` header** — Explicitly set by the calling service (must be the `mail_accounts.id` UUID)
2. **`access_token` cookie** — JWT is decoded and the `mid` claim is used as tenant ID
3. **Auth fallback** — The `mid` or `tenant_id` claim from the `Authorization` header JWT

```bash
# Option 1: Explicit header
curl -H "X-Tenant-ID: <mail_accounts_id>" -H "Authorization: Bearer <jwt>" ...

# Option 2: Cookie (set by your auth system)
# The access_token cookie is decoded automatically

# Option 3: JWT mid claim (automatic from Authorization header)
curl -H "Authorization: Bearer <jwt-with-mid-claim>" ...
```

### Basic Auth (CalDAV clients)
For Apple Calendar, Thunderbird, etc. — users create app passwords:

```bash
# Create an app password (requires JWT auth)
curl -X POST http://localhost:8443/api/app-passwords/ \
  -H "Authorization: Bearer <jwt>" \
  -H "Content-Type: application/json" \
  -d '{"label": "MacBook Calendar", "password": "my-app-password"}'
```

Then configure the CalDAV client:
- **Server:** `http://localhost:8443/dav/{tenant}/{user}/calendars/`
- **Username:** `user@example.com`
- **Password:** `my-app-password`

## Client Configuration

### Apple Calendar (macOS/iOS)
1. Settings → Accounts → Add Account → Other → CalDAV
2. Server: `localhost:8443`
3. Username: your email
4. Password: your app password
5. Path prefix: `/dav/{tenant}/{user}/calendars/`

### Thunderbird
1. New Calendar → On the Network
2. URL: `http://localhost:8443/.well-known/caldav`
3. Username: your email
4. Password: your app password

### JMAP Clients
```
Session URL: http://localhost:8443/jmap/session
API URL:     http://localhost:8443/jmap/api
```

### Outlook (ICS Feed Subscription)
Outlook does **not** support CalDAV natively. Use the ICS feed subscription:

1. **File** → **Account Settings** → **Account Settings**
2. Go to the **Internet Calendars** tab
3. Click **New…**
4. Paste the feed URL (Basic Auth credentials in the URL):
   ```
   http://user@example.com:app-password@yourserver:8443/feeds/{tenant}/{user}/{calendar}.ics
   ```
5. Give it a name and click **OK**

> **Note:** This is **read-only** — Outlook will poll for changes but cannot create/edit events through this method. For full read-write CalDAV sync, use the [CalDav Synchronizer](https://caldavsynchronizer.org/) Outlook plugin.

### Google Calendar (ICS Feed)
1. Click **+** next to "Other calendars" → **From URL**
2. Paste: `http://user:password@yourserver:8443/feeds/{tenant}/{user}/{calendar}.ics`
3. Click **Add calendar**

## Background Jobs (Valkey)

CPU-intensive tasks run in background workers:

| Job Type | Trigger | Description |
|---|---|---|
| `send_scheduling_email` | Event with attendees created/updated | Sends RFC 6638 iMIP invitations via SMTP |
| `cleanup_locks` | Every 5 minutes | Removes expired WebDAV locks |
| `cleanup_job_history` | Every 5 minutes | Prunes job history older than 7 days |
| `sync_external_calendar` | Per-subscription interval | Fetches ICS feed and imports events |
| `enqueue_due_syncs` | Periodic | Checks for subscriptions due for sync and enqueues them |

Jobs support automatic retry (configurable max + exponential backoff) and dead-letter queuing.

## Database Schema

CalDev uses the `caldev` schema and references `public.mail_accounts`:

```
public.mail_accounts (YOUR TABLE — not managed by CalDev)
  ├── id (UUID PK)        ← serves as both user ID and tenant ID
  ├── email
  └── name

caldev.app_passwords     → FK public.mail_accounts(id)
caldev.calendars         → FK public.mail_accounts(id)
caldev.events            → FK public.mail_accounts(id), caldev.calendars(id)
caldev.sync_changes      → FK caldev.calendars(id)
caldev.scheduling_messages → FK public.mail_accounts(id)
caldev.locks
caldev.jmap_states       → FK public.mail_accounts(id)
caldev.job_history
caldev.calendar_subscriptions → FK caldev.calendars(id), public.mail_accounts(id)
```

---

## Full API Reference (for UI development)

This section provides all the detail needed to build a frontend client.

**Base URL:** `http://localhost:8443`

### Authentication

Two methods are supported on all endpoints:

| Method | Header | Used By |
|---|---|---|
| **JWT Bearer** | `Authorization: Bearer <jwt-token>` | Frontend apps, JMAP clients |
| **Basic Auth** | `Authorization: Basic base64(email:app-password)` | CalDAV clients, ICS feeds |

JWT tokens are validated using a shared `AUTH_JWT_SECRET` + `AUTH_JWT_ISSUER` (same as your mail/auth system). The JWT payload should contain `sub` (user UUID), `email`, and `mid` (the `mail_accounts.id` UUID used as tenant ID). The `mid` claim takes priority over `tenant_id` when both are present.

Basic Auth requires an **app password** (not the login password). App passwords are managed via the `/api/app-passwords/` endpoints.

All authenticated requests set `user_id`, `tenant_id`, and `email` in the request context. The `tenant_id` is the `mail_accounts.id` UUID, resolved via the X-Tenant-ID header, access_token cookie, or JWT `mid` claim. All data is scoped by `tenant_id` + `user_id`.

### CORS

All endpoints return CORS headers:
```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MOVE, COPY, LOCK, UNLOCK
Access-Control-Allow-Headers: Authorization, Content-Type, Depth, If-Match, If-None-Match, Lock-Token, Destination, Overwrite, Prefer, Brief
Access-Control-Expose-Headers: ETag, DAV, Allow, Lock-Token
```

---

### `GET /health`
No authentication required.

**Response:**
```json
{
  "status": "ok",
  "service": "caldev",
  "time": "2026-02-26T12:00:00Z"
}
```

---

### App Passwords — `/api/app-passwords/`

Auth: **JWT Bearer** or **Basic Auth**

#### `POST /api/app-passwords/`
Create an app password for CalDAV/ICS Basic Auth.

**Request:**
```json
{
  "label": "MacBook Calendar",
  "password": "my-app-password"
}
```

**Response (201):**
```json
{
  "id": "uuid",
  "label": "MacBook Calendar",
  "message": "App password created. Use your email + this password for CalDAV Basic Auth."
}
```

**Errors:**
- `400` — `{"error": "password required"}`

#### `DELETE /api/app-passwords/:id`
Delete an app password.

**Response:** `204 No Content`

---

### Calendar Subscriptions — `/api/subscriptions/`

Auth: **JWT Bearer** or **Basic Auth**

Subscribe to external ICS feeds (Google Calendar, Outlook, any public `.ics` URL). Events are synced into a local read-only calendar.

#### `GET /api/subscriptions/`
List all subscriptions for the authenticated user.

**Response:**
```json
{
  "subscriptions": [
    {
      "id": "uuid",
      "tenant_id": "uuid",
      "user_id": "uuid",
      "calendar_id": "uuid",
      "source_type": "google",
      "source_url": "https://calendar.google.com/calendar/ical/.../basic.ics",
      "display_name": "Indian Holidays",
      "color": "#10B981",
      "auth_type": "none",
      "is_enabled": true,
      "sync_interval": 3600,
      "last_synced_at": "2026-02-26T12:00:00Z",
      "last_etag": "",
      "last_error": "",
      "error_count": 0,
      "events_count": 535,
      "created_at": "2026-02-26T10:00:00Z",
      "updated_at": "2026-02-26T12:00:00Z"
    }
  ],
  "help": {
    "google_calendar": "Settings → calendar → Integrate → Secret address in iCal format",
    "outlook": "Settings → Calendar → Shared calendars → Publish a calendar → ICS link",
    "any_ics": "Any public or authenticated .ics feed URL"
  }
}
```

#### `POST /api/subscriptions/`
Create a new subscription. Automatically creates a read-only calendar and triggers the first sync.

**Request:**
```json
{
  "source_url": "https://calendar.google.com/calendar/ical/en.indian%23holiday%40group.v.calendar.google.com/public/basic.ics",
  "display_name": "Indian Holidays",
  "color": "#10B981",
  "source_type": "ics_url",
  "auth_type": "none",
  "auth_credentials": "",
  "sync_interval": 3600,
  "calendar_id": ""
}
```

| Field | Required | Default | Description |
|---|---|---|---|
| `source_url` | **Yes** | — | The ICS feed URL |
| `display_name` | No | `"Imported Calendar"` | Calendar display name |
| `color` | No | `"#6B7280"` | Hex color for UI |
| `source_type` | No | `"ics_url"` | Auto-detects `"google"` or `"outlook"` from URL |
| `auth_type` | No | `"none"` | `"none"`, `"basic"`, `"bearer"`, `"api_key"` |
| `auth_credentials` | No | `""` | Credentials for authenticated feeds |
| `sync_interval` | No | `3600` | Sync interval in seconds (minimum 300) |
| `calendar_id` | No | `""` | Link to existing calendar (if empty, auto-creates one) |

**Response (201):**
```json
{
  "subscription": { ... },
  "calendar_id": "uuid",
  "message": "Subscription created. First sync is running in the background."
}
```

#### `GET /api/subscriptions/:id`
Get a single subscription.

**Response:**
```json
{
  "subscription": { ... }
}
```

**Errors:** `404` — `{"error": "subscription not found"}`

#### `PUT /api/subscriptions/:id`
Update a subscription. All fields are optional (partial update).

**Request:**
```json
{
  "display_name": "New Name",
  "color": "#EF4444",
  "source_url": "https://new-url.ics",
  "auth_type": "basic",
  "auth_credentials": "user:pass",
  "is_enabled": false,
  "sync_interval": 7200
}
```

**Response:**
```json
{
  "subscription": { ... }
}
```

#### `DELETE /api/subscriptions/:id`
Delete a subscription. Optionally delete the linked calendar and all its events.

**Query params:**
- `delete_calendar=true` — also delete the calendar and all imported events

**Response:** `204 No Content`

#### `POST /api/subscriptions/:id/sync`
Trigger an immediate sync. Runs synchronously and returns the updated subscription.

**Response:**
```json
{
  "message": "Sync completed successfully",
  "subscription": {
    "last_synced_at": "2026-02-26T12:30:00Z",
    "events_count": 535,
    "error_count": 0,
    "last_error": ""
  }
}
```

**Errors:** `500` — `{"error": "sync failed", "details": "..."}`

---

### ICS Feeds — `/feeds/`

Auth: **Basic Auth** (email + app password)

Read-only ICS feeds for subscribing from Outlook, Google Calendar, Apple Calendar.

#### `GET /feeds/{tenant}/{user}/`
JSON index of available feeds with subscribe URLs.

**Response:**
```json
{
  "user": "uuid",
  "tenant": "uuid",
  "feeds": [
    {
      "calendar_id": "uuid",
      "name": "Personal",
      "color": "#4F46E5",
      "event_count": 42,
      "feed_url": "/feeds/{tenant}/{user}/{calendar_id}.ics"
    }
  ],
  "merged_feed": "/feeds/{tenant}/{user}/all.ics"
}
```

#### `GET /feeds/{tenant}/{user}/{calendar}.ics`
Full ICS file for a single calendar. Supports `ETag` + `If-None-Match` (returns `304` if unchanged).

**Response:** `text/calendar; charset=utf-8`

#### `GET /feeds/{tenant}/{user}/all.ics`
Merged ICS of all user's calendars. Supports `ETag` + `If-None-Match`.

**Response:** `text/calendar; charset=utf-8`

---

### JMAP — `/jmap/`

Auth: **JWT Bearer** or **Basic Auth**

#### `GET /jmap/session`
JMAP Session resource (RFC 8620 §2). Returns capabilities, accounts, API URLs.

**Response:**
```json
{
  "capabilities": {
    "urn:ietf:params:jmap:core": { ... },
    "urn:ietf:params:jmap:calendars": { ... }
  },
  "accounts": {
    "{tenant}:{user}": {
      "name": "user@example.com",
      "isPersonal": true,
      "isReadOnly": false
    }
  },
  "primaryAccounts": { "urn:ietf:params:jmap:calendars": "{tenant}:{user}" },
  "username": "user@example.com",
  "apiUrl": "http://localhost:8443/jmap/api",
  "downloadUrl": "http://localhost:8443/jmap/download/{accountId}/{blobId}/{name}",
  "uploadUrl": "http://localhost:8443/jmap/upload/{accountId}/",
  "eventSourceUrl": "http://localhost:8443/jmap/eventsource",
  "state": "a1b2c3d4"
}
```

#### `POST /jmap/api`
JMAP API endpoint (RFC 8620 §3). Accepts batched method calls.

**Request:**
```json
{
  "using": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:calendars"],
  "methodCalls": [
    ["Calendar/get", {"accountId": "{tenant}:{user}"}, "c0"],
    ["CalendarEvent/get", {"accountId": "{tenant}:{user}", "ids": ["event-uuid"]}, "e0"],
    ["CalendarEvent/query", {"accountId": "{tenant}:{user}", "filter": {"calendarIds": ["cal-uuid"]}}, "q0"]
  ]
}
```

**Supported methods:**
- `Calendar/get` — Get calendars
- `Calendar/set` — Create/update/delete calendars
- `CalendarEvent/get` — Get events by ID
- `CalendarEvent/set` — Create/update/delete events
- `CalendarEvent/query` — Query events with filters
- `CalendarEvent/changes` — Get changes since a sync state

**Response:**
```json
{
  "methodResponses": [
    ["Calendar/get", { "accountId": "...", "state": "...", "list": [...] }, "c0"],
    ["CalendarEvent/get", { "accountId": "...", "state": "...", "list": [...] }, "e0"]
  ],
  "sessionState": "a1b2c3d4"
}
```

---

### CalDAV — `/dav/`

Auth: **Basic Auth** (email + app password) or **JWT Bearer**

Full RFC 4791/4918 WebDAV/CalDAV implementation. Path structure:

```
/dav/{tenant}/{user}/                             → Principal resource
/dav/{tenant}/{user}/calendars/                   → Calendar home collection
/dav/{tenant}/{user}/calendars/{calendar}/        → Calendar collection
/dav/{tenant}/{user}/calendars/{calendar}/{uid}.ics → Event resource
```

**Supported WebDAV/CalDAV methods:**
- `OPTIONS` — Returns `Allow` + `DAV` headers
- `PROPFIND` — Query properties (Depth: 0, 1)
- `PROPPATCH` — Modify properties (displayname, color, description)
- `REPORT` — `calendar-query`, `calendar-multiget`, `sync-collection`
- `GET` — Retrieve ICS event
- `PUT` — Create/update event (uses `If-Match` ETag for conflict detection)
- `DELETE` — Delete event or calendar
- `MKCALENDAR` — Create a new calendar
- `MOVE` — Move event between calendars
- `COPY` — Copy event
- `LOCK` / `UNLOCK` — WebDAV locking

---

### Data Models

All JSON field names match these models exactly.

#### Calendar
```json
{
  "id": "uuid",
  "tenant_id": "uuid",
  "user_id": "uuid",
  "name": "Personal",
  "description": "My default calendar",
  "color": "#4F46E5",
  "timezone": "UTC",
  "sort_order": 0,
  "ctag": "...",
  "sync_token": "...",
  "is_default": true,
  "is_visible": true,
  "is_readonly": false,
  "supported_components": ["VEVENT", "VTODO"],
  "max_resource_size": 10485760,
  "properties": {},
  "created_at": "2026-02-26T10:00:00Z",
  "updated_at": "2026-02-26T10:00:00Z"
}
```

#### Event
```json
{
  "id": "uuid",
  "tenant_id": "uuid",
  "user_id": "uuid",
  "calendar_id": "uuid",
  "uid": "unique-uid@caldev",
  "summary": "Team Meeting",
  "description": "Weekly standup",
  "location": "Room 42",
  "dtstart": "2026-02-26T14:00:00Z",
  "dtend": "2026-02-26T15:00:00Z",
  "duration": "",
  "all_day": false,
  "recurrence_rule": "FREQ=WEEKLY;BYDAY=MO",
  "recurrence_id": "",
  "sequence": 0,
  "status": "CONFIRMED",
  "transparency": "OPAQUE",
  "classification": "PUBLIC",
  "organizer": "mailto:user@example.com",
  "attendees": [
    {
      "email": "colleague@example.com",
      "display_name": "Jane Doe",
      "role": "REQ-PARTICIPANT",
      "status": "NEEDS-ACTION",
      "rsvp": true,
      "type": "INDIVIDUAL"
    }
  ],
  "categories": ["Work", "Meeting"],
  "priority": 0,
  "url": "",
  "geo": "",
  "dtstamp": "2026-02-26T10:00:00Z",
  "etag": "\"abc123\"",
  "raw_ics": "BEGIN:VCALENDAR...",
  "size_bytes": 1234,
  "component_type": "VEVENT",
  "properties": {},
  "created_at": "2026-02-26T10:00:00Z",
  "updated_at": "2026-02-26T10:00:00Z"
}
```

#### CalendarSubscription
```json
{
  "id": "uuid",
  "tenant_id": "uuid",
  "user_id": "uuid",
  "calendar_id": "uuid",
  "source_type": "google | outlook | ics_url",
  "source_url": "https://...",
  "display_name": "Indian Holidays",
  "color": "#10B981",
  "auth_type": "none | basic | bearer | api_key",
  "is_enabled": true,
  "sync_interval": 3600,
  "last_synced_at": "2026-02-26T12:00:00Z",
  "last_etag": "",
  "last_error": "",
  "error_count": 0,
  "events_count": 535,
  "created_at": "2026-02-26T10:00:00Z",
  "updated_at": "2026-02-26T12:00:00Z"
}
```

> **Note:** `auth_credentials` is never returned in API responses (redacted with `json:"-"`).

#### AppPassword
```json
{
  "id": "uuid",
  "user_id": "uuid",
  "label": "MacBook Calendar",
  "scopes": [],
  "last_used_at": "2026-02-26T12:00:00Z",
  "created_at": "2026-02-26T10:00:00Z"
}
```

> **Note:** `password_hash` is never returned in API responses.

---

### Route Summary Table

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/health` | None | Health check |
| `ALL` | `/.well-known/caldav` | Basic/JWT | CalDAV discovery redirect |
| `*` | `/dav/{tenant}/{user}/...` | Basic/JWT | CalDAV (WebDAV methods) |
| `GET` | `/jmap/session` | Basic/JWT | JMAP session resource |
| `POST` | `/jmap/api` | Basic/JWT | JMAP method calls |
| `GET` | `/feeds/{tenant}/{user}/` | Basic/JWT | ICS feed index (JSON) |
| `GET` | `/feeds/{tenant}/{user}/{cal}.ics` | Basic/JWT | Single calendar ICS feed |
| `GET` | `/feeds/{tenant}/{user}/all.ics` | Basic/JWT | Merged ICS feed |
| `POST` | `/api/app-passwords/` | Basic/JWT | Create app password |
| `DELETE` | `/api/app-passwords/:id` | Basic/JWT | Delete app password |
| `GET` | `/api/subscriptions/` | Basic/JWT | List subscriptions |
| `POST` | `/api/subscriptions/` | Basic/JWT | Create subscription |
| `GET` | `/api/subscriptions/:id` | Basic/JWT | Get subscription |
| `PUT` | `/api/subscriptions/:id` | Basic/JWT | Update subscription |
| `DELETE` | `/api/subscriptions/:id` | Basic/JWT | Delete subscription |
| `POST` | `/api/subscriptions/:id/sync` | Basic/JWT | Trigger manual sync |

---

### Error Responses

All REST API errors follow this format:
```json
{
  "error": "description of the error"
}
```

Sync errors may include additional detail:
```json
{
  "error": "sync failed",
  "details": "HTTP 404: Not Found"
}
```

Common HTTP status codes:
- `200` — Success
- `201` — Created
- `204` — No Content (successful delete)
- `304` — Not Modified (ETag match on feeds)
- `400` — Bad Request (validation error)
- `401` — Unauthorized (missing or invalid credentials)
- `404` — Not Found
- `429` — Too Many Requests (rate limited)
- `500` — Internal Server Error

## License

MIT
