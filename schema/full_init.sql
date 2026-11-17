-- ============================================================
-- CalDev: the complete database schema, final state, in one file
--
-- Creates every table with its final columns, types, defaults, indexes and constraints, plus the
-- functions and triggers, in the `caldev` schema. Nothing here is a change step: no ALTER, no
-- data fix. Use it to set up a brand-new database in one go:
--
--     psql -U postgres -d airsend_mail -v ON_ERROR_STOP=1 -f schema/full_init.sql
--
-- It is the raw schema only: no migration history table, no list of applied migrations. When the
-- server starts on a database built from this file it recognises that and skips the original
-- migrations by itself (see Store.ApplyMigrations). Safe to run again.
--
-- A different schema name: replace "caldev" in CREATE SCHEMA, in SET search_path, and in the
-- `SET search_path = caldev, public` of each function.
--
-- Needs PostgreSQL 14+ (CREATE OR REPLACE TRIGGER). Never touches the mail system's public tables.
--
-- Keep it current: when a migration changes the schema, change the table here to the new final
-- shape (do not append the change).
-- ============================================================

CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS "pgcrypto" WITH SCHEMA public;

CREATE SCHEMA IF NOT EXISTS caldev;
SET search_path = caldev, public;

-- ============================================================
-- CalDev's own accounts (caldev.mail_accounts — the mail system's public.mail_accounts is never
-- written). tenant_id == user_id == mail_accounts.id everywhere in CalDev.
--   internal: copy of an Airsend mailbox, same id and email (added on first use, calendar enabled)
--   external: created by the onboarding API, ids from 1,000,000,000 so they never meet Airsend ids
--   status:   CalDev's own ACTIVE | INACTIVE | SUSPENDED
-- ============================================================
CREATE SEQUENCE IF NOT EXISTS external_account_id_seq START 1000000000;
CREATE TABLE IF NOT EXISTS mail_accounts (
    id INTEGER PRIMARY KEY DEFAULT nextval('external_account_id_seq'),
    name VARCHAR(255) NOT NULL DEFAULT '',
    email VARCHAR(255) NOT NULL UNIQUE,
    tenant_name VARCHAR(255) NOT NULL DEFAULT '',
    status VARCHAR(50) NOT NULL DEFAULT 'ACTIVE'
        CONSTRAINT chk_caldev_mail_accounts_status CHECK (status IN ('ACTIVE', 'INACTIVE', 'SUSPENDED')),
    type VARCHAR(50) NOT NULL DEFAULT 'USER',
    account_origin VARCHAR(16) NOT NULL DEFAULT 'external'
        CONSTRAINT chk_caldev_mail_accounts_origin CHECK (account_origin IN ('internal', 'external')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_caldev_mail_accounts_email_lower ON mail_accounts (lower(email));

-- Case-insensitive account lookups (GetUserByEmail's lower(email) fallback)
CREATE INDEX IF NOT EXISTS idx_mail_accounts_email_lower ON mail_accounts (lower(email));

-- ============================================================
-- App passwords (CalDAV Basic Auth: email + app password)
-- ============================================================
CREATE TABLE IF NOT EXISTS app_passwords (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    label           VARCHAR(256) NOT NULL DEFAULT '',
    password_hash   VARCHAR(512) NOT NULL,
    scopes          TEXT[] NOT NULL DEFAULT ARRAY['caldav','jmap'],
    last_used_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_app_passwords_user ON app_passwords(user_id);

-- ============================================================
-- Onboarding API keys. Only the SHA-256 hash of a key is stored.
-- A key is linked to an owner email and may onboard only users of that email's domain.
-- ============================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    owner_email   VARCHAR(512) NOT NULL,
    domain        VARCHAR(255) NOT NULL,          -- lower-case domain the key may onboard (*@domain)
    label         VARCHAR(255) NOT NULL DEFAULT '',
    key_prefix    VARCHAR(16)  NOT NULL,          -- first characters, to recognise a key in lists
    key_hash      CHAR(64)     NOT NULL UNIQUE,   -- hex SHA-256 of the full key
    created_by    INTEGER,                        -- mail_accounts.id of the creator; NULL = admin
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_caldev_api_keys_owner ON api_keys(lower(owner_email));

-- ============================================================
-- Calendars
-- ============================================================
CREATE TABLE IF NOT EXISTS calendars (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    name            VARCHAR(512) NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    color           VARCHAR(32) NOT NULL DEFAULT '#0078D4',
    timezone        VARCHAR(128) NOT NULL DEFAULT 'UTC',
    sort_order      INT NOT NULL DEFAULT 0,
    ctag            VARCHAR(128) NOT NULL DEFAULT '',
    sync_token      VARCHAR(256) NOT NULL DEFAULT '',
    is_default      BOOLEAN NOT NULL DEFAULT FALSE,
    is_visible      BOOLEAN NOT NULL DEFAULT TRUE,
    is_readonly     BOOLEAN NOT NULL DEFAULT FALSE,
    supported_components TEXT[] NOT NULL DEFAULT ARRAY['VEVENT','VTODO','VJOURNAL'],
    max_resource_size   INT NOT NULL DEFAULT 10485760, -- 10MB
    properties      JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_calendars_tenant ON calendars(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_calendars_user ON calendars(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_calendars_tenant_user ON calendars(tenant_id, user_id);

-- ============================================================
-- Events (calendar objects). One row per VEVENT/VTODO component of a resource (.ics): the master
-- has recurrence_id = '' (so it sorts first); each RECURRENCE-ID override has its own row with a
-- normalised key (a UTC instant like 20261116T033000Z, or a plain date for all-day series).
-- ============================================================
CREATE TABLE IF NOT EXISTS events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    calendar_id     UUID NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    uid             VARCHAR(512) NOT NULL,
    summary         VARCHAR(1024) NOT NULL DEFAULT '',
    description     TEXT NOT NULL DEFAULT '',
    location        VARCHAR(1024) NOT NULL DEFAULT '',
    dtstart         TIMESTAMPTZ NOT NULL,
    dtend           TIMESTAMPTZ,
    duration        VARCHAR(64) NOT NULL DEFAULT '',
    all_day         BOOLEAN NOT NULL DEFAULT FALSE,
    recurrence_rule TEXT NOT NULL DEFAULT '',
    recurrence_id   VARCHAR(256) NOT NULL DEFAULT '',
    sequence        INT NOT NULL DEFAULT 0,
    status          VARCHAR(32) NOT NULL DEFAULT 'CONFIRMED',
    transparency    VARCHAR(32) NOT NULL DEFAULT 'OPAQUE',
    classification  VARCHAR(32) NOT NULL DEFAULT 'PUBLIC',
    organizer       VARCHAR(512) NOT NULL DEFAULT '',
    attendees       JSONB NOT NULL DEFAULT '[]',
    categories      TEXT[] NOT NULL DEFAULT '{}',
    priority        INT NOT NULL DEFAULT 0,
    url             VARCHAR(2048) NOT NULL DEFAULT '',
    geo             VARCHAR(128) NOT NULL DEFAULT '',
    dtstamp         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    etag            VARCHAR(128) NOT NULL,
    raw_ics         TEXT NOT NULL,
    size_bytes      INT NOT NULL DEFAULT 0,
    component_type  VARCHAR(32) NOT NULL DEFAULT 'VEVENT',
    properties      JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT caldev_events_component_key UNIQUE (calendar_id, uid, recurrence_id)
);

CREATE INDEX IF NOT EXISTS idx_caldev_events_tenant ON events(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_events_user ON events(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_events_calendar ON events(calendar_id);
CREATE INDEX IF NOT EXISTS idx_caldev_events_uid ON events(uid);
CREATE INDEX IF NOT EXISTS idx_caldev_events_dtstart ON events(dtstart);
CREATE INDEX IF NOT EXISTS idx_caldev_events_dtend ON events(dtend);
CREATE INDEX IF NOT EXISTS idx_caldev_events_etag ON events(etag);
CREATE INDEX IF NOT EXISTS idx_caldev_events_updated ON events(updated_at);
CREATE INDEX IF NOT EXISTS idx_caldev_events_time_range ON events(calendar_id, dtstart, dtend);
CREATE INDEX IF NOT EXISTS idx_caldev_events_component ON events(calendar_id, component_type);
CREATE INDEX IF NOT EXISTS idx_caldev_events_tenant_calendar ON events(tenant_id, calendar_id);
-- availability lookups (time-bounded scans per owner)
CREATE INDEX IF NOT EXISTS idx_caldev_events_owner_time ON events (tenant_id, user_id, dtstart);
-- "all rows of this UID, master first"
CREATE INDEX IF NOT EXISTS idx_caldev_events_resource ON events (tenant_id, calendar_id, uid, recurrence_id);

-- ============================================================
-- Change log behind CalDAV sync-collection and JMAP /changes (written by triggers below).
-- recurrence_id and event_id come last: they were added after the table was first created.
-- ============================================================
CREATE TABLE IF NOT EXISTS sync_changes (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    calendar_id     UUID NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    event_uid       VARCHAR(512) NOT NULL,
    change_type     VARCHAR(16) NOT NULL, -- 'created', 'modified', 'deleted'
    sync_token      VARCHAR(256) NOT NULL,
    changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    recurrence_id   VARCHAR(256) NOT NULL DEFAULT '',
    event_id        UUID
);

CREATE INDEX IF NOT EXISTS idx_caldev_sync_changes_calendar ON sync_changes(calendar_id);
CREATE INDEX IF NOT EXISTS idx_caldev_sync_changes_token ON sync_changes(calendar_id, sync_token);
CREATE INDEX IF NOT EXISTS idx_caldev_sync_changes_tenant ON sync_changes(tenant_id);

-- ============================================================
-- Scheduling (invitations / replies / cancellations outbox)
-- ============================================================
CREATE TABLE IF NOT EXISTS scheduling_messages (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    sender_id       INTEGER NOT NULL, -- references mail_accounts(id)
    recipient_email VARCHAR(512) NOT NULL,
    event_uid       VARCHAR(512) NOT NULL,
    method          VARCHAR(32) NOT NULL, -- REQUEST, REPLY, CANCEL
    status          VARCHAR(32) NOT NULL DEFAULT 'pending', -- pending, sent, failed
    raw_ics         TEXT NOT NULL,
    error_message   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_tenant ON scheduling_messages(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_sender ON scheduling_messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_status ON scheduling_messages(status);
CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_event ON scheduling_messages(event_uid);

-- ============================================================
-- Reminder deliveries: dedup log so a reminder milestone is delivered at most once
-- ============================================================
CREATE TABLE IF NOT EXISTS reminder_deliveries (
    id                BIGSERIAL PRIMARY KEY,
    tenant_id         INTEGER NOT NULL,
    event_uid         VARCHAR(512) NOT NULL,
    recurrence_id     VARCHAR(256) NOT NULL DEFAULT '',
    recipient_email   VARCHAR(255) NOT NULL,
    milestone_minutes INT NOT NULL, -- e.g. 15, 10, 5, 0
    channel           VARCHAR(32) NOT NULL DEFAULT 'push', -- 'push', 'mail'
    delivered_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_reminder_delivery UNIQUE (event_uid, recurrence_id, recipient_email, milestone_minutes, channel)
);

CREATE INDEX IF NOT EXISTS idx_reminder_deliveries_lookup
ON reminder_deliveries(event_uid, recurrence_id, recipient_email, milestone_minutes);

-- ============================================================
-- WebDAV locks (LOCK / UNLOCK)
-- ============================================================
CREATE TABLE IF NOT EXISTS locks (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    resource_path   VARCHAR(2048) NOT NULL,
    lock_token      VARCHAR(512) NOT NULL UNIQUE,
    owner           VARCHAR(512) NOT NULL DEFAULT '',
    lock_type       VARCHAR(32) NOT NULL DEFAULT 'write',
    lock_scope      VARCHAR(32) NOT NULL DEFAULT 'exclusive',
    depth           VARCHAR(16) NOT NULL DEFAULT '0',
    timeout_seconds INT NOT NULL DEFAULT 3600,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_caldev_locks_resource ON locks(resource_path);
CREATE INDEX IF NOT EXISTS idx_caldev_locks_token ON locks(lock_token);
CREATE INDEX IF NOT EXISTS idx_caldev_locks_expires ON locks(expires_at);
CREATE INDEX IF NOT EXISTS idx_caldev_locks_tenant ON locks(tenant_id);

-- ============================================================
-- JMAP state. Only 'Calendar' is stored here (kept by trigger); the 'CalendarEvent' state is
-- MAX(sync_changes.id) for the tenant.
-- ============================================================
CREATE TABLE IF NOT EXISTS jmap_states (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    entity_type     VARCHAR(64) NOT NULL, -- 'CalendarEvent', 'Calendar'
    state           VARCHAR(256) NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, user_id, entity_type)
);

CREATE INDEX IF NOT EXISTS idx_caldev_jmap_states_user ON jmap_states(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_jmap_states_tenant ON jmap_states(tenant_id);

-- ============================================================
-- Background jobs (audit trail; the actual queue is in Valkey)
-- ============================================================
CREATE TABLE IF NOT EXISTS job_history (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    job_type        VARCHAR(64) NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    status          VARCHAR(32) NOT NULL DEFAULT 'completed', -- completed, failed, dead
    error_message   TEXT NOT NULL DEFAULT '',
    duration_ms     INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_job_history_type ON job_history(job_type);
CREATE INDEX IF NOT EXISTS idx_caldev_job_history_status ON job_history(status);
CREATE INDEX IF NOT EXISTS idx_caldev_job_history_created ON job_history(created_at);

-- ============================================================
-- Calendar subscriptions (external ICS feeds)
-- ============================================================
CREATE TABLE IF NOT EXISTS calendar_subscriptions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    calendar_id     UUID NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,

    -- Source configuration
    source_type     VARCHAR(32) NOT NULL DEFAULT 'ics_url',   -- ics_url, google, outlook
    source_url      TEXT NOT NULL,                             -- ICS feed URL
    display_name    VARCHAR(512) NOT NULL DEFAULT '',          -- User-friendly name
    color           VARCHAR(32) NOT NULL DEFAULT '#6B7280',

    -- Authentication for protected feeds
    auth_type       VARCHAR(32) NOT NULL DEFAULT 'none',       -- none, basic, bearer, api_key
    auth_credentials TEXT NOT NULL DEFAULT '',                  -- encrypted credentials

    -- Sync state
    is_enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    sync_interval   INT NOT NULL DEFAULT 3600,                 -- seconds (default 1 hour)
    last_synced_at  TIMESTAMPTZ,
    last_etag       VARCHAR(256) NOT NULL DEFAULT '',
    last_error      TEXT NOT NULL DEFAULT '',
    error_count     INT NOT NULL DEFAULT 0,
    events_count    INT NOT NULL DEFAULT 0,

    -- Metadata
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_tenant ON calendar_subscriptions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_user ON calendar_subscriptions(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_calendar ON calendar_subscriptions(calendar_id);
CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_enabled ON calendar_subscriptions(is_enabled, last_synced_at);

-- ============================================================
-- Sharing: availability (free/busy only) and read-only calendar shares
-- ============================================================
CREATE TABLE IF NOT EXISTS availability_shares (
    owner_user_id   INTEGER NOT NULL,
    grantee_user_id INTEGER NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_user_id, grantee_user_id),
    CHECK (owner_user_id <> grantee_user_id)
);

CREATE INDEX IF NOT EXISTS idx_caldev_avail_shares_grantee ON availability_shares (grantee_user_id);

CREATE TABLE IF NOT EXISTS calendar_shares (
    calendar_id     UUID NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    owner_user_id   INTEGER NOT NULL,
    grantee_user_id INTEGER NOT NULL,
    privilege       VARCHAR(16) NOT NULL DEFAULT 'read' CHECK (privilege IN ('read')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (calendar_id, grantee_user_id),
    CHECK (owner_user_id <> grantee_user_id)
);

CREATE INDEX IF NOT EXISTS idx_caldev_calendar_shares_grantee ON calendar_shares (grantee_user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_calendar_shares_owner ON calendar_shares (owner_user_id);

-- ============================================================
-- Calendar BYO SMTP: outbound mail settings per calendar, password encrypted (AES-256-GCM)
-- ============================================================
CREATE TABLE IF NOT EXISTS calendar_smtp (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    calendar_id         UUID NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
    tenant_id           INTEGER NOT NULL,
    user_id             INTEGER NOT NULL,
    host                VARCHAR(255) NOT NULL,
    port                INTEGER NOT NULL DEFAULT 587,
    username            VARCHAR(255) NOT NULL,
    password_encrypted  TEXT NOT NULL,
    from_email          VARCHAR(255) NOT NULL,
    from_name           VARCHAR(255) NOT NULL DEFAULT '',
    reply_to            VARCHAR(255) NOT NULL DEFAULT '',
    encryption          VARCHAR(32) NOT NULL DEFAULT 'starttls', -- 'none', 'ssl', 'starttls'
    status              VARCHAR(32) NOT NULL DEFAULT 'active',   -- 'active', 'failing', 'disabled'
    last_tested_at      TIMESTAMPTZ,
    last_error          TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_calendar_smtp UNIQUE (calendar_id)
);

-- (redundant with uq_calendar_smtp's own index; kept so a fresh database matches a migrated one)
CREATE INDEX IF NOT EXISTS idx_calendar_smtp_calendar ON calendar_smtp(calendar_id);
CREATE INDEX IF NOT EXISTS idx_calendar_smtp_tenant ON calendar_smtp(tenant_id);
CREATE INDEX IF NOT EXISTS idx_calendar_smtp_user ON calendar_smtp(user_id);

-- ============================================================
-- Functions
-- ============================================================

-- Keep updated_at current
CREATE OR REPLACE FUNCTION caldev_update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

-- Housekeeping, called by background jobs
CREATE OR REPLACE FUNCTION caldev_cleanup_job_history()
RETURNS void AS $$
BEGIN
    DELETE FROM job_history WHERE created_at < NOW() - INTERVAL '7 days';
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

CREATE OR REPLACE FUNCTION caldev_cleanup_expired_locks()
RETURNS void AS $$
BEGIN
    DELETE FROM locks WHERE expires_at < NOW();
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

-- Event change log. Bumps the calendar's ctag/sync_token and records one change row. Does nothing
-- when the calendar itself is being deleted (a calendar delete cascades to its events).
CREATE OR REPLACE FUNCTION caldev_log_event_change(
    p_tenant INTEGER, p_calendar UUID, p_uid TEXT, p_recurrence_id TEXT, p_event_id UUID, p_change TEXT)
RETURNS void AS $$
DECLARE
    tok TEXT;
BEGIN
    UPDATE calendars
    SET ctag = encode(digest(uuid_generate_v4()::text, 'sha256'), 'hex'),
        sync_token = 'http://cal.example.com/sync/' || encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex')
    WHERE id = p_calendar
    RETURNING sync_token INTO tok;

    IF NOT FOUND THEN
        RETURN; -- the calendar itself is being deleted (cascade): there is nothing left to sync
    END IF;

    INSERT INTO sync_changes (tenant_id, calendar_id, event_uid, recurrence_id, event_id, change_type, sync_token)
    VALUES (p_tenant, p_calendar, p_uid, COALESCE(p_recurrence_id, ''), p_event_id, p_change, tok);
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

-- Trigger function for events. The per-tenant advisory lock is held until commit, so a
-- tenant's change rows get their ids in commit order (the JMAP state is MAX(id)). Moving an event
-- to another calendar logs 'deleted' for the old calendar and 'created' for the new one.
CREATE OR REPLACE FUNCTION caldev_update_calendar_ctag()
RETURNS TRIGGER AS $$
BEGIN
    -- Held until commit: a tenant's change rows get their ids in commit order.
    PERFORM pg_advisory_xact_lock(6001, COALESCE(NEW.tenant_id, OLD.tenant_id));

    IF TG_OP = 'INSERT' THEN
        PERFORM caldev_log_event_change(NEW.tenant_id, NEW.calendar_id, NEW.uid, NEW.recurrence_id, NEW.id, 'created');
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM caldev_log_event_change(OLD.tenant_id, OLD.calendar_id, OLD.uid, OLD.recurrence_id, OLD.id, 'deleted');
    ELSIF OLD.calendar_id IS DISTINCT FROM NEW.calendar_id THEN
        PERFORM caldev_log_event_change(OLD.tenant_id, OLD.calendar_id, OLD.uid, OLD.recurrence_id, OLD.id, 'deleted');
        PERFORM caldev_log_event_change(NEW.tenant_id, NEW.calendar_id, NEW.uid, NEW.recurrence_id, NEW.id, 'created');
    ELSE
        PERFORM caldev_log_event_change(NEW.tenant_id, NEW.calendar_id, NEW.uid, NEW.recurrence_id, NEW.id, 'modified');
    END IF;

    RETURN NULL; -- AFTER trigger: the return value is ignored
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

-- JMAP "Calendar" state: bumped only when calendar metadata changes (not when the ctag/sync_token
-- trigger touches the row after an event write).
CREATE OR REPLACE FUNCTION caldev_update_calendar_state()
RETURNS TRIGGER AS $$
DECLARE
    t INTEGER;
    u INTEGER;
BEGIN
    IF TG_OP = 'DELETE' THEN
        t := OLD.tenant_id; u := OLD.user_id;
    ELSE
        t := NEW.tenant_id; u := NEW.user_id;
    END IF;

    INSERT INTO jmap_states (id, tenant_id, user_id, entity_type, state, updated_at)
    VALUES (uuid_generate_v4(), t, u, 'Calendar',
            encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex'), NOW())
    ON CONFLICT (tenant_id, user_id, entity_type)
    DO UPDATE SET state = EXCLUDED.state, updated_at = NOW();

    RETURN NULL;
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

-- Realtime change feed: every write that matters to a live UI emits a NOTIFY on channel
-- 'caldev_changes'; the server LISTENs and fans out over SSE.
--   k = what changed ('event' | 'calendar' | 'avail_share' | 'cal_share')
--   u = owning user id (event/calendar) ; o/g = owner/grantee user ids (shares)
CREATE OR REPLACE FUNCTION caldev_notify_owner_change()
RETURNS TRIGGER AS $$
DECLARE
    kind TEXT;
    uid  INTEGER;
    tid  INTEGER;
BEGIN
    IF TG_TABLE_NAME = 'events' THEN kind := 'event'; ELSE kind := 'calendar'; END IF;
    IF TG_OP = 'DELETE' THEN
        uid := OLD.user_id; tid := OLD.tenant_id;
    ELSE
        uid := NEW.user_id; tid := NEW.tenant_id;
    END IF;
    PERFORM pg_notify('caldev_changes', json_build_object('k', kind, 'u', uid, 't', tid)::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

CREATE OR REPLACE FUNCTION caldev_notify_share_change()
RETURNS TRIGGER AS $$
DECLARE
    kind TEXT;
    oid  INTEGER;
    gid  INTEGER;
BEGIN
    IF TG_TABLE_NAME = 'availability_shares' THEN kind := 'avail_share'; ELSE kind := 'cal_share'; END IF;
    IF TG_OP = 'DELETE' THEN
        oid := OLD.owner_user_id; gid := OLD.grantee_user_id;
    ELSE
        oid := NEW.owner_user_id; gid := NEW.grantee_user_id;
    END IF;
    PERFORM pg_notify('caldev_changes', json_build_object('k', kind, 'o', oid, 'g', gid)::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql SET search_path = caldev, public;

-- ============================================================
-- Triggers
-- ============================================================

-- updated_at
CREATE OR REPLACE TRIGGER trg_caldev_calendars_updated_at BEFORE UPDATE ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();

CREATE OR REPLACE TRIGGER trg_caldev_events_updated_at BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();

CREATE OR REPLACE TRIGGER trg_caldev_subscriptions_updated_at BEFORE UPDATE ON calendar_subscriptions
    FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();

-- ctag / sync change log on every event write
CREATE OR REPLACE TRIGGER trg_caldev_events_ctag_insert AFTER INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();

CREATE OR REPLACE TRIGGER trg_caldev_events_ctag_update AFTER UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();

CREATE OR REPLACE TRIGGER trg_caldev_events_ctag_delete AFTER DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();

-- JMAP "Calendar" state
CREATE OR REPLACE TRIGGER trg_caldev_calendars_state_ins AFTER INSERT ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

CREATE OR REPLACE TRIGGER trg_caldev_calendars_state_upd
    AFTER UPDATE OF name, description, color, timezone, sort_order, is_default, is_visible, is_readonly
    ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

CREATE OR REPLACE TRIGGER trg_caldev_calendars_state_del AFTER DELETE ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

-- realtime NOTIFY
CREATE OR REPLACE TRIGGER trg_caldev_events_notify AFTER INSERT OR UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

CREATE OR REPLACE TRIGGER trg_caldev_calendars_notify_ins AFTER INSERT OR DELETE ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

CREATE OR REPLACE TRIGGER trg_caldev_calendars_notify_upd
    AFTER UPDATE OF name, description, color, timezone, sort_order, is_default, is_visible, is_readonly
    ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

CREATE OR REPLACE TRIGGER trg_caldev_avail_shares_notify AFTER INSERT OR DELETE ON availability_shares
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_share_change();

CREATE OR REPLACE TRIGGER trg_caldev_cal_shares_notify AFTER INSERT OR DELETE ON calendar_shares
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_share_change();
