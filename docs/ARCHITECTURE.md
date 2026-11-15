# CalDev Architecture Specification

CalDev is a high-performance, RFC-compliant calendar server written in Go (using Fiber), backed by PostgreSQL for persistence and Valkey (Redis-compatible) for job queues and real-time pub/sub notifications.

---

## 1. High-Level System Topology

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
│ caldev_calendars              │               │ Job Queue (LPUSH / BRPOP):      │
│ caldev_events                 │               │  - Delayed Countdown Reminders  │
│ caldev_sync_changes           │               │  - Periodic 1h Window Sweeps    │
│ caldev_reminder_deliv.        │               │  - Scheduling Email Generation  │
│ caldev_scheduling_msgs        │               ├─────────────────────────────────┤
│ caldev_subscriptions          │               │ Pub/Sub Channels:               │
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

---

## 2. Core Subsystems

### 2.1 CalDAV Engine (RFC 4791 / RFC 4918)
- **Pathing**: `/dav/{tenant_id}/{user_id}/...` and automatic redirect via `/.well-known/caldav`.
- **Methods**: `PROPFIND`, `PROPPATCH`, `REPORT` (calendar-query, calendar-multiget, sync-collection), `MKCALENDAR`, `PUT`, `DELETE`, `MOVE`, `COPY`, `LOCK`, `UNLOCK`.
- **Synchronization**: Sync-tokens (`http://caldev.enjoys.in/sync/...`) for fast incremental sync without querying full event bodies.

### 2.2 JMAP Engine (RFC 8620)
- **Session**: `GET /jmap/session` returns capabilities, account metadata, and API endpoints.
- **API Call**: `POST /jmap/api` supports atomic multi-method invocations:
  - `Calendar/get`, `Calendar/set`
  - `CalendarEvent/get`, `CalendarEvent/set`, `CalendarEvent/query`, `CalendarEvent/changes`
- **Recurrence Overrides in JMAP**: A master series and its overrides are returned as a unified JMAP object where modified instances appear in the `recurrenceOverrides` map, keyed by their recurrence timestamp.

### 2.3 Recurrence & Series Engine
- **Parser & Expander**: Powered by `teambition/rrule-go` strictly following RFC 5545.
- **Supported Rules**: `FREQ` (DAILY, WEEKLY, MONTHLY, YEARLY), `INTERVAL`, `COUNT`, `UNTIL`, `BYDAY`, `BYMONTHDAY`, `BYSETPOS`.
- **Exceptions (`EXDATE`)**: Excluded instances are omitted from generated occurrences.
- **Inclusions (`RDATE`)**: Ad-hoc dates are merged into the series.
- **Instance Overrides (`RECURRENCE-ID`)**:
  - Individual modified instances share the master `UID` and specify `recurrence_id`.
  - `Series.Expand(from, to, limit)` automatically replaces master instances with their override rows.
  - Instances marked `Status: CANCELLED` are excluded from the expanded timeline.
- **Timezone Stability**: Preserves local wall-clock times across Daylight Saving Time (DST) changes.

---

## 3. Server-Side Background Reminders Daemon (Option 3)

The server contains a built-in background reminder daemon designed to fire countdown notifications even when the client browser is closed.

```
                          ┌────────────────────────┐
                          │   1-Hour Cron Ticker   │
                          └───────────┬────────────┘
                                      │
                                      ▼
                      ┌────────────────────────────────┐
                      │    SweepReminders (Store)      │
                      │ Query: [NOW(), NOW() + 24 hrs] │
                      └───────────────┬────────────────┘
                                      │
Event Created / Updated ─────────────┼────────────────────────┐
(On-demand Trigger)                   │                        │
                                      ▼                        ▼
                      ┌──────────────────────────────────────────────┐
                      │          ScheduleEventReminders              │
                      │  - Expands series occurrences in 24h window  │
                      │  - Resolves organizer + active attendees     │
                      │  - Calculates milestones: 15m, 10m, 5m, 0m   │
                      └──────────────────────┬───────────────────────┘
                                             │
                                             ▼
                      ┌──────────────────────────────────────────────┐
                      │    Valkey Queue: EnqueueDelayed              │
                      │    (Delayed until occurrence - milestone)    │
                      └──────────────────────┬───────────────────────┘
                                             │ Delay expires
                                             ▼
                      ┌──────────────────────────────────────────────┐
                      │       HandleCalendarReminderJob              │
                      │  1. Re-read the event: skip if gone,         │
                      │     cancelled, moved, or recipient declined  │
                      │  2. Claim caldev_reminder_deliveries (DEDUP) │
                      │  3. Publish to Redis `calendar:push` channel │
                      │     (claim released on failure → retried)    │
                      └──────────────────────────────────────────────┘
```

### Key Principles:
1. **Strict 24-Hour Scan Window**:
   $$\text{Database Poll Query} \implies \text{WHERE } (\text{dtstart} \ge \text{NOW}() \text{ AND } \text{dtstart} \le \text{NOW}() + 24\text{h}) \text{ OR } (\text{recurrence\_rule} \ne '')$$
   Matches are strictly filtered so neither past events nor events further than 24 hours ahead consume memory or CPU.
2. **Dual-Trigger Architecture**:
   - **Periodic 1-Hour Sweep**: Ticker in `internal/jobs/worker.go` ensures all events within the next 24 hours are enqueued.
   - **On-Demand Immediate Scheduling**: Any event created or updated via JMAP or CalDAV immediately enqueues its countdown milestones without waiting for the next periodic sweep.
