# CalDev Full API Reference (for UI Development)

This document provides complete endpoint specifications, authentication rules, JMAP/CalDAV payload formats, data models, and error responses needed to build frontend clients for CalDev.

**Base URL:** `http://localhost:8443`

---

## 1. Authentication

| Method | Send | Accepted on | Used by |
|---|---|---|---|
| **JWT** | `Authorization: Bearer <jwt>` header or `access_token` cookie | every authenticated route | Frontend apps, JMAP clients |
| **Basic Auth** | `Authorization: Basic base64(email:app-password)` | `/dav/*`, `/feeds/*` only | CalDAV clients, ICS feeds |

`/api/*` and `/jmap/*` accept only the JWT. A token in the URL (`?access_token=`) is accepted only on `/jmap/eventsource`. Onboarding and the admin routes have their own rules (§9).

JWT tokens are validated using a shared `AUTH_JWT_SECRET` + `AUTH_JWT_ISSUER` (same as your mail/auth system). The token's `email` identifies the user in `mail_accounts`; if a token has an email that isn't there it is refused, and the numeric `user_id`/`sub` is used only for tokens without an email. The `mid` and `tenant_id` claims are not used.

Basic Auth requires an **app password** (not the login password). App passwords are managed via the `/api/app-passwords/` endpoints. 10 failed logins for one email within 15 minutes lock Basic Auth for that email for 15 minutes (`429` with `Retry-After`).

All authenticated requests set `user_id`, `tenant_id`, and `email` in the request context, always from the authenticated account: `tenant_id == user_id ==` the integer `mail_accounts.id`. Headers such as `X-Tenant-ID` are ignored. All data is scoped by `tenant_id` + `user_id`.

---

## 2. CORS

CORS headers (with credentials) are returned only for allowed origins: `https://airsend.in`, `https://calender.airsend.in`, a fixed list of local dev ports, anything in `CORS_ALLOWED_ORIGINS`, and — only in dev (no `CALDEV_PUBLIC_URL`, or a localhost one) — any `http(s)://localhost:<port>` / `127.0.0.1:<port>`.
```
Access-Control-Allow-Origin: <the request's Origin>
Access-Control-Allow-Credentials: true
Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS, PROPFIND, PROPPATCH, REPORT, MKCALENDAR, MOVE, COPY, LOCK, UNLOCK
Access-Control-Expose-Headers: ETag, DAV, Allow, Lock-Token
```

Request bodies are limited to 10 MB, also after decompression. `Content-Encoding: gzip` and `deflate` are accepted; other encodings get `415`.

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

Auth: **JWT** (`Authorization: Bearer` header or `access_token` cookie)

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

Auth: **JWT** (`Authorization: Bearer` header or `access_token` cookie)

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
| `source_url` | **Yes** | — | The ICS feed URL. `webcal://` is stored as `https://`. URLs that resolve to loopback, private, link-local or unspecified addresses are rejected (also checked on update and at every fetch) |
| `display_name` | No | `"Imported Calendar"` | Calendar display name |
| `color` | No | `"#6B7280"` | Hex color for UI |
| `source_type` | No | `"ics_url"` | Auto-detects `"google"` or `"outlook"` from URL |
| `auth_type` | No | `"none"` | `"none"`, `"basic"`, `"bearer"`, `"api_key"` |
| `auth_credentials` | No | `""` | Credentials for authenticated feeds |
| `sync_interval` | No | `3600` | Sync interval in seconds (minimum 300) |
| `calendar_id` | No | `""` | If empty, a new calendar is created. A given id must already be the calendar of one of your subscriptions (`403` otherwise): every sync replaces all events in that calendar, so it must never be a normal calendar |

Each sync applies the feed to the calendar in one transaction, and overlapping syncs of the same calendar run one after another. Events keep their ids across syncs: unchanged events are not rewritten, changed ones are updated in place (reported as modified), and events no longer in the feed are deleted.

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

Auth: **Basic Auth** (email + app password) or **JWT**

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

Auth: **JWT** (`Authorization: Bearer` header or `access_token` cookie)

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

