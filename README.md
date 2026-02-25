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
│  public.users (shared)   │      Scheduling / Cleanup         │
└──────────────────────────┴───────────────────────────────────┘
```

## Key Features

- **CalDAV** — Full RFC 4791/4918 (PROPFIND, PROPPATCH, REPORT, MKCALENDAR, PUT, DELETE, MOVE, COPY, LOCK/UNLOCK)
- **JMAP** — RFC 8620 session + API (CalendarEvent/get, set, query, changes; Calendar/get, set)
- **ICS** — RFC 5545 iCalendar parser & generator
- **Scheduling** — RFC 6638 invitations/replies/cancellations via SMTP (background jobs)
- **Multi-tenant** — Full tenant isolation via `tenant_id`
- **Shared Users** — Reads from `public.users` (same table as your mail/auth system)
- **App Passwords** — CalDAV clients use `email + app-password` for Basic Auth
- **JWT** — Shared secret & issuer with your other services
- **Background Jobs** — Valkey-backed queue with retry, dead-letter, delayed scheduling
- **Sync Tokens** — Efficient delta sync for CalDAV clients

## Project Structure

```
cal-dev/
├── cmd/server/main.go          # Entry point, routing, graceful shutdown
├── internal/
│   ├── auth/auth.go            # JWT (shared issuer) + app password auth
│   ├── caldav/handler.go       # All WebDAV/CalDAV method handlers
│   ├── config/config.go        # Environment-based configuration
│   ├── ics/ics.go              # RFC 5545 ICS parser & generator
│   ├── jmap/handler.go         # JMAP session + API handlers
│   ├── jobs/
│   │   ├── queue.go            # Valkey-backed job queue (LPUSH/BRPOP)
│   │   └── worker.go           # Worker pool with retry & dead-letter
│   ├── middleware/middleware.go # Security, rate limiting, CORS, auth
│   ├── models/models.go        # Domain models
│   ├── scheduling/scheduling.go# RFC 6638 scheduling via job queue
│   ├── store/store.go          # PostgreSQL repository layer
│   └── xmlutil/xmlutil.go      # WebDAV XML parsing & response generation
├── migrations/001_init.sql     # PostgreSQL schema (caldev + public.users refs)
├── docker-compose.yml          # PostgreSQL + Valkey + CalDev
├── Dockerfile                  # Multi-stage build
└── .env.example                # All environment variables
```

## Prerequisites

- Go 1.22+
- PostgreSQL 16+
- Valkey 8+ (or Redis 7+ — wire-compatible)
- Your existing `public.users` table with columns: `id (UUID)`, `email`, `tenant_id`, `display_name`

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

## Authentication

### JWT (Bearer)
Tokens issued by your existing auth system work automatically — CalDev validates with the same `AUTH_JWT_SECRET` and `AUTH_JWT_ISSUER`.

```
Authorization: Bearer <jwt-token>
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

## Background Jobs (Valkey)

CPU-intensive tasks run in background workers:

| Job Type | Trigger | Description |
|---|---|---|
| `send_scheduling_email` | Event with attendees created/updated | Sends RFC 6638 iMIP invitations via SMTP |
| `cleanup_locks` | Every 5 minutes | Removes expired WebDAV locks |
| `cleanup_job_history` | Every 5 minutes | Prunes job history older than 7 days |

Jobs support automatic retry (configurable max + exponential backoff) and dead-letter queuing.

## Database Schema

CalDev uses the `caldev` schema and references `public.users`:

```
public.users (YOUR TABLE — not managed by CalDev)
  ├── id (UUID PK)
  ├── email
  ├── tenant_id
  └── display_name

caldev.tenants
caldev.app_passwords     → FK public.users(id)
caldev.calendars         → FK public.users(id), caldev.tenants(id)
caldev.events            → FK public.users(id), caldev.calendars(id)
caldev.sync_changes      → FK caldev.calendars(id)
caldev.scheduling_messages → FK public.users(id)
caldev.locks
caldev.jmap_states       → FK public.users(id)
caldev.job_history
```

## License

MIT
