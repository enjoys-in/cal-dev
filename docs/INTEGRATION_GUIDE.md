# CalDev Integration Guide

This guide explains how external services, web applications, background workers, and email systems integrate with CalDev.

---

## 1. Authentication & Security

CalDev supports two primary authentication schemes:

### 1.1 JWT (Recommended for Web Clients & Internal Services)
Send the shared system JWT in the HTTP Authorization header:
```http
Authorization: Bearer <jwt_token>
```
- **JWT Payload Requirements**: Must include `email` (looked up in `mail_accounts`; an unknown email is refused). `sub`/`user_id` is used only for tokens without an email; `mid` is not used.
- **Shared Secret & Issuer**: Configured via `AUTH_JWT_SECRET` and `AUTH_JWT_ISSUER`.
- **Tenant Context**: Always the authenticated account's `mail_accounts.id` (`tenant_id == user_id`); headers can't change it. Users with the same `tenant_name` (their email domain, set at onboarding) form an organisation.

### 1.2 Basic Auth with App Passwords (for CalDAV Clients)
Native CalDAV clients (Apple Calendar, Thunderbird, Android) authenticate using HTTP Basic Auth:
```http
Authorization: Basic <base64(email:app_password)>
```
Users generate application-specific passwords via `POST /api/app-passwords/`.

### 1.3 Optional Application Signature Headers
When `APP_SIGNATURE` or `APP_API_KEY` are configured in `.env`, requests may include:
```http
X-App-Name: AirSend
X-App-Version: 1.0.0
X-App-Signature: <configured_signature>
```

---

## 2. Web Client Integration (JMAP API)

Web clients communicate with CalDev primarily through **JMAP (RFC 8620)**.

### 2.1 Discovering Capabilities & Session
Call `GET /jmap/session` with the user's Bearer token:
```bash
curl -X GET https://caldev.example.com:8443/jmap/session \
  -H "Authorization: Bearer <token>"
```
Returns:
- `apiUrl`: `/jmap/api`
- `eventSourceUrl`: `/jmap/eventsource`
- `accounts`: List of active accounts the user can access.

### 2.2 Onboarding & "Start Using Calendar" (1 Calendar Per Email)
CalDev enforces **one primary calendar per email address**.
- If a user has no calendar (or is encountering a setup state), prompt with **"Start Using Calendar"**.
- Trigger onboarding via `POST /api/calendar/onboard` (or `/api/users/onboard`) or JMAP `Calendar/set`.
  - AirSend apps call it with the user's JWT; it onboards only that user.
  - External services that can't hold the JWT secret get an onboarding API key (`X-API-Key`), which onboards users of the key's email domain only. Keys come from `POST /api/admin/api-keys` (admin) or `POST /api/api-keys` (a signed-in user, for their own domain). See [API Reference §9](API_REFERENCE.md).
- Once created, feature settings (name, description, color, timezone) can be customized anytime.

### 2.3 Handling 401 (View-Only Mode)
When CalDev returns `401 Unauthorized`:
- Web applications should place the calendar in a **code-level view-only state**.
- Do not disable the entire DOM — allow users to scroll and inspect existing cached events while suppressing destructive write, edit, and delete requests.

### 2.4 Creating and Updating Events
Send a standard JMAP invocation to `POST /jmap/api`:
```json
{
  "using": [
    "urn:ietf:params:jmap:core",
    "urn:ietf:params:jmap:calendars"
  ],
  "methodCalls": [
    [
      "CalendarEvent/set",
      {
        "accountId": "1",
        "create": {
          "evt-1": {
            "calendarId": "default-cal-id",
            "title": "Team Sprint Review",
            "start": "2026-11-16T10:00:00Z",
            "duration": "PT1H",
            "locations": {
              "loc-1": { "name": "Conference Room A / Google Meet" }
            },
            "participants": {
              "attendee@example.com": {
                "name": "Alex Smith",
                "email": "attendee@example.com",
                "kind": "individual",
                "roles": { "attendee": true }
              }
            }
          }
        }
      },
      "call-1"
    ]
  ]
}
```