- `encryption` must be `none`, `ssl` or `starttls` (empty: decided by port — `465` → `ssl`, otherwise `starttls`). `port` must be 1–65535 (default `587`). Otherwise `400`.
- `password` may be omitted to keep the saved one, but only when `host`, `port` and `username` are unchanged; changing any of them requires the password again (`400`).
- `reply_to` (optional) is sent as the `Reply-To` header of invitation emails.

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
Verify SMTP server connectivity, handshake, and authentication (60 s limit). The authentication mechanism (PLAIN or LOGIN) is chosen from what the server advertises; credentials are never sent over an unencrypted connection to a non-localhost host.

With no body the saved configuration is tested, and its `status` / `last_tested_at` / `last_error` are updated. With a body, the unsaved values are tested and the saved status is left alone; the saved password is reused only if `host`, `port` and `username` match the saved ones (otherwise send `password`). A failure returns `400` with a generic message; details are in the server log.

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

## 8. Public Web RSVP — `/api/public/rsvp`

Auth: **None (protected by a signed HMAC token)**. No app headers needed.

Lets email recipients (e.g. Gmail Web, Yahoo) answer an invitation from the buttons in a BYO SMTP invitation email. The token is signed with a key derived from `AUTH_JWT_SECRET`, names the event and the attendee, and is valid for 30 days or until a day after the event ends, whichever is later.

### `GET /api/public/rsvp?token=...[&action=ACCEPTED|DECLINED|TENTATIVE]`
Only shows a page — it never records anything, so email security scanners that open links can't answer for the recipient. Without `action` the page offers Accept / Tentative / Decline; with `action` it asks to confirm that choice.

