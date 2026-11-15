# CalDev — CalDAV + JMAP Calendar Server

Production-grade calendar server written in Go with [Fiber](https://gofiber.io), PostgreSQL, and Valkey.

Supports Apple Calendar, Thunderbird, Google Calendar interop, and modern JMAP clients.

## Architecture

```
                   ┌─────────────────────────────────────────┐
                   │               Client Tier               │
                   │  - Web Client (imap-web)                │
                   │  - CalDAV Clients (Apple / Thunderbird) │
                   │  - External ICS Subscriptions (Outlook) │
                   └────────────────────┬────────────────────┘
                                        │ HTTP / HTTPS (Port 8443)
                                        ▼
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           CalDev Server (Go / Fiber)                            │
├───────────────────────┬─────────────────────────┬───────────────────────────────┤
│   CalDAV Subsystem    │     JMAP Subsystem      │        REST / Sync API        │
│   (RFC 4791 / 4918)   │   (RFC 8620 / Calendar) │  (Feeds, Availability, Share) │
├───────────────────────┴─────────────────────────┴───────────────────────────────┤
│                               Auth Middleware                                   │
│            JWT (Shared Issuer/Secret)  |  App Passwords (Basic Auth)            │
├─────────────────────────────────────────────────────────────────────────────────┤
│                               Core Engines                                      │
│  - Recurrence Engine: RRULE, EXDATE, RDATE & RECURRENCE-ID Overrides            │
│  - Scheduling Engine: RFC 6638 Internal Fan-out & External iMIP                 │
│  - Reminders Daemon: 24h Window Sweeper & Countdown Milestones (15m, 10m, 5m, 0m│
│  - Live Realtime Engine: SSE EventSource Stream                                 │
└───────────────┬─────────────────────────────────────────────────┬───────────────┘
                │                                                 │
                ▼                                                 ▼
┌───────────────────────────────┐               ┌─────────────────────────────────┐
│     PostgreSQL 16+ Store      │               │    Valkey / Redis (Port 6379)   │
├───────────────────────────────┤               ├─────────────────────────────────┤
│ caldev.caldev_calendars       │               │ Job Queue (LPUSH / BRPOP):      │
│ caldev.caldev_events          │               │  - Delayed Countdown Reminders  │
│ caldev.caldev_changes         │               │  - Periodic 1h Window Sweeps    │
│ caldev.caldev_reminder_deliv. │               │  - Scheduling Email Generation  │
│ caldev.caldev_scheduling_msgs │               ├─────────────────────────────────┤
│ caldev.caldev_subscriptions   │               │ Pub/Sub Channels:               │
│ public.mail_accounts (shared) │               │  - `calendar:push` (Realtime)   │
└───────────────────────────────┘               │  - `calendar:mail` (SMTP Relay) │
                                                └────────────────┬────────────────┘
                                                                 │
                                                                 ▼
                                                ┌─────────────────────────────────┐
                                                │   External Mail / SMTP Worker   │
                                                │  (Processes .ics invites &      │
                                                │   sends email to invitees)      │
                                                └─────────────────────────────────┘
```

## Documentation & Guides

Comprehensive guides for development, architecture, and client/service integration:

| Document | Purpose |
|---|---|
| [**Architecture Specification**](docs/ARCHITECTURE.md) | In-depth topology, database schemas, recurrence engine, reminder daemon, and attendee fan-out. |
| [**Integration Guide**](docs/INTEGRATION_GUIDE.md) | How to integrate CalDev with web apps, external mail relays, push workers, and CalDAV clients. |
| [**Full API Reference**](docs/API_REFERENCE.md) | Complete endpoint specifications, data models, app passwords, subscriptions, and error codes. |
| [**UI Integration Guide**](docs/UI_GUIDE.md) | Complete frontend manual: sign-in, JMAP calls, recurrence overrides, free/busy, and SSE stream. |
| [**API Guide**](API_GUIDE.txt) | Detailed reference for REST, JMAP, and WebDAV routes and payloads. |
| [**Backend Guide**](BACKEND_GUIDE.txt) | Architectural rules and conventions for Go backend development. |

## Key Features

- **CalDAV** — Full RFC 4791/4918 (PROPFIND, PROPPATCH, REPORT, MKCALENDAR, PUT, DELETE, MOVE, COPY, LOCK/UNLOCK)
- **JMAP** — RFC 8620 session + API (CalendarEvent/get, set, query, changes; Calendar/get, set)
- **Recurrence & Series Engine** — Full RFC 5545 recurrence (`FREQ`, `INTERVAL`, `COUNT`, `UNTIL`, `BYDAY`, `EXDATE`, `RDATE`) with single-occurrence modifications and cancellations (`RECURRENCE-ID` overrides)
- **Server-Side Background Reminders** — Built-in background daemon evaluating upcoming events in a strict 24-hour window $[NOW(), NOW() + 24\text{ hours}]$, scheduling countdown milestone alerts (15m, 10m, 5m, 0m) via Redis `calendar:push`
- **Dual Sweeper & Triggering** — 1-hour periodic safety sweep ticker + instant on-demand scheduling when events are created or updated
- **Attendee Fan-Out & Role Demarcation** — Local attendees get meetings directly in their calendar via `deliverLocal`; external attendees have `.ics` invites dispatched via Redis `calendar:mail` for SMTP delivery
- **1 Calendar Per Email** — Enforces single-calendar-per-account architecture with onboarding ("Start Using Calendar") and customizable feature settings
- **Multi-tenant** — Full tenant isolation; `mail_accounts.id` serves as both user ID and tenant ID
- **Shared Users** — Reads from `public.mail_accounts` (same table as your mail system)
- **App Passwords** — CalDAV clients use `email + app-password` for Basic Auth
- **JWT** — Shared secret & issuer with your other services
- **Background Jobs** — Valkey-backed queue with retry, dead-letter, delayed scheduling
- **Sync Tokens** — Efficient delta sync for CalDAV clients
- **ICS Feeds** — Read-only `.ics` feed endpoints for Outlook, Google Calendar, Apple subscription
- **External Calendar Import** — Subscribe to Google Calendar, Outlook, or any ICS URL — events synced automatically
- **One-Command Build & Deploy** — Automated `build.sh` script (builds fresh image, stops old container, runs new container with `.env`)

## Project Structure

```
cal-dev/
├── cmd/
│   ├── server/main.go           # Entry point, routing, graceful shutdown
│   └── demo/demo.go             # Standalone demo (no external deps)
├── docs/
│   ├── API_REFERENCE.md         # Complete endpoint specifications, models & payloads
│   ├── ARCHITECTURE.md          # Complete architectural specification & schemas
│   ├── INTEGRATION_GUIDE.md     # Client and service integration manual
│   └── UI_GUIDE.md              # Frontend UI integration guide
├── internal/
│   ├── auth/auth.go             # JWT (shared issuer) + app password auth + tenant resolution
│   ├── availability/            # Team availability & free/busy calculations
│   ├── caldav/handler.go        # All WebDAV/CalDAV method handlers
│   ├── config/config.go         # Environment-based configuration
│   ├── feeds/handler.go         # ICS feed endpoints for Outlook/Google/Apple
│   ├── ics/ics.go               # RFC 5545 ICS parser & generator
│   ├── jmap/handler.go          # JMAP session + API handlers
│   ├── jobs/
│   │   ├── queue.go             # Valkey-backed job queue (LPUSH/BRPOP)
│   │   └── worker.go            # Worker pool with 1h maintenance sweep & retry
│   ├── middleware/middleware.go  # Security, rate limiting, CORS, auth
│   ├── models/models.go         # Domain models
│   ├── publisher/               # Redis Pub/Sub publisher (calendar:push, calendar:mail)
│   ├── realtime/                # SSE stream (/jmap/eventsource)
│   ├── recurrence/              # Series expansion & RECURRENCE-ID overrides
│   ├── scheduling/              # RFC 6638 fan-out, deliverLocal, & reminder scheduler
│   ├── store/store.go           # PostgreSQL repository layer & reminder queries
│   ├── sync/                    # Sync engine for external calendar feeds
│   └── xmlutil/xmlutil.go       # WebDAV XML parsing & response generation
├── migrations/
│   ├── 001_init.sql                 # Full CalDev schema
│   ├── 002_sharing_realtime.sql     # Availability & calendar shares, LISTEN/NOTIFY
│   ├── 003_recurrence_overrides.sql # One row per RECURRENCE-ID override
│   ├── 004_reminders.sql            # Reminder deliveries deduplication table
│   ├── 005_calendar_byo_smtp.sql    # Per-calendar BYO SMTP settings
│   └── 006_change_log_fixes.sql     # Change log: calendar delete/move, ordered ids
├── build.sh                     # Automated build, stop-old, run-new deployment
├── run.sh                       # Quick run script
├── docker-compose.yml           # PostgreSQL + Valkey + CalDev
├── Dockerfile                   # Multi-stage production build
└── .env.example                 # Environment configuration template
```

## Prerequisites

- Go 1.22+
- PostgreSQL 16+
- Valkey 8+ (or Redis 7+ — wire-compatible)
- Your existing `public.mail_accounts` table with columns: `id (INTEGER PK)`, `email`, `name`

## Quick Start

### 1. Standalone Container Deployment (`build.sh`)

Automated one-command deployment that builds the image first, stops the old container, and starts the new container with `.env`:

```bash
# Configure environment (copy and adjust secrets/DB host)
cp .env.example .env

# Build new image, stop old container, and run new container
./build.sh
```

### 2. Docker Compose (Full Stack with local Valkey)

```bash
# Put the shared JWT secret in .env (compose refuses to start without it; never hard-code it in
# docker-compose.yml — it is shared with the mail services)
echo 'AUTH_JWT_SECRET=your-shared-secret' >> .env

# Start everything
docker-compose up --build -d
```

Valkey is published on `127.0.0.1:6382` only: it has no password, and Docker's published ports bypass host firewalls such as ufw.

### 3. Manual Setup

```bash
# Clone and build
git clone https://github.com/enjoys-in/cal-dev.git
cd cal-dev
go build -o caldev ./cmd/server

# Configure (copy and edit)
cp .env.example .env

# Start (run from the repo root: migrations in ./migrations are applied automatically at startup,
# and startup stops if one is missing or fails)
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
| `DB_SCHEMA` | — | Read but **not applied**: tables live in `public` (switching `search_path` could silently point the app at other tables) |
| `CALDEV_PUBLIC_URL` | `http://localhost:PORT` | **Set in production.** Public base URL used for RSVP links in emails and the JMAP session URLs. While unset (or localhost) the server treats itself as dev and allows CORS from any localhost port |
| `CALDEV_SMTP_ENCRYPTION_KEY` | — | Key for encrypting BYO SMTP passwords. When unset, a key derived from `AUTH_JWT_SECRET` is used (rotating that secret then makes saved SMTP passwords unreadable). Passwords saved before the key was set still decrypt |
| `CORS_ALLOWED_ORIGINS` | — | Extra allowed browser origins, comma-separated |
| `TRUSTED_PROXIES` | loopback + private ranges | IPs/CIDRs of reverse proxies whose client-IP header is trusted (rate limiting, logs) |
| `PROXY_HEADER` | `X-Forwarded-For` | Header the reverse proxy puts the client IP in |
| `REDIS_HOST` | `localhost` | Valkey/Redis host |
| `AUTH_JWT_SECRET` | — | **Required.** Same secret as your mail/auth services. The server refuses to start if it is empty or a known placeholder (`change-me-in-production`, `test`, …) unless `ALLOW_INSECURE_DEFAULTS=true` |
| `AUTH_JWT_ISSUER` | — | JWT issuer to enforce (shared with other services). Strongly recommended |
| `AUTH_REQUIRE_EXP` | `true` | Reject tokens without an `exp` claim. Set `false` only if your auth system issues non-expiring tokens |
| `ALLOW_INSECURE_DEFAULTS` | `false` | Local development only: turn the startup security errors into warnings |
| `AVAILABILITY_ORG_VISIBLE` | `false` | Let users with the same non-empty `mail_accounts.tenant_name` see each other's free/busy without an explicit share. Only enable if `tenant_name` really identifies an organisation |
| `SSE_HEARTBEAT_SECONDS` | `20` | Keep-alive interval for `/jmap/eventsource` |
| `AUTH_BASIC_ENABLED` | `true` | Enable Basic Auth (email + app password); `false` makes `/dav` and `/feeds` refuse Basic credentials |
| `WORKER_CONCURRENCY` | `4` | Background job worker threads (values below 1 become 1) |
| `WORKER_RETRY_MAX` | `3` | Max retry attempts for failed jobs |
| `WORKER_SHUTDOWN_TIMEOUT` | `10s` | How long shutdown waits for running jobs |

Values that fail to parse (for example `SERVER_READ_TIMEOUT=30` without a unit) are logged as warnings and the default is used.

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

Resources are addressed by UID: a `PUT` to `{uid}.ics` must carry that same `UID` in its body (otherwise `400`). `VJOURNAL` is not supported (`403 supported-calendar-component`).

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

### Recurring Events & Exceptions

A calendar object resource (one `.ics`, one CalDAV href) can hold a master `VEVENT` with an `RRULE` plus one `VEVENT` per changed or cancelled occurrence (same `UID`, a `RECURRENCE-ID`). Each component is stored as its own row in `caldev_events`, unique on `(calendar_id, uid, recurrence_id)` (migration `003`):

- the master has `recurrence_id = ''`; an override's `recurrence_id` is the original start of that occurrence, normalised to a UTC instant (`20261116T033000Z`) or a plain date (`20261116`) for all-day series, so the same occurrence always gets the same key whatever zone the client wrote it in;
- the primary row (master, else the earliest override) holds the **whole resource** in `raw_ics` exactly as received, so CalDAV `GET` returns it byte-for-byte; every row of a resource shares its ETag;
- a `PUT` replaces the resource atomically: components are updated in place and components the client dropped are deleted;
- a resource whose components do not share one `UID` is rejected with 400.

Occurrences are expanded server-side (`RRULE`, `EXDATE`, `RDATE`, `COUNT`, `UNTIL`, time zones, DST) and overrides are applied: a moved occurrence moves, a cancelled one disappears, a `TRANSPARENT` or declined one stops counting as busy. This is what `calendar-query`, free/busy, shared calendars and the live availability status use.

JMAP shows one object per series; overrides are folded into it, keyed by the **original** start of the occurrence (RFC 3339 UTC, or `YYYY-MM-DD` for all-day series):

```jsonc
"recurrenceRules": [{ "rule": "FREQ=WEEKLY;BYDAY=MO" }],
"recurrenceOverrides": {
  "2026-11-16T09:00:00Z": { "start": "2026-11-16T14:00:00Z", "title": "Standup (afternoon)" }, // moved/retitled
  "2026-11-23T09:00:00Z": { "excluded": true }                                                // cancelled
}
```

To change one occurrence, `CalendarEvent/set` **update** the series id with `recurrenceOverrides`; set a key to `null` to remove the override. Updating the master never drops existing overrides, alarms or `EXDATE`s. Override rows are not separate JMAP events: updating or destroying one by its row id is refused.

Data stored by earlier versions (where a series and its exceptions were merged into one row) is repaired automatically at startup from the original `raw_ics`; this is idempotent and keeps ETags.

#### Time zones

A timed event is stored as a UTC instant plus, when it has one, the IANA zone its wall-clock times belong to (`properties.tzid`, set from `DTSTART;TZID=…` over CalDAV or from the JMAP `timeZone` property). The zone is what keeps "every Monday 09:00 New York" at 09:00 across daylight-saving changes: expansion uses it, and **every ICS the server writes carries it** — `DTSTART`/`DTEND`/`EXDATE`/`RDATE`/`RECURRENCE-ID` are written as `;TZID=America/New_York:20270301T090000` rather than a fixed UTC time (events without a zone, and all-day events, are unchanged). That includes text regenerated after a JMAP edit, an RSVP or a local invitation, and the iMIP emailed to external attendees.

- JMAP `timeZone`: returned on every timed event (`"Etc/UTC"` for a fixed UTC time), accepted on create and update (IANA or common Windows names; `null`/`""`/`"UTC"` = UTC; unknown names → `invalidProperties`). A recurring event created without one takes its calendar's `timezone` (default `UTC`, i.e. no change); an update only does so when it changes the recurrence rule of an event that has no zone.
- Changing a series' zone keeps every instant fixed, moves the zone of its exceptions with it, and pins `EXDATE`/`RDATE` values written as local time to UTC so they keep pointing at the same occurrence.
- The server writes `TZID` with the IANA name but no `VTIMEZONE` block. Recurring events that were *created through JMAP before `timeZone` existed* have no zone and stay fixed-UTC until a client sets one.

### Team Availability, Sharing & Live Updates

Free/busy is derived from a user's events (recurring series are expanded; `TRANSPARENT`, `CANCELLED` and declined events don't count; unanswered invitations show as `tentative`). Only busy/free blocks are ever shared, never titles or details.