---

## 3. Reminders and Email

### 3.1 Reminders: SSE event `reminder`
When a meeting milestone is due (15m, 10m, 5m, or 0m), CalDev sends a `reminder` event on the user's open `/jmap/eventsource` stream (the same stream that carries `state`, `availability` and `shares`). Internally the worker sends it through a Postgres `NOTIFY` on `caldev_changes`, so it reaches the user's stream on whichever server instance holds it. There is no Redis Pub/Sub channel and no separate push daemon.

A reminder is shown only to users with a stream open at that moment (the web client should show it as a notification). It is not stored or replayed, and there is no Web Push to a closed browser.

**Event data**:
```json
{
  "event_uid": "7c3a41b2-1094-4b52-b883-82ffab181041",
  "recurrence_id": "20261116T100000Z",
  "title": "Meeting Reminder: Team Sprint Review",
  "location": "Conference Room A",
  "start_utc": "2026-11-16T10:00:00Z",
  "recipient_email": "user@example.com",
  "recipient_user_id": 42,
  "milestone_minutes": 15,
  "body": "Starts in 15 minutes • Conference Room A",
  "tag": "cal-7c3a41b2-1094-4b52-b883-82ffab181041-20261116T100000Z-15m",
  "url": "/calendar",
  "timestamp": "2026-11-16T09:45:00Z"
}
```

**Client side**:
```js
const es = new EventSource("/jmap/eventsource", { withCredentials: true });
es.addEventListener("reminder", (e) => {
  const r = JSON.parse(e.data);
  new Notification(r.title, { body: r.body, tag: r.tag }); // `tag` de-duplicates across tabs
});
```

---

### 3.2 Outbound Email Invitations

CalDev does not use Redis for email. For every guest it creates one message (`scheduling_messages`) and sends it, in this order:

1. **The calendar's own SMTP** (BYO), when configured and active.
2. **airsend-api** for internal Airsend accounts: a signed `POST` to `AIRSEND_INTERNAL_URL` (`https://api.airsend.in/api/v1/internal/calendar/mail`), which DKIM-signs, sends and stores the email in the sender's mailbox. The request carries a `Bearer` token (`AIRSEND_INTERNAL_TOKEN`), an HMAC signature (`AIRSEND_INTERNAL_SECRET`) and the message id as idempotency key. A failure is retried after 5m, 10m, 15m, 25m and 1h, then the message is marked `failed`.
3. Neither available: the message is marked `unconfigured_smtp`.

Each email carries the `.ics` (`METHOD:REQUEST`, `CANCEL` or `REPLY`) and, for invitations, per-guest RSVP links on `CALDEV_PUBLIC_URL`.

---

## 4. Connecting Native CalDAV Clients

### Apple Calendar (macOS / iOS)
- **Account Type**: CalDAV
- **Account Mode**: Manual
- **User Name**: Full email address (e.g., `user@domain.com`)
- **Password**: App Password generated from web settings
- **Server Address**: `caldev.yourdomain.com:8443`
- **Server Path**: `/.well-known/caldav` (public; redirects to `/dav/`, then sign in) or `/dav/1/42/`

### Thunderbird Lightning
- **Location**: `https://caldev.yourdomain.com:8443/dav/{tenant_id}/{user_id}/`
- **Username**: Full email address
- **Password**: App Password

---

## 5. Deployment with `build.sh`

Deployments are automated through [build.sh](file:///f:/private/ENJOYS/airsend/cal-dev/build.sh):

```bash
# Execute deployment
./build.sh
```

**Deployment Lifecycle**:
1. Builds the fresh image (`caldev-server`) via `docker build -t caldev-server .`.
2. Validates that the build succeeded before altering any running service.
3. Stops and removes the old container `caldev-server`.
4. Starts the new container with:
   - `--env-file .env`
   - `--restart unless-stopped`
   - `--add-host=host.docker.internal:host-gateway`
   - `-p 127.0.0.1:$PORT:$PORT` (binds strictly to localhost to prevent direct public exposure; fronted by reverse proxy or internal network)