### `POST /api/public/rsvp` (form fields `token`, `action`)
Records the answer (sent by the page's buttons), updates the organizer's copy and other local attendees' copies, and shows a confirmation page. Invalid or expired token: `400`; unknown event or attendee: `404`.

---

## 9. User Onboarding & API Keys

### `POST /api/users/onboard` (same as `POST /api/calendar/onboard`)
Creates the user's account if it doesn't exist yet and makes sure they have a default calendar. No app headers needed. Two ways to authenticate:

| Caller | Send | May onboard |
|---|---|---|
| AirSend apps (internal) | `Authorization: Bearer <jwt>` or the `access_token` cookie | only the token's own email (the body `email` may be omitted; a different one → `403`) |
| External services (can't hold the JWT secret) | `X-API-Key: cdk_…` | any user whose email is in the key's domain (key linked to `admin@customer.com` → `*@customer.com`; other domains → `403`) |

**Request:** `{ "email": "bob@customer.com", "name": "Bob" }`

**Response:**
```json
{ "user_id": 42, "email": "bob@customer.com", "tenant_id": 42,
  "calendar_id": "uuid", "calendars": [ … ] }
```

- New accounts are created `ACTIVE`. Every onboarding sets `tenant_name` to the email's domain (that decides org visibility), also for an existing account — any previous value is overwritten.
- An existing account keeps its status: a suspended account is **not** reactivated, and onboarding it returns `403 account is not active`.
- No credentials → `401`; invalid or revoked key → `401`.

### Managing API keys
A key is shown **once**, when it is created; only its SHA-256 hash is stored. Keys don't expire; revoke them when no longer needed.

**Your own keys** (JWT, normal app headers) — scoped to your email's domain. Not allowed for public webmail domains (gmail.com, outlook.com, …); at most 10 active keys.
```
GET    /api/api-keys                 → { "api_keys": [ … ] }   (key text is never listed)
POST   /api/api-keys   {"label": ""} → 201 { "key": "cdk_…", "api_key": { … } }
DELETE /api/api-keys/:id             → 204 (404 if not yours or already revoked)
```

**Admin** (`X-API-Key: <ADMIN_API_KEY>`) — keys for any owner email, including public domains:
```
GET    /api/admin/api-keys[?owner_email=…]
POST   /api/admin/api-keys   {"owner_email": "admin@customer.com", "label": "Customer CRM"} → 201
DELETE /api/admin/api-keys/:id
POST   /api/admin/users      {"email": "…", "name": "…"}   → onboard any email (no domain limit)
```

---

## 10. JMAP API — `/jmap/`

Auth: **JWT** (`Authorization: Bearer` header or `access_token` cookie)

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

## 11. CalDAV — `/dav/`

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

## 12. Data Models

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

## 13. Route Summary Table

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/health` | None | Health check |
| `ALL` | `/.well-known/caldav` | None (public) | CalDAV discovery (RFC 6764): `307` → `/dav/` |
| `GET` | `/.well-known/jmap` | None (public) | JMAP autodiscovery (RFC 8620 §2.2): `307` → `/jmap/session` |
| `PROPFIND` | `/dav/` | Basic/JWT | DAV context root: returns `current-user-principal` |
| `*` | `/dav/{tenant}/{user}/...` | Basic/JWT | CalDAV (WebDAV methods) |
| `GET` | `/jmap/session` | JWT | JMAP session resource |
| `POST` | `/jmap/api` | JWT | JMAP method calls |
| `GET` | `/feeds/{tenant}/{user}/` | Basic/JWT | ICS feed index (JSON) |
| `GET` | `/feeds/{tenant}/{user}/{cal}.ics` | Basic/JWT | Single calendar ICS feed |
| `GET` | `/feeds/{tenant}/{user}/all.ics` | Basic/JWT | Merged ICS feed |
| `POST` | `/api/app-passwords/` | JWT | Create app password |
| `DELETE` | `/api/app-passwords/:id` | JWT | Delete app password |
| `GET` | `/api/subscriptions/` | JWT | List subscriptions |
| `POST` | `/api/subscriptions/` | JWT | Create subscription |
| `GET` | `/api/subscriptions/:id` | JWT | Get subscription |
| `PUT` | `/api/subscriptions/:id` | JWT | Update subscription |
| `DELETE` | `/api/subscriptions/:id` | JWT | Delete subscription |
| `POST` | `/api/subscriptions/:id/sync` | JWT | Trigger manual sync |
| `GET` | `/api/calendars/:id/smtp` | JWT | Get calendar SMTP configuration |
| `PUT` | `/api/calendars/:id/smtp` | JWT | Upsert calendar SMTP configuration |
| `POST` | `/api/calendars/:id/smtp/test` | JWT | Test calendar SMTP connectivity |
| `DELETE` | `/api/calendars/:id/smtp` | JWT | Delete calendar SMTP configuration |
| `GET` | `/api/public/rsvp` | Signed token | Public web RSVP page (shows choices / asks to confirm) |
| `POST` | `/api/public/rsvp` | Signed token | Public web RSVP: records the answer |
| `POST` | `/api/scheduling/rsvp` | JWT | In-app user RSVP endpoint |
| `POST` | `/api/scheduling/incoming` | JWT | Inbound iMIP webhook (mail server) |
| `POST` | `/api/users/onboard`, `/api/calendar/onboard` | JWT (own email) or `X-API-Key` (key's domain) | Create account + default calendar |
| `GET` | `/api/api-keys` | JWT | List your onboarding API keys |
| `POST` | `/api/api-keys` | JWT | Create an onboarding API key (shown once) |
| `DELETE` | `/api/api-keys/:id` | JWT | Revoke one of your keys |
| `GET` | `/api/admin/api-keys` | Admin key | List all keys (`?owner_email=`) |
| `POST` | `/api/admin/api-keys` | Admin key | Create a key for any owner email |
| `DELETE` | `/api/admin/api-keys/:id` | Admin key | Revoke any key |
| `POST` | `/api/admin/users` | Admin key | Onboard any email |

---

## 14. Error Responses

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
- `403` — Forbidden (e.g. onboarding outside your scope, read-only calendar, suspended account)
- `404` — Not Found
- `409` — Conflict (e.g. too many API keys)
- `412` — Precondition Failed (CalDAV `If-Match` / `If-None-Match`)
- `413` — Request body too large (10 MB, also after decompression)
- `415` — Unsupported `Content-Encoding`
- `429` — Too Many Requests (rate limited, or Basic Auth locked after failed logins)
- `500` — Internal Server Error
