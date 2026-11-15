# CalDev Full API Reference (for UI Development)

This document provides complete endpoint specifications, authentication rules, JMAP/CalDAV payload formats, data models, and error responses needed to build frontend clients for CalDev.

**Base URL:** `http://localhost:8443`

---

## 1. Authentication

Two methods are supported on all endpoints:

| Method | Header | Used By |
|---|---|---|
| **JWT Bearer** | `Authorization: Bearer <jwt-token>` | Frontend apps, JMAP clients |
| **Basic Auth** | `Authorization: Basic base64(email:app-password)` | CalDAV clients, ICS feeds |

JWT tokens are validated using a shared `AUTH_JWT_SECRET` + `AUTH_JWT_ISSUER` (same as your mail/auth system). The JWT payload should contain `sub` (user UUID), `email`, and `mid` (the `mail_accounts.id` UUID used as tenant ID). The `mid` claim takes priority over `tenant_id` when both are present.

Basic Auth requires an **app password** (not the login password). App passwords are managed via the `/api/app-passwords/` endpoints.

All authenticated requests set `user_id`, `tenant_id`, and `email` in the request context. The `tenant_id` is the `mail_accounts.id` UUID, resolved via the X-Tenant-ID header, access_token cookie, or JWT `mid` claim. All data is scoped by `tenant_id` + `user_id`.

---

## 2. CORS

All endpoints return CORS headers:
```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MOVE, COPY, LOCK, UNLOCK
Access-Control-Allow-Headers: Authorization, Content-Type, Depth, If-Match, If-None-Match, Lock-Token, Destination, Overwrite, Prefer, Brief
Access-Control-Expose-Headers: ETag, DAV, Allow, Lock-Token
```

---

## 3. Health Endpoint

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

## 4. App Passwords — `/api/app-passwords/`

Auth: **JWT Bearer** or **Basic Auth**

### `POST /api/app-passwords/`
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

### `DELETE /api/app-passwords/:id`
Delete an app password.

**Response:** `204 No Content`

---

## 5. Calendar Subscriptions — `/api/subscriptions/`

Auth: **JWT Bearer** or **Basic Auth**

Subscribe to external ICS feeds (Google Calendar, Outlook, any public `.ics` URL). Events are synced into a local read-only calendar.

### `GET /api/subscriptions/`
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

### `POST /api/subscriptions/`
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

### `GET /api/subscriptions/:id`
Get a single subscription.

**Response:**
```json
{
  "subscription": { ... }
}
```

**Errors:** `404` — `{"error": "subscription not found"}`

### `PUT /api/subscriptions/:id`
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

### `DELETE /api/subscriptions/:id`
Delete a subscription. Optionally delete the linked calendar and all its events.

**Query params:**
- `delete_calendar=true` — also delete the calendar and all imported events

**Response:** `204 No Content`

### `POST /api/subscriptions/:id/sync`
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

## 6. ICS Feeds — `/feeds/`

Auth: **Basic Auth** (email + app password)

Read-only ICS feeds for subscribing from Outlook, Google Calendar, Apple Calendar.

### `GET /feeds/{tenant}/{user}/`
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

### `GET /feeds/{tenant}/{user}/{calendar}.ics`
Full ICS file for a single calendar. Supports `ETag` + `If-None-Match` (returns `304` if unchanged).

**Response:** `text/calendar; charset=utf-8`

### `GET /feeds/{tenant}/{user}/all.ics`
Merged ICS of all user's calendars. Supports `ETag` + `If-None-Match`.

**Response:** `text/calendar; charset=utf-8`

---

## 7. Calendar BYO SMTP — `/api/calendars/:id/smtp`

Auth: **JWT Bearer** or **Basic Auth**

Manages Bring Your Own SMTP credentials per calendar collection.

### `GET /api/calendars/:id/smtp`
Get current SMTP configuration (password is never exposed).

**Response (Configured):**
```json
{
  "configured": true,
  "id": "uuid",
  "calendar_id": "cal-uuid",
  "host": "smtp.gmail.com",
  "port": 587,
  "username": "user@gmail.com",
  "from_email": "user@gmail.com",
  "from_name": "Alice Smith",
  "encryption": "starttls",
  "status": "active",
  "last_tested_at": "2026-11-15T12:00:00Z",
  "last_error": ""
}
```

**Response (Not Configured):**
```json
{
  "configured": false,
  "calendar_id": "cal-uuid"
}
```

### `PUT /api/calendars/:id/smtp`
Create or update SMTP configuration. Passwords are encrypted with AES-256-GCM.

**Request:**
```json
{
  "host": "smtp.gmail.com",
  "port": 587,
  "username": "user@gmail.com",
  "password": "app-specific-password",
  "from_email": "user@gmail.com",
  "from_name": "Alice Smith",
  "encryption": "starttls"
}
```

### `POST /api/calendars/:id/smtp/test`
Verify SMTP server connectivity, handshake, and authentication.

**Request (Optional payload to test unsaved values):**
```json
{
  "host": "smtp.gmail.com",
  "port": 587,
  "username": "user@gmail.com",
  "password": "secret",
  "encryption": "starttls"
}
```

**Response (Success):**
```json
{
  "success": true,
  "message": "SMTP connection verified successfully"
}
```

### `DELETE /api/calendars/:id/smtp`
Remove SMTP configuration for the calendar.

**Response:** `204 No Content`

---

## 8. Public 1-Click Web RSVP — `/api/scheduling/rsvp`

Auth: **None (Protected by signed HMAC token)**

Allows external email recipients (e.g. Gmail Web, Yahoo) to RSVP directly from email buttons.

### `GET /api/scheduling/rsvp?token=...&action=ACCEPTED|DECLINED|TENTATIVE`
Renders a responsive RSVP confirmation landing page and dispatches realtime updates to the organizer.

---

## 7. JMAP API — `/jmap/`

Auth: **JWT Bearer** or **Basic Auth**

### `GET /jmap/session`
JMAP Session resource (RFC 8620 §2). Returns capabilities, accounts, API URLs.

**Response:**
```json
{
  "capabilities": {
    "urn:ietf:params:jmap:core": { ... },
    "urn:ietf:params:jmap:calendars": {
      "supportsScheduling": true,
      "supportsAvailability": true,
      "maxCalendarsPerEvent": 1,
      "maxAttendeesPerEvent": 3000
    }
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

### `POST /jmap/api`
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
- `CalendarEvent/set` — Create/update/delete events (maximum 3,000 attendees per event; requests exceeding 3,000 return `invalidProperties`)
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

## 8. CalDAV — `/dav/`

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

## 9. Data Models

All JSON field names match these models exactly.

### Calendar
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

### Event
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

### CalendarSubscription
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

### AppPassword
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

## 10. Route Summary Table

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
| `GET` | `/api/calendars/:id/smtp` | Basic/JWT | Get calendar SMTP configuration |
| `PUT` | `/api/calendars/:id/smtp` | Basic/JWT | Upsert calendar SMTP configuration |
| `POST` | `/api/calendars/:id/smtp/test` | Basic/JWT | Test calendar SMTP connectivity |
| `DELETE` | `/api/calendars/:id/smtp` | Basic/JWT | Delete calendar SMTP configuration |
| `GET` | `/api/scheduling/rsvp` | None | Public 1-click web RSVP landing |
| `POST` | `/api/scheduling/rsvp` | Basic/JWT | In-app user RSVP endpoint |
| `POST` | `/api/scheduling/incoming` | Basic/JWT | Inbound iMIP webhook (mail server) |

---

## 11. Error Responses

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
