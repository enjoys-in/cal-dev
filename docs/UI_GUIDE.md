# CalDev — UI Integration Guide

How to build the calendar UI on top of CalDev: sign-in, calendars, events (including recurring events and single-occurrence edits), invitations and RSVP, team free/busy, sharing, and the live stream that keeps all of it up to date.

Everything here was checked against the running server. `API_GUIDE.txt` is the older reference for the basics; where the two disagree, this guide wins.

**Contents**

1. [The 60-second picture](#1-the-60-second-picture)
2. [Connecting: auth, headers, errors](#2-connecting-auth-headers-errors)
3. [Start-up sequence](#3-start-up-sequence)
4. [Calendars](#4-calendars)
5. [Events](#5-events)
6. [Recurring events and "edit this occurrence"](#6-recurring-events-and-edit-this-occurrence)
7. [Invitations and RSVP](#7-invitations-and-rsvp)
8. [Team availability: free / busy](#8-team-availability-free--busy)
9. [Sharing calendars](#9-sharing-calendars)
10. [Live updates (SSE)](#10-live-updates-sse)
11. [Time and all-day rules](#11-time-and-all-day-rules)
12. [Limits and known gaps](#12-limits-and-known-gaps)
13. [Endpoint cheat-sheet](#13-endpoint-cheat-sheet)

---

## 1. The 60-second picture

| You want to… | Use |
|---|---|
| Read/write calendars and events | **JMAP** `POST /jmap/api` |
| Show teammates as busy/free | **REST** `GET /api/availability` + the SSE `availability` event |
| Share free/busy or a calendar | **REST** `/api/availability/shares`, `/api/calendars/:id/shares` |
| Accept/decline an invitation | **REST** `POST /api/scheduling/rsvp` |
| Know when anything changed | **SSE** `GET /jmap/eventsource` (one stream per browser) |

Rules of thumb:

- **One object per series.** A recurring event with exceptions is a single JMAP event; its exceptions live in `recurrenceOverrides`. Never expect exceptions as separate events.
- **Free/busy never reveals details.** Teammates see time blocks only. A calendar shared with someone shows `PRIVATE`/`CONFIDENTIAL` events as plain "Busy".
- **The server tells you who may be seen.** Do not guess; ask. Unknown users and users who have not shared with you look identical (`allowed: false`) on purpose.
- **Users come from the mail system.** CalDev has no sign-up. A signed-in mail user is recognised automatically; a calendar is created on first use.

---

## 2. Connecting: auth, headers, errors

### Authentication

Send the mail system's JWT in one of three ways, in this order of preference:

1. `Authorization: Bearer <jwt>` header
2. the `access_token` cookie (use `fetch(..., { credentials: "include" })`)
3. `?access_token=<jwt>` query parameter — accepted **only** on `/jmap/eventsource` (browsers cannot set headers on `EventSource`). Any other route ignores it and answers `401`.

Tokens **must carry an `exp` claim** (unless the server sets `AUTH_REQUIRE_EXP=false`).

The server decides who you are from the token's `email` (falling back to `user_id`/`sub`), looked up in `mail_accounts`. Nothing you send in headers can change that.

### Required headers

For every `/api/*` and `/jmap/*` call (not for `/jmap/eventsource`, `/dav/*`, `/feeds/*`):

```
X-App-Name: AirSend
X-App-Version: 1.0.0
```

plus `x-api-key` and `x-signature` if the deployment configures them. Without them you get `400 Invalid or missing X-App-Name` (or `401 … x-api-key`) — that is not an auth problem.

### CORS

Allowed origins: `https://airsend.in`, `https://calender.airsend.in`, a fixed list of local dev ports (3000, 3001, 9500, 9503), anything in the server's `CORS_ALLOWED_ORIGINS`, and — on a dev server only (`CALDEV_PUBLIC_URL` is a localhost one; unset means the default `https://calender.airsend.in`) — any `http(s)://localhost:<port>` / `127.0.0.1:<port>`. Cookies work cross-origin only for those origins.

### Error shapes

**REST (`/api/*`)**

```json
{ "success": false, "message": "calendar not found", "result": null, "error": { "details": "…" } }
```

Success is `{ "success": true, "message": "…", "result": { … } }`. Always read `result`.

**JMAP method errors** are returned *inside* a 200 response:

```json
{ "methodResponses": [["error", { "type": "invalidProperties", "description": "…" }, "c1"]], "sessionState": "…" }
```

Per-object failures inside `CalendarEvent/set` are in `notCreated` / `notUpdated` / `notDestroyed`, keyed by id, each `{ type, description }`.

**401 from authentication** — JSON `{ "error": "…", "code": "…" }`:

| `code` | Meaning | What the UI should do |
|---|---|---|
| *(none)* `Authentication required` | no token reached the server | send the user to sign-in |
| `invalid_token` | bad signature, wrong issuer, expired, or no `exp` | refresh the token / sign in again |
| `user_not_found` | valid token, but the email has no account in this calendar service | show "calendar not enabled for this account"; **do not** loop on refresh |
| `account_inactive` | account suspended | same as above |

```ts
async function jmap(methods: unknown[]) {
  const res = await fetch(`${BASE}/jmap/api`, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json", "X-App-Name": "AirSend", "X-App-Version": "1.0.0" },
    body: JSON.stringify({ using: ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:calendars"], methodCalls: methods }),
  });
  if (res.status === 401) throw new AuthError((await res.json()).code);
  const body = await res.json();
  return body.methodResponses as [string, any, string][];
}
```

---

## 3. Start-up sequence

```
1. GET  /jmap/session                 → accountId, username          (401 → handle as above)
2. JMAP Calendar/get                  → calendars + their state      (creates a default calendar on first use)
3. JMAP CalendarEvent/query + get     → events for the visible window; remember `state`
4. GET  /api/availability/shares      → who you can see (for the team panel)
5. open the SSE stream                → keeps 2–4 current from now on
```

`GET /jmap/session` → `primaryAccounts["urn:ietf:params:jmap:calendars"]` is your `accountId`, formatted `"<id>:<id>"` (for example `"7:7"`). Pass it in method arguments. The server derives the real account from your token, so a wrong value is ignored, not an escalation.

---

## 4. Calendars

```ts
interface JmapCalendar {
  id: string;
  name: string;
  description: string;
  color: string;            // hex, e.g. "#0078D4"
  sortOrder: number;
  isDefault: boolean;
  isVisible: boolean;
  isReadOnly: boolean;      // subscription (external ICS) calendars are read-only
  timezone: string;         // IANA name
  supportedComponents: string[];
  myRights: { mayReadItems: boolean; mayWriteAll: boolean; mayDelete: boolean; mayRename: boolean };
}
```

```jsonc
// list
["Calendar/get", { "accountId": "7:7" }, "c1"]

// create / update / delete
["Calendar/set", { "accountId": "7:7",
  "create":  { "tmp1": { "name": "Work", "color": "#16A34A", "description": "", "timezone": "Asia/Kolkata" } },
  "update":  { "<calendarId>": { "name": "Work (EU)", "color": "#2563EB", "isVisible": true, "sortOrder": 2 } },
  "destroy": [ "<calendarId>" ]
}, "c2"]
```

Result: `created: { tmp1: { id } }`, `updated: { id: null }`, `destroyed: [id]`, and `notCreated`/`notUpdated` with reasons. Deleting a calendar deletes its events.

**Read-only calendars** (`isReadOnly: true`): disable create, edit, drag and delete in the UI. The server enforces this too (`forbidden`), and a subscription calendar is re-imported from its source on every sync, so local edits would vanish anyway.

**Where do new events go?** If you omit `calendarId`, the server uses the default calendar, or any writable one. Never a read-only one.

---

## 5. Events

### Shape

```ts
interface JmapEvent {
  id: string;                       // stable; the series id for recurring events
  uid: string;                      // iCalendar UID (same across the organizer's and invitees' copies)
  calendarId: string;
  title: string;
  description: string;
  location: string;
  start: string;                    // RFC 3339, UTC offset as stored
  end?: string;
  duration?: string;                // ISO 8601, only if the source used DURATION
  timeZone?: string;                // IANA zone the wall-clock times belong to; "Etc/UTC" = a fixed UTC time; absent for all-day
  isAllDay: boolean;
  status: "CONFIRMED" | "TENTATIVE" | "CANCELLED";
  sequence: number;
  priority: number;
  classification: "PUBLIC" | "PRIVATE" | "CONFIDENTIAL";
  organizer?: string;               // email
  participants?: Record<string, Participant>;
  recurrenceRules?: { rule: string }[];                 // RRULE text without the "RRULE:" prefix
  recurrenceOverrides?: Record<string, OccurrencePatch>;
  recurrenceId?: string;            // only for an override whose series is not in this account
  created: string;
  updated: string;
}

interface Participant {
  name: string; email: string; kind: "individual";
  roles: { attendee: true };
  participationStatus: "NEEDS-ACTION" | "ACCEPTED" | "DECLINED" | "TENTATIVE";
  expectReply: boolean;
}
```

### Query a window, then fetch

```jsonc
["CalendarEvent/query", { "accountId": "7:7",
   "filter": { "after": "2026-11-01T00:00:00Z", "before": "2026-12-01T00:00:00Z", "calendarId": "<optional>" },
   "limit": 200, "position": 0 }, "q"],
["CalendarEvent/get", { "accountId": "7:7", "#ids": { "resultOf": "q", "name": "CalendarEvent/query", "path": "/ids" } }, "g"]
```

The server resolves `#` back-references (RFC 8620 §3.7) such as `/ids`, `/list/*/id` and `/created/<id>/id`; a bad reference returns `invalidResultReference`.

- `query` returns ids of events that have **any occurrence** in the window (recurring series included). It returns **series**, not occurrences — the UI expands them (below).
- `total` is the overall number of matching series. Page with `position`/`limit` (sorted by start, then id); a negative `limit` or an unparseable `after`/`before` returns `invalidArguments`.
- `CalendarEvent/get` with no `ids` (or `ids: null`) returns everything for the account, up to 500 events (`requestTooLarge` beyond); `ids: []` returns nothing. Ids that don't exist or aren't UUIDs come back in `notFound`.
- At most 500 ids per `/get` and 500 create+update+destroy per `/set` (`requestTooLarge`).
- `/set` calls for one account run one at a time across all server instances (a Redis lock), so `ifInState` is reliable. If the account stays busy for 5 s the method returns `serverUnavailable` — retry after a short backoff.
- Each response carries `state`. Store the latest one.

### Create

```jsonc
["CalendarEvent/set", { "accountId": "7:7", "create": { "k1": {
  "title": "Design review",
  "start": "2026-11-20T10:00:00Z", "end": "2026-11-20T11:00:00Z",
  "timeZone": "America/New_York",            // always send it — see "Time zone" below
                                             // start/end: RFC 3339, or LocalDateTime ("2026-11-20T10:00:00", read in timeZone).
                                             // A missing or unparseable start → invalidProperties ["start"]
  "location": "Room 4", "description": "…",
  "calendarId": "<optional>",
  "participants": {
    "1": { "email": "bob@example.com", "name": "Bob" },
    "2": { "email": "carol@example.com", "name": "Carol" }
  },
  "recurrenceRules": [{ "rule": "FREQ=WEEKLY;BYDAY=TH;COUNT=8" }]
}}}, "c"]
```

Result: `created.k1 = { id, uid, etag }`.

- **Do not send `organizer`.** The server sets you as the organizer whenever there are participants, and ignores any value you send. This is deliberate (it stops people sending invitations in someone else's name).
- Adding participants sends invitations automatically — see [§7](#7-invitations-and-rsvp).

#### Time zone

`start` and `end` are always instants. `timeZone` says which zone those instants are *wall-clock times in*, and that is what makes "every Thursday 10:00 New York" stay at 10:00 across daylight-saving changes. **Send `timeZone` on every timed event**, using the user's zone: `Intl.DateTimeFormat().resolvedOptions().timeZone`.

- It must be an IANA name (`America/New_York`, `Asia/Kolkata`; common Windows names such as `Eastern Standard Time` are accepted). An unknown name fails that object with `invalidProperties` and `properties: ["timeZone"]`.
- `"Etc/UTC"`, `"UTC"`, `""` and `null` all mean a fixed UTC time.
- If you leave it out on a **recurring** event, the server uses the zone of the event's calendar (`timezone` on the calendar, `UTC` by default). Do not rely on this: a calendar's zone is rarely the zone of the person creating the event.
- All-day events have no zone; `timeZone` is not sent for them and is ignored if you send it.
- Every `CalendarEvent/get` returns the stored `timeZone`, so edit forms can show it.

### Update

```jsonc
["CalendarEvent/set", { "accountId": "7:7", "update": { "<id>": {
  "title": "Design review (moved)", "start": "…", "end": "…", "location": "…",
  "timeZone": "Europe/London",    // re-labels the series' zone; the instants in start/end do not change
  "participants": { … },          // replaces the guest list; removed guests get a cancellation
  "recurrenceRules": [{ "rule": "…" }],   // [] removes recurrence
  "status": "CANCELLED",
  "calendarId": "<another of your writable calendars>"
}}}, "u"]
```

Send only what changed. Updating an event **never** loses its exceptions, reminders or excluded dates. Each update bumps `sequence`.

A `timeZone` you send always applies, and the zone of every exception follows the series. Without one, an update leaves the zone alone — except when it *changes* the recurrence rule of an event that has no zone, which then takes the calendar's zone. Re-sending the same rule changes nothing. To change the zone and keep the same **wall-clock** time (09:00 New York → 09:00 London), send the new `timeZone` together with the new `start`/`end` instants.

### Delete

`"destroy": ["<id>"]` removes the whole event (every occurrence) and sends cancellations to guests. To remove **one occurrence**, use an override ([§6](#6-recurring-events-and-edit-this-occurrence)); destroying an occurrence's row id is refused.

### Per-object error types

`notFound`, `forbidden` (not yours / read-only calendar), `invalidProperties` (message says what), `serverFail`.

---

## 6. Recurring events and "edit this occurrence"

A series comes back as **one** event. Exceptions are in `recurrenceOverrides`, keyed by the **original** start of the occurrence:

```jsonc
{
  "id": "…", "title": "Standup", "start": "2026-11-02T09:00:00Z", "end": "2026-11-02T10:00:00Z",
  "timeZone": "Europe/London",
  "recurrenceRules": [{ "rule": "FREQ=WEEKLY;BYDAY=MO" }],
  "recurrenceOverrides": {
    "2026-11-16T09:00:00Z": { "start": "2026-11-16T14:00:00Z", "title": "Standup (afternoon)" },   // moved + retitled
    "2026-11-23T09:00:00Z": { "excluded": true }                                                  // cancelled
  }
}
```

An `OccurrencePatch` lists **only what differs** from the series: `start`, `end`, `title`, `description`, `location`, `status`, or `excluded: true`. For all-day series the key is `YYYY-MM-DD`.

### Expanding in the UI

Server-side expansion is used for free/busy and shared calendars, but the UI expands its own series. Use an RFC 5545 library (here `rrule` and `luxon`). **Expand in the event's `timeZone`, not in UTC**: a UTC-only expansion shows "every Monday 09:00 New York" an hour late after the daylight-saving change.

```ts
import { RRule } from "rrule";
import { DateTime } from "luxon";

// rrule works on "floating" times: a Date whose UTC fields hold the wall-clock fields.
const toFloating = (d: Date, zone: string) => {
  const l = DateTime.fromJSDate(d, { zone });
  return new Date(Date.UTC(l.year, l.month - 1, l.day, l.hour, l.minute, l.second));
};
const fromFloating = (f: Date, zone: string) =>
  DateTime.fromObject({ year: f.getUTCFullYear(), month: f.getUTCMonth() + 1, day: f.getUTCDate(),
    hour: f.getUTCHours(), minute: f.getUTCMinutes(), second: f.getUTCSeconds() }, { zone }).toJSDate();

// The real start instants of a series inside [from, to].
function seriesStarts(ev: JmapEvent, from: Date, to: Date): Date[] {
  const rule = ev.recurrenceRules?.[0]?.rule;
  if (!rule) return [new Date(ev.start)];
  const zone = ev.timeZone ?? "Etc/UTC";
  const opts = RRule.parseString(rule);
  if (opts.until) opts.until = toFloating(opts.until, zone);       // UNTIL is a UTC instant
  const rr = new RRule({ ...opts, dtstart: toFloating(new Date(ev.start), zone) });
  const DAY = 864e5;                                                // window edges are converted approximately: widen
  return rr.between(new Date(+toFloating(from, zone) - DAY), new Date(+toFloating(to, zone) + DAY), true)
           .map(f => fromFloating(f, zone));
}

function occurrences(ev: JmapEvent, from: Date, to: Date) {
  const dur = ev.end ? +new Date(ev.end) - +new Date(ev.start) : 0;

  const out = [];
  for (const s of seriesStarts(ev, from, to)) {
    const key = s.toISOString().replace(".000Z", "Z");          // "2026-11-16T09:00:00Z"
    const o = ev.recurrenceOverrides?.[key];
    if (o?.excluded) continue;                                  // cancelled
    out.push({
      eventId: ev.id, originalStart: key,
      start: new Date(o?.start ?? s), end: new Date(o?.end ?? (o?.start ? +new Date(o.start) + dur : +s + dur)),
      title: o?.title ?? ev.title, isException: !!o,
    });
  }
  // an occurrence moved INTO the window from outside is not produced by the rule: add those too
  for (const [key, o] of Object.entries(ev.recurrenceOverrides ?? {})) {
    if (o.excluded || !o.start || out.some(x => x.originalStart === key)) continue;
    const s = new Date(o.start);
    out.push({ eventId: ev.id, originalStart: key, start: s, end: new Date(o.end ?? +s + dur), title: o.title ?? ev.title, isException: true });
  }
  // keep only what is actually visible: a moved occurrence may now start outside the window
  return out.filter(o => o.end > from && o.start < to);
}
```

The override keys are always UTC instants, so `s.toISOString()` of a real start instant matches them even when the series is in another zone. Two cases are easy to miss: an occurrence whose *original* time is outside the window but that was moved **into** it (the second loop), and one whose original time is inside the window but that was moved **out** of it (the final filter).

### Editing

**This occurrence only** — update the series id with a patch keyed by the occurrence's original start:

```jsonc
["CalendarEvent/set", { "accountId": "7:7", "update": { "<seriesId>": { "recurrenceOverrides": {
  "2026-11-16T09:00:00Z": { "start": "2026-11-16T14:00:00Z", "title": "Standup (afternoon)" }
}}}}, "u"]
```

**Cancel this occurrence only:** `{ "excluded": true }`.

**Undo an exception** (occurrence returns to the series): set the key to `null`.

**All occurrences:** update the series fields normally. Existing exceptions are kept.

**This and following:** not a single call — set `COUNT`/`UNTIL` on the old series and create a new series from that date.

**Delete the whole series:** `destroy` the series id.

---

## 7. Invitations and RSVP

### Sending

Create or update an event with `participants`. For each guest:

- **Guests who have an account here** get the event **directly in their own calendar** immediately (their default writable calendar), and also a normal invitation email.
- **Everyone else** gets the invitation email only.

Edits to your event reach local guests' calendars; guests you remove lose the event and get a cancellation; deleting the event cancels it for everyone.

### Receiving

An invited user sees the event in their calendar with `organizer` set to someone else and `participants[].participationStatus: "NEEDS-ACTION"` for themselves. Show Accept / Maybe / Decline for events where `organizer !== me` and your own `participationStatus` is `NEEDS-ACTION`.

```
POST /api/scheduling/rsvp
{ "event_uid": "<uid>", "response": "accept" | "decline" | "maybe" }      // or ACCEPTED / DECLINED / TENTATIVE
→ { "success": true, "result": { "event_uid": "…", "attendee": "me@…", "response": "ACCEPTED" } }
```

- Use `uid` (not `id`): your copy and the organizer's copy have different ids but the same `uid`.
- The answer is applied to **every occurrence** of a series, on your copy, the organizer's copy and other local guests' copies. Per-occurrence replies are not supported yet.
- Errors: `404 event not found or you are not an attendee`.
- The organizer's UI receives a `state` event and refetches; their guest list shows the new status.

---

## 8. Team availability: free / busy

### Who can I see?

A teammate is visible when **they shared their availability with you** — or, if the deployment enables `AVAILABILITY_ORG_VISIBLE`, when you both belong to the same organisation (`mail_accounts.tenant_name`).

```
GET /api/availability/shares
→ result: {
    shared_by_me:   [{ user_id, email, name, created_at }],     // people who can see my free/busy
    shared_with_me: [{ user_id, email, name, created_at }],     // people whose free/busy I can see
    org_visible:    boolean                                     // true → my whole organisation is visible too
  }
```

Build the team panel from `shared_with_me`. When `org_visible` is true, colleagues in your organisation are visible too without a share, but this endpoint does not list them: take them from your own directory (the server answers `allowed: true` for those emails), or from the SSE stream, whose initial snapshot contains one `availability` event for every visible person (up to 100), org members included.

### Share / stop sharing

```
POST   /api/availability/shares   { "email": "bob@example.com" }    → 201 { user_id, email }
DELETE /api/availability/shares/:user_id                            → 204
```

`404 user not found` covers unknown, suspended and non-existent accounts alike. `400 cannot share with yourself`.

### Look up busy/free

```
GET /api/availability?emails=alice@x.com,bob@x.com&start=2026-11-14T00:00:00Z&end=2026-11-21T00:00:00Z
```

```json
{ "success": true, "result": {
  "start": "…", "end": "…",
  "results": [
    { "email": "alice@x.com", "allowed": true, "user_id": 1, "name": "Alice",
      "current": { "busy": true, "busyStatus": "confirmed", "until": "2026-11-14T19:36:40Z" },
      "busy": [ { "start": "…", "end": "…", "status": "confirmed" } ] },
    { "email": "bob@x.com", "allowed": false, "busy": [] }
  ] } }
```

- `busy[]` are **merged** blocks (overlaps and back-to-back meetings are joined). `status` is `confirmed` or `tentative`. Overlapping confirmed + tentative shows as confirmed.
- `current` is "right now": `busy`, `busyStatus`, `until` (end of the current block), and `nextBusyAt` when free.
- `allowed: false` means *not visible to you* — never *offline* or *doesn't exist*. Render it as "No access".
- Defaults: start = now, end = start + 7 days. Window ≤ **62 days**, ≤ **50 emails** per call.
- What counts as busy: events that are not `TRANSPARENT` ("free") and not `CANCELLED`; recurring events are expanded; a **declined** invitation is free; an unanswered or `TENTATIVE` one is `tentative`. Zero-length events do not occupy time.

### Scheduling assistant ("find a time")

1. `GET /api/availability?emails=<all invitees>&start=…&end=…`
2. Union every `allowed` person's `busy[]`; subtract from working hours; offer the gaps.
3. Treat `allowed: false` people as unknown and say so — don't offer a "free for everyone" answer that ignores them.

Standard-protocol equivalents exist for third-party clients (JMAP `Principal/getAvailability`, CalDAV `free-busy-query`, RFC 6638 outbox), but the REST call is simplest for the web UI.

---

## 9. Sharing calendars

Read-only sharing of a whole calendar (events visible with details; private events masked).

```
GET    /api/calendars/:id/shares                    → who this calendar is shared with
POST   /api/calendars/:id/shares  { "email": "…" }  → 201; read-only
DELETE /api/calendars/:id/shares/:user_id           → 204

GET    /api/shared/calendars                        → calendars others shared with me
GET    /api/shared/calendars/:id/events?start=…&end=…   → expanded occurrences (window ≤ 366 days, ≤ 2000 events)
```

`result.calendars[]` from `/api/shared/calendars` has the usual calendar fields (snake_case: `id`, `name`, `color`, `timezone`, …) plus `owner_email` and `privilege: "read"`.

`/api/shared/calendars/:id/events` returns **already expanded** occurrences, with exceptions applied:

```ts
{ id: string; uid: string; title: string; location?: string; start: string; end: string;
  is_all_day: boolean; status: string; recurring: boolean; private?: true }
```

When `private` is true the title is `"Busy"` and there is no location. Show these as read-only; every write to someone else's calendar is refused. Calendars you don't have access to return `404 calendar not found` (same as non-existent).

Show shared calendars in a separate "Shared with me" group with the owner's email; they do **not** appear in `Calendar/get`.

---

## 10. Live updates (SSE)

One stream carries everything: your own changes made elsewhere (another tab, a phone, a CalDAV client), teammates' busy/free, and share changes.

```
GET /jmap/eventsource?access_token=<jwt>
```

### Frames

| `event:` | Sent when | `data` |
|---|---|---|
| `state` | once on connect, then whenever **your own** events or calendars change | `{"@type":"StateChange","changed":{"7:7":{"CalendarEvent":"42","Calendar":"c0ffee…"}}}` — only the types that changed |
| `availability` | once per visible teammate on connect; then whenever their schedule changes, **and** when a meeting starts or ends (checked every 30 s) | see below |
| `shares` | a share involving you was added or removed | `{"@type":"SharesChanged"}` → refetch `/api/availability/shares` (and `/api/shared/calendars`) |
| `resync` | you may have missed events (slow connection, server reconnected to its database) | `{"@type":"Resync"}` → refetch everything |

`availability` data:

```json
{ "@type": "AvailabilityChange", "accountId": "1:1", "userId": 1, "email": "alice@x.com", "name": "Alice",
  "busy": true, "busyStatus": "confirmed", "until": "2026-11-14T19:36:40Z" }
```

Free looks like `{ …, "busy": false, "nextBusyAt": "…" }` (`nextBusyAt` only if something is coming within 7 days). Use it to colour a presence dot: **busy** (red), **busy tentative** (amber), **free** (green), and show "free until / busy until" from `until`/`nextBusyAt`. Teammates not shared with you never appear.

> A teammate being *free* means "nothing on their calendar right now", not "online". CalDev has no presence/last-seen. Don't label the dot "online".

### Reacting to `state`

The `state` payload only tells you *that* something changed. Fetch the delta:

```ts
async function onState(changed: { CalendarEvent?: string; Calendar?: string }) {
  if (changed.Calendar && changed.Calendar !== calState) { calState = changed.Calendar; await reloadCalendars(); }
  if (changed.CalendarEvent && changed.CalendarEvent !== eventState) {
    const [[, res]] = await jmap([["CalendarEvent/changes", { accountId, sinceState: eventState }, "c"]]);
    if (res.type === "cannotCalculateChanges") return reloadVisibleWindow();
    await refetch([...res.created, ...res.updated]);   // CalendarEvent/get by ids
    remove(res.destroyed);
    eventState = res.newState;
  }
}
```

- `CalendarEvent` state is a number-like string that only increases; `Calendar` state is an opaque string — compare for equality only.
- `changes` reports a series id as **updated** when any of its exceptions changed, and as **destroyed** under the series' real id.
- There is no `Calendar/changes`: on a `Calendar` change, call `Calendar/get` again.
- The initial `state` frame on every (re)connect already contains the current state: compare it with what you hold and fetch the delta. You need no separate "catch-up" request.

### A connection that survives real life

```ts
class LiveStream {
  private es?: EventSource; private backoff = 1000; private closed = false;
  constructor(private getToken: () => Promise<string>, private on: Handlers) {}

  async open() {
    if (this.closed) return;
    const token = await this.getToken();                         // a FRESH token every time
    const es = new EventSource(`${BASE}/jmap/eventsource?access_token=${encodeURIComponent(token)}`);
    this.es = es;
    es.addEventListener("state",        e => this.on.state(JSON.parse((e as MessageEvent).data).changed));
    es.addEventListener("availability", e => this.on.availability(JSON.parse((e as MessageEvent).data)));
    es.addEventListener("shares",       () => this.on.shares());
    es.addEventListener("resync",       () => this.on.resync());
    es.onopen  = () => { this.backoff = 1000; };
    es.onerror = () => {                                          // server closes every 30 min on purpose
      es.close();
      setTimeout(() => this.open(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, 30_000);
    };
  }
  close() { this.closed = true; this.es?.close(); }
}
```

Important details:

- **Reconnect with a fresh token.** The server ends each stream after **30 minutes** so a revoked or expired token cannot keep one alive. `EventSource` cannot change its URL, so close and recreate it with a new token, as above.
- **One stream per browser, not per tab.** The server allows **10 open streams per account**; the 11th gets `429`. Share one connection across tabs (a `SharedWorker`, or `BroadcastChannel` plus a `navigator.locks` leader election) and fan events out.
- **Use HTTP/2** in front of the app. Over HTTP/1.1 browsers allow only 6 connections per host, and a long-lived stream uses one.
- The stream sends `: ping` comments every 20 s to keep proxies open. Comments are invisible to `EventSource`; don't build a watchdog on them. If you need to detect a silently dead connection, call a cheap endpoint periodically and reconnect on failure.
- On every (re)connect you get a fresh snapshot (`state` + one `availability` per visible teammate), so reconnecting is always safe and idempotent.
- A token in the URL can end up in proxy logs and browser history. Prefer short-lived tokens for the stream; never log the URL.
- Ignore frames you do not know. New `event:` types may be added.

### Updating the UI

| Frame | UI effect |
|---|---|
| `state` (events) | refresh the visible calendar, animate changed items |
| `state` (calendars) | refresh the sidebar list and colours |
| `availability` | update the teammate's dot and tooltip; re-run the scheduling assistant if open |
| `shares` | refresh the team panel and "Shared with me" |
| `resync` | refetch calendars, visible events, shares |

---

## 11. Time and all-day rules

- Every instant is RFC 3339. Display in the user's own time zone; store and send UTC (`…Z`).
- **All-day events:** send `isAllDay: true` with `start` at **00:00:00Z of the first day** and `end` at 00:00:00Z of the day **after the last day** (end is exclusive, as in iCalendar). A single all-day on 20 Nov is `start 2026-11-20T00:00:00Z`, `end 2026-11-21T00:00:00Z`. Render by date, not by local midnight, so users west of UTC do not see it a day early.
- **Time zones of recurring events:** a recurring event stays at the same wall-clock time across daylight-saving changes **if it has a `timeZone`**. Events created by calendar apps over CalDAV always have one, and events created through JMAP have one when you send `timeZone` ([§5](#time-zone)). The zone survives everything the server does to the event afterwards: edits from any client, invitations and RSVPs, and the copy invitees receive.
- **Series without a zone** (`timeZone: "Etc/UTC"`) repeat at a fixed UTC time, so in a zone with daylight saving their local time shifts by an hour twice a year. That is right for "every 24 h" and wrong for "every Monday at 09:00". **Recurring events created through JMAP before `timeZone` existed are in this state.** To fix one, send it a `timeZone` (`update: { "<id>": { "timeZone": "America/New_York" } }`): the first occurrence stays at the same instant and later ones follow the local clock. Check that the instant is the right local time first — a series created in summer and labelled with a winter zone's offset in mind will show an hour off.
- Display is the UI's job: convert instants to the viewer's zone, and when the event's `timeZone` differs from the viewer's, show that too ("10:00 New York" for a viewer in Delhi) so people are not surprised when the local time moves after a daylight-saving change.
- `RRULE` text uses iCalendar syntax: `FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20270101T000000Z`. Rules with `FREQ=SECONDLY|MINUTELY` are not expanded.

---

## 12. Limits and known gaps

| Topic | Behaviour |
|---|---|
| Attendees per event | ≤ 3,000 attendees / recipients per event; rejected with `400 invalidProperties` if exceeded |
| Streams | 10 per account; closed after 30 min; one `retry:` hint of 5 s |
| Availability | ≤ 62 days per query, ≤ 50 emails; recurring series expanded up to 1000 occurrences per event |
| Shared calendar events | ≤ 366 days, ≤ 2000 events per call |
| Sharing | read-only only; no write/delegate sharing; no groups |
| Time zone definitions | the server writes `TZID=<IANA name>` to calendar apps without an embedded `VTIMEZONE` definition; a client that does not know IANA names may treat the time as UTC |
| Invitations to external addresses (email) | the emailed invitation describes the event's first occurrence only (no recurrence rule yet), in the event's `timeZone`; invitees with an account here get the whole series |
| RSVP | applies to the whole series; no per-occurrence reply |
| Edit "this and following" | not a single operation (split the series) |
| Reminders (alarms) | preserved and synced to CalDAV clients, but not exposed in the JMAP event shape; the server sends no push for them |
| Presence | none — only calendar busy/free |
| Invitations to people without an account | email only; the server does not track their reply unless it arrives at `/api/scheduling/incoming` |
| `CalendarEvent/get` without ids | returns everything (no paging) |
| Onboarding | `POST /api/calendar/onboard` with the user's JWT (cookie or Bearer) onboards **that** user only; a different `email` in the body gets `403`. Suspended accounts get `403` |

---

## 13. Endpoint cheat-sheet

```
Session & data (JMAP)
  GET   /jmap/session
  POST  /jmap/api        Calendar/get · Calendar/set
                         CalendarEvent/get · /set · /query · /changes
                         Principal/getAvailability   (id = email | "7:7" | 7)
  GET   /jmap/eventsource?access_token=…              (SSE: state · availability · shares · resync)

Availability
  GET     /api/availability?emails=&start=&end=
  GET     /api/availability/shares
  POST    /api/availability/shares            { email }
  DELETE  /api/availability/shares/:user_id

Calendar sharing
  GET     /api/calendars/:id/shares
  POST    /api/calendars/:id/shares           { email }
  DELETE  /api/calendars/:id/shares/:user_id
  GET     /api/shared/calendars
  GET     /api/shared/calendars/:id/events?start=&end=

Invitations
  POST    /api/scheduling/rsvp                { event_uid, response }

Subscriptions (import external ICS)           /api/subscriptions/…      (see README)
CalDAV for native apps                        /dav/{tenant}/{user}/…    (see README)
```

### Minimal state model for the UI

```ts
type Store = {
  calendars: JmapCalendar[];                    // Calendar/get
  events: Map<string, JmapEvent>;               // by id; one entry per series
  eventState: string; calendarState: string;    // for CalendarEvent/changes
  team: Map<number, { email: string; name: string; busy: boolean; busyStatus?: string; until?: string; nextBusyAt?: string }>;
  sharedWithMe: ShareInfo[]; sharedCalendars: SharedCalendar[];
};
```

Everything on screen derives from this store; the SSE handlers (`state`, `availability`, `shares`, `resync`) are the only things that need to mutate it from outside user actions.