A user can see another user's free/busy only if the other user shared it with them (or, when `AVAILABILITY_ORG_VISIBLE=true`, both have the same non-empty `mail_accounts.tenant_name`). Unknown users and users who haven't shared look identical to the caller, so these endpoints can't be used to discover accounts.

```
GET    /api/availability?emails=a@x,b@x&start=&end=   → busy blocks + current status (RFC 3339, window ≤ 62 days)
GET    /api/availability/shares                        → who you share with / who shares with you
POST   /api/availability/shares   {"email": "..."}     → let that user see your free/busy
DELETE /api/availability/shares/:user_id               → stop sharing

GET    /api/calendars/:id/shares                       → who a calendar is shared with
POST   /api/calendars/:id/shares  {"email": "..."}     → share a calendar read-only
DELETE /api/calendars/:id/shares/:user_id              → revoke
GET    /api/shared/calendars                           → calendars others shared with you
GET    /api/shared/calendars/:id/events?start=&end=    → expanded events (PRIVATE/CONFIDENTIAL shown as "Busy")
```

Standards-based equivalents for native clients: JMAP `Principal/getAvailability`, CalDAV `free-busy-query` REPORT, and RFC 6638 `POST /dav/{tenant}/{user}/outbox/` free-busy requests (the principal advertises `schedule-inbox-URL`, `schedule-outbox-URL`, `calendar-user-address-set`).

