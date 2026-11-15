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
- **JWT Payload Requirements**: Must include user identity (`email`, `sub`, or `mid`).
- **Shared Secret & Issuer**: Configured via `AUTH_JWT_SECRET` and `AUTH_JWT_ISSUER`.
- **Tenant Context**: Automatically resolved from the user's domain or `mail_accounts.id`.

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
  - External services that can't hold the JWT secret get an onboarding API key (`X-API-Key`), which onboards users of the key's email domain only. Keys come from `POST /api/admin/api-keys` (admin) or `POST /api/api-keys` (a signed-in user, for their own domain). See [API Reference §8b](API_REFERENCE.md).
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

## 3. Redis Pub/Sub Channels

CalDev uses Redis (or Valkey) Pub/Sub to communicate asynchronously with notification daemons and external email relays.

### 3.1 Push Notifications: `calendar:push`
Published when a meeting milestone is due (15m, 10m, 5m, or 0m) or when a calendar event changes.

**Channel**: `calendar:push`

**Payload Schema**:
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

**How Notification Daemons / Web Push Should Listen**:
```python
# Example: Python Redis Subscriber for Web Push
import redis, json

r = redis.Redis.from_url("redis://localhost:6379/0")
pubsub = r.pubsub()
pubsub.subscribe("calendar:push")

for msg in pubsub.listen():
    if msg["type"] == "message":
        data = json.loads(msg["data"])
        send_web_push(
            user_id=data["recipient_user_id"],
            title=data["title"],
            body=data["body"],
            tag=data["tag"],
            url=data["url"]
        )
```

---

### 3.2 Outbound Email Invitations: `calendar:mail`
CalDev does not maintain direct outbound SMTP connections. Instead, it generates RFC 5545 `.ics` MIME scheduling envelopes and publishes them to `calendar:mail`.

**Channel**: `calendar:mail`

**Payload Schema**:
```json
{
  "message_id": "msg-8812c",
  "tenant_id": 1,
  "sender_id": 42,
  "organizer_email": "organizer@example.com",
  "recipient_email": "external-client@clientdomain.com",
  "event_uid": "7c3a41b2-1094-4b52-b883-82ffab181041",
  "method": "REQUEST",
  "subject": "Invitation: Team Sprint Review @ Mon Nov 16 10:00 UTC",
  "raw_ics": "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Enjoys//CalDev//EN\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:7c3a41b2-1094-4b52-b883-82ffab181041\r\n...",
  "created_at": "2026-11-15T15:00:00Z"
}
```

**How External SMTP Services Consume This**:
1. Listen on `calendar:mail`.
2. Wrap `raw_ics` into a multipart MIME email with `Content-Type: text/calendar; method=REQUEST; charset=UTF-8`.
3. Dispatch via postfix, Haraka, or cloud SMTP relay.

---

## 4. Connecting Native CalDAV Clients

### Apple Calendar (macOS / iOS)
- **Account Type**: CalDAV
- **Account Mode**: Manual
- **User Name**: Full email address (e.g., `user@domain.com`)
- **Password**: App Password generated from web settings
- **Server Address**: `caldev.yourdomain.com:8443`
- **Server Path**: `/.well-known/caldav` (or `/dav/1/42/`)

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