3. **Countdown Milestones**:
   - 15 minutes before start
   - 10 minutes before start
   - 5 minutes before start
   - 0 minutes (start time / "Starting now")
4. **Delivery Deduplication**:
   - Backed by PostgreSQL table `caldev_reminder_deliveries`:
     `UNIQUE (event_uid, recurrence_id, recipient_email, milestone_minutes, channel)`.
   - The stored `recurrence_id` is `<recurrence id>@<occurrence start>`, so a meeting moved to a new time gets reminders for the new time instead of being blocked by the old ones.
   - Each delayed job has a deterministic id (tenant, calendar, UID, occurrence, start, recipient, milestone), so the hourly sweep and repeated saves replace a pending reminder instead of adding duplicates.
5. **Who gets reminded**:
   - The organizer's copy reminds the organizer and all non-declined attendees. An attendee's local copy of a meeting organised by a local user schedules nothing (no double reminders); a copy of a meeting from an external organizer reminds only its owner.
   - All-day events get no minute-based countdown reminders.

---

## 4. Scheduling & Multi-Attendee Fan-Out (RFC 6638)

CalDev separates calendar and event management from direct SMTP delivery:

```
Organizer creates event with Attendees
                │
                ├────────────────────────────────────────────────┐
                ▼                                                ▼
     [Attendee is Local User]                         [Attendee is External]
(Email exists in public.mail_accounts)              (Non-system email domain)
                │                                                │
                ▼                                                ▼
     deliverLocal()                                   enqueueMessage()
Automatically creates or updates                 Generates RFC 5545 .ics payload
event copy in Attendee's default                 and stores in caldev_scheduling_messages
writable calendar without email.                                 │
                │                                                ▼
                ▼                                   Publish to Redis:
Enrolls in Server Reminders                         Channel: `calendar:mail`
via `calendar:push` channel.                                     │
                                                                 ▼
                                                    External SMTP Service
                                                    Delivers .ics invite email
```

1. **Internal Attendees**:
   - Discovered automatically by checking `public.mail_accounts`.
   - The event (including all series overrides) is immediately placed in the attendee's primary writable calendar.
   - Internal attendees automatically receive live push notifications and countdown alerts without needing an email.
2. **External Attendees & Dispatch**:
   - CalDev creates an RFC 5545 iMIP payload (`REQUEST`, `REPLY`, or `CANCEL`) and records it in `caldev_scheduling_messages`.
   - **Bring Your Own SMTP (BYO SMTP)**: When the event's calendar has an active custom SMTP configuration (`caldev_calendar_smtp`), the background worker delivers the invitation directly via the user's custom SMTP server with full SPF, DKIM, and DMARC alignment. Emails are delivered as RFC 5322 MIME multipart messages (quoted-printable text and HTML, RFC 2047-encoded headers, optional `Reply-To`) with RSVP buttons on invitations (`REQUEST` only), and the `.ics` component. Each connection has a 60 s deadline.
   - **Retries**: a failed send leaves the message `pending` (error recorded) and the job is retried; it is marked `failed` only after the last attempt. Connection or authentication failures mark the calendar's SMTP config `failing`, after which messages use the managed fallback until it is re-tested.
   - **Managed Fallback**: If no BYO SMTP is configured and managed fallback is active, payloads are published to Redis channel `calendar:mail` for pickup by the centralized mail service.
   - **Web RSVP**: The email buttons open a public HMAC-signed page (`/api/public/rsvp`). Opening it only shows the choice; the recipient confirms with a button (POST), so link-scanning email security tools can't answer on their behalf. The answer updates the organizer's copy and other local attendees' copies and is pushed in real time.
3. **Attendee Capacity & Limits**:
   - CalDev enforces an explicit limit of **3,000 attendees / recipients** per event (`MaxAttendeesPerEvent = 3000`).
   - Any JMAP `CalendarEvent/set` or CalDAV `PUT` containing more than 3,000 attendees is rejected with a `400 Bad Request` (`invalidProperties`).
   - The reminder daemon and fan-out scheduler protect worker queues by strictly bounding recipient fan-out to 3,000 participants.

---

## 5. Database Schema & Tables

All CalDev tables live in the `public` schema with a `caldev_` prefix, next to `public.mail_accounts` (`DB_SCHEMA` is not applied):

- **`caldev_calendars`**: Calendar containers, color, timezone, default calendar flags, and access control.
- **`caldev_events`**: Stores master events and individual `RECURRENCE-ID` overrides. Indexed on `(calendar_id, uid, recurrence_id)`.
- **`caldev_reminder_deliveries`**: Deduplication log ensuring a reminder milestone is delivered at most once per user per instance.
- **`caldev_scheduling_messages`**: Outbox for meeting invitations, replies, and cancellation payloads.
- **`caldev_sync_changes`**: Change log behind CalDAV sync-collection and JMAP `/changes`. Written by triggers on `caldev_events` under a per-tenant advisory lock, so ids commit in order and the JMAP state (`MAX(id)`) never skips a change. Moving an event logs `deleted` in the old calendar and `created` in the new one; deleting a calendar logs nothing (migration `006`).
- **`caldev_subscriptions`**: External read-only calendars imported from Google Calendar, Outlook, or ICS URLs.
- **`caldev_calendar_smtp`**: Outbound BYO SMTP credentials (AES-256-GCM encrypted passwords) per calendar collection.