**Invitations.** When an organizer invites a user that exists on this server, the event is added straight to that user's default calendar (and a normal iMIP email is still queued). RSVPs update the organizer's copy and the other local invitees' copies; removing an attendee or cancelling removes their copy. An invitation can never overwrite an event the invitee already owns under the same UID unless it comes from the same organizer. Inbound iMIP `REQUEST`/`CANCEL` posted to `/api/scheduling/incoming` is applied to the authenticated user's own calendar under the same rules.

**Live stream** — `GET /jmap/eventsource?access_token=<jwt>` (browsers can't set headers on `EventSource`; this is the only route that accepts a token in the URL). One long-lived SSE connection per client; max 10 per account; the server closes it after 30 minutes and the browser reconnects.

| SSE `event:` | When | Data |
|---|---|---|
| `state` | Your own events or calendars changed (also sent once on connect) | JMAP `StateChange`: `{"changed":{"<acct>":{"CalendarEvent":"<state>","Calendar":"<state>"}}}` — only the types that changed |
| `availability` | A teammate who shares with you changed their schedule, or a meeting started/ended (checked every 30 s); also sent on connect for everyone you can see | `{"@type":"AvailabilityChange","userId":7,"email":"…","busy":true,"busyStatus":"confirmed","until":"…","nextBusyAt":"…"}` |
| `shares` | A share involving you was added or removed | `{"@type":"SharesChanged"}` — re-read the share lists |
| `resync` | You may have missed events (slow client or database reconnect) | refetch everything |

Changes are detected with PostgreSQL `LISTEN/NOTIFY` (migration `002`), so every write path (CalDAV, JMAP, subscription sync, scheduling) triggers updates, and multiple server instances work without extra infrastructure. If you run behind nginx, keep `proxy_buffering off` for this route (the server also sends `X-Accel-Buffering: no`).

## Authentication

### JWT (Bearer)
Tokens issued by your existing auth system work automatically — CalDev validates with the same `AUTH_JWT_SECRET` and `AUTH_JWT_ISSUER`.

The JWT payload should include the `email` claim. If a token has an email that isn't in `mail_accounts`, it is refused; the numeric `user_id`/`sub` is used only for tokens without an email.

```
Authorization: Bearer <jwt-token>
```

### Tenant ID Resolution

The tenant and user are **always taken from the authenticated account** (the `mail_accounts` row matched by the token's `email`, or its `user_id`/`sub`). `tenant_id == mail_accounts.id`. Request headers cannot override this: `X-Tenant-ID` is ignored for any authenticated request, and the `mid`/`tenant_id` token claims are not used for access.

A valid token is still refused (HTTP 401) when:

| Response `code` | Meaning |
|---|---|
| `invalid_token` | Bad signature, wrong issuer, expired, or no `exp` claim (see `AUTH_REQUIRE_EXP`) |
| `user_not_found` | The token's email/id has no row in `mail_accounts` of the database CalDev is connected to |
| `account_inactive` | The `mail_accounts` row's status is not `ACTIVE` |

The server logs the exact cause (never the token) as `auth: 401 ... stage=... code=... cause=...`.

### Basic Auth (CalDAV clients)
For Apple Calendar, Thunderbird, etc. — users create app passwords:

```bash
# Create an app password (requires JWT auth)
curl -X POST http://localhost:8443/api/app-passwords/ \
  -H "Authorization: Bearer <jwt>" \
  -H "Content-Type: application/json" \
  -d '{"label": "MacBook Calendar", "password": "my-app-password"}'
```

After 10 failed logins for one email within 15 minutes, Basic auth for that email is refused for 15 minutes with `429` and `Retry-After` (counted per server instance; a client whose password was verified in the last 5 minutes keeps working). Successful logins are cached for 5 minutes, so a revoked app password can keep working for up to 5 minutes.

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

Delivery is **at-least-once**: taking a job moves it into a `<prefix>:processing` set with a 5-minute deadline in one atomic step, and it is removed only after its result (success, retry scheduled, or dead-lettered) is stored. If a worker crashes or is cut off at shutdown, the job goes back on its queue when the deadline passes. Handlers tolerate a re-run (invitation emails send only while `pending`; reminders are deduplicated). Jobs of unknown type and unparseable payloads go to the dead-letter list.

## Database Schema

All CalDev tables live in the `public` schema with a `caldev_` prefix and reference `mail_accounts` by integer id (no foreign key is enforced, so the mail system can own that table). Migrations in `migrations/` run automatically at startup, in order.

```
mail_accounts (YOUR TABLE — created only for standalone/test setups)
  ├── id (INTEGER PK)     ← serves as both user ID and tenant ID
  ├── email (unique)
  ├── name, tenant_name   ← tenant_name groups users into an organisation (AVAILABILITY_ORG_VISIBLE)
  └── status              ← must be ACTIVE to sign in

caldev_app_passwords          CalDAV Basic-auth passwords
caldev_calendars              calendars (owner = user_id)
caldev_events                 one row per VEVENT/VTODO component, UNIQUE (calendar_id, uid, recurrence_id)
caldev_sync_changes           change log behind CalDAV sync-collection and JMAP changes
caldev_jmap_states            JMAP "Calendar" state tokens
caldev_availability_shares    owner → grantee: free/busy visibility            (002)
caldev_calendar_shares        owner → grantee: read-only calendar              (002)
caldev_scheduling_messages    invitation / reply / cancel mail queue
caldev_calendar_subscriptions external ICS feeds
caldev_locks, caldev_job_history, caldev_migrations
```

---

## API & UI Reference

Detailed API specifications, endpoint payloads, data models, and UI integration patterns are documented in the [docs/](docs/) folder:

- [**Full API Reference**](docs/API_REFERENCE.md) — Endpoint specifications, JSON request/response structures, data models, query parameters, and error responses.
- [**UI Integration Guide**](docs/UI_GUIDE.md) — Step-by-step frontend guide covering JMAP invocations, recurrence overrides, team free/busy availability, and real-time SSE updates.
- [**Integration Guide**](docs/INTEGRATION_GUIDE.md) — External mail relay (Redis `calendar:mail`), push notification daemon (`calendar:push`), and CalDAV client setup.
- [**Architecture Specification**](docs/ARCHITECTURE.md) — System topology, background reminders daemon, and database table schemas.

## License

MIT
