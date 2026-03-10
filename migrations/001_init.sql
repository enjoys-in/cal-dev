-- ============================================================
-- CalDAV Server Database Schema (single consolidated migration)
-- All tables in public schema with caldev_ prefix.
-- Users are sourced from mail_accounts (shared with mail system).
-- mail_accounts MUST exist with at least: id (INTEGER PK, auto-increment), email, name
-- Since mail_accounts has no tenant_id, we use mail_accounts.id as tenant_id.
-- ============================================================

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ============================================================
-- App Passwords (for CalDAV Basic Auth — like Nextcloud/Fastmail)
-- Users authenticate CalDAV clients with email + app-password.
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_app_passwords (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    label           VARCHAR(256) NOT NULL DEFAULT '',
    password_hash   VARCHAR(512) NOT NULL,
    scopes          TEXT[] NOT NULL DEFAULT ARRAY['caldav','jmap'],
    last_used_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_app_passwords_user ON caldev_app_passwords(user_id);

-- ============================================================
-- Calendars
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_calendars (
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

CREATE INDEX IF NOT EXISTS idx_caldev_calendars_tenant ON caldev_calendars(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_calendars_user ON caldev_calendars(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_calendars_tenant_user ON caldev_calendars(tenant_id, user_id);

-- ============================================================
-- Events (Calendar Objects)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    calendar_id     UUID NOT NULL REFERENCES caldev_calendars(id) ON DELETE CASCADE,
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
    UNIQUE(calendar_id, uid)
);

CREATE INDEX IF NOT EXISTS idx_caldev_events_tenant ON caldev_events(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_events_user ON caldev_events(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_events_calendar ON caldev_events(calendar_id);
CREATE INDEX IF NOT EXISTS idx_caldev_events_uid ON caldev_events(uid);
CREATE INDEX IF NOT EXISTS idx_caldev_events_dtstart ON caldev_events(dtstart);
CREATE INDEX IF NOT EXISTS idx_caldev_events_dtend ON caldev_events(dtend);
CREATE INDEX IF NOT EXISTS idx_caldev_events_etag ON caldev_events(etag);
CREATE INDEX IF NOT EXISTS idx_caldev_events_updated ON caldev_events(updated_at);
CREATE INDEX IF NOT EXISTS idx_caldev_events_time_range ON caldev_events(calendar_id, dtstart, dtend);
CREATE INDEX IF NOT EXISTS idx_caldev_events_component ON caldev_events(calendar_id, component_type);
CREATE INDEX IF NOT EXISTS idx_caldev_events_tenant_calendar ON caldev_events(tenant_id, calendar_id);

-- ============================================================
-- Sync Tokens (Change tracking)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_sync_changes (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    calendar_id     UUID NOT NULL REFERENCES caldev_calendars(id) ON DELETE CASCADE,
    event_uid       VARCHAR(512) NOT NULL,
    change_type     VARCHAR(16) NOT NULL, -- 'created', 'modified', 'deleted'
    sync_token      VARCHAR(256) NOT NULL,
    changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_sync_changes_calendar ON caldev_sync_changes(calendar_id);
CREATE INDEX IF NOT EXISTS idx_caldev_sync_changes_token ON caldev_sync_changes(calendar_id, sync_token);
CREATE INDEX IF NOT EXISTS idx_caldev_sync_changes_tenant ON caldev_sync_changes(tenant_id);

-- ============================================================
-- Scheduling (Invitations / Responses)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_scheduling_messages (
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

CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_tenant ON caldev_scheduling_messages(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_sender ON caldev_scheduling_messages(sender_id);
CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_status ON caldev_scheduling_messages(status);
CREATE INDEX IF NOT EXISTS idx_caldev_scheduling_event ON caldev_scheduling_messages(event_uid);

-- ============================================================
-- Locks (WebDAV LOCK/UNLOCK)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_locks (
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

CREATE INDEX IF NOT EXISTS idx_caldev_locks_resource ON caldev_locks(resource_path);
CREATE INDEX IF NOT EXISTS idx_caldev_locks_token ON caldev_locks(lock_token);
CREATE INDEX IF NOT EXISTS idx_caldev_locks_expires ON caldev_locks(expires_at);
CREATE INDEX IF NOT EXISTS idx_caldev_locks_tenant ON caldev_locks(tenant_id);

-- ============================================================
-- JMAP State tracking
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_jmap_states (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    entity_type     VARCHAR(64) NOT NULL, -- 'CalendarEvent', 'Calendar'
    state           VARCHAR(256) NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, user_id, entity_type)
);

CREATE INDEX IF NOT EXISTS idx_caldev_jmap_states_user ON caldev_jmap_states(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_jmap_states_tenant ON caldev_jmap_states(tenant_id);

-- ============================================================
-- Background Jobs (audit trail — actual queue is in Valkey)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_job_history (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    job_type        VARCHAR(64) NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    status          VARCHAR(32) NOT NULL DEFAULT 'completed', -- completed, failed, dead
    error_message   TEXT NOT NULL DEFAULT '',
    duration_ms     INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_caldev_job_history_type ON caldev_job_history(job_type);
CREATE INDEX IF NOT EXISTS idx_caldev_job_history_status ON caldev_job_history(status);
CREATE INDEX IF NOT EXISTS idx_caldev_job_history_created ON caldev_job_history(created_at);

-- Auto-cleanup old job history (keep 7 days)
CREATE OR REPLACE FUNCTION caldev_cleanup_job_history()
RETURNS void AS $$
BEGIN
    DELETE FROM caldev_job_history WHERE created_at < NOW() - INTERVAL '7 days';
END;
$$ LANGUAGE plpgsql;

-- ============================================================
-- Calendar Subscriptions (external ICS feeds)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_calendar_subscriptions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       INTEGER NOT NULL, -- references mail_accounts(id)
    user_id         INTEGER NOT NULL, -- references mail_accounts(id)
    calendar_id     UUID NOT NULL REFERENCES caldev_calendars(id) ON DELETE CASCADE,

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

CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_tenant ON caldev_calendar_subscriptions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_user ON caldev_calendar_subscriptions(user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_calendar ON caldev_calendar_subscriptions(calendar_id);
CREATE INDEX IF NOT EXISTS idx_caldev_subscriptions_enabled ON caldev_calendar_subscriptions(is_enabled, last_synced_at);

-- ============================================================
-- Functions & Triggers
-- ============================================================

-- Auto-update updated_at timestamp
CREATE OR REPLACE FUNCTION caldev_update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_calendars_updated_at') THEN
        CREATE TRIGGER trg_caldev_calendars_updated_at BEFORE UPDATE ON caldev_calendars
            FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_updated_at') THEN
        CREATE TRIGGER trg_caldev_events_updated_at BEFORE UPDATE ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_subscriptions_updated_at') THEN
        CREATE TRIGGER trg_caldev_subscriptions_updated_at BEFORE UPDATE ON caldev_calendar_subscriptions
            FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();
    END IF;
END $$;

-- Auto-update calendar ctag when events change
CREATE OR REPLACE FUNCTION caldev_update_calendar_ctag()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE caldev_calendars
    SET ctag = encode(digest(uuid_generate_v4()::text, 'sha256'), 'hex'),
        sync_token = 'http://cal.example.com/sync/' || encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex')
    WHERE id = COALESCE(NEW.calendar_id, OLD.calendar_id);

    -- Record sync change
    INSERT INTO caldev_sync_changes (tenant_id, calendar_id, event_uid, change_type, sync_token)
    SELECT
        COALESCE(NEW.tenant_id, OLD.tenant_id),
        COALESCE(NEW.calendar_id, OLD.calendar_id),
        COALESCE(NEW.uid, OLD.uid),
        CASE
            WHEN TG_OP = 'INSERT' THEN 'created'
            WHEN TG_OP = 'UPDATE' THEN 'modified'
            WHEN TG_OP = 'DELETE' THEN 'deleted'
        END,
        (SELECT sync_token FROM caldev_calendars WHERE id = COALESCE(NEW.calendar_id, OLD.calendar_id));

    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_ctag_insert') THEN
        CREATE TRIGGER trg_caldev_events_ctag_insert AFTER INSERT ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_ctag_update') THEN
        CREATE TRIGGER trg_caldev_events_ctag_update AFTER UPDATE ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_ctag_delete') THEN
        CREATE TRIGGER trg_caldev_events_ctag_delete AFTER DELETE ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();
    END IF;
END $$;

-- Auto-update JMAP state on event changes
CREATE OR REPLACE FUNCTION caldev_update_jmap_state()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO caldev_jmap_states (id, tenant_id, user_id, entity_type, state, updated_at)
    VALUES (
        uuid_generate_v4(),
        COALESCE(NEW.tenant_id, OLD.tenant_id),
        COALESCE(NEW.user_id, OLD.user_id),
        'CalendarEvent',
        encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex'),
        NOW()
    )
    ON CONFLICT (tenant_id, user_id, entity_type)
    DO UPDATE SET
        state = EXCLUDED.state,
        updated_at = NOW();

    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_jmap_state_insert') THEN
        CREATE TRIGGER trg_caldev_events_jmap_state_insert AFTER INSERT ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_jmap_state();
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_jmap_state_update') THEN
        CREATE TRIGGER trg_caldev_events_jmap_state_update AFTER UPDATE ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_jmap_state();
    END IF;
END $$;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_caldev_events_jmap_state_delete') THEN
        CREATE TRIGGER trg_caldev_events_jmap_state_delete AFTER DELETE ON caldev_events
            FOR EACH ROW EXECUTE FUNCTION caldev_update_jmap_state();
    END IF;
END $$;

-- Cleanup expired locks
CREATE OR REPLACE FUNCTION caldev_cleanup_expired_locks()
RETURNS void AS $$
BEGIN
    DELETE FROM caldev_locks WHERE expires_at < NOW();
END;
$$ LANGUAGE plpgsql;
