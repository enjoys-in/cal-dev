-- CalDAV Server Database Schema
-- Schema: caldev
-- PostgreSQL
--
-- IMPORTANT: Users are sourced from public.users (shared with mail system).
-- public.users MUST exist with at least: id (UUID), email, tenant_id, display_name

-- Create schema
CREATE SCHEMA IF NOT EXISTS caldev;

SET search_path TO caldev, public;

-- Extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ============================================================
-- Tenants (mirrors/references your existing tenant structure)
-- ============================================================
CREATE TABLE caldev.tenants (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    slug        VARCHAR(128) NOT NULL UNIQUE,
    name        VARCHAR(256) NOT NULL,
    settings    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tenants_slug ON caldev.tenants(slug);

-- ============================================================
-- App Passwords (for CalDAV Basic Auth — like Nextcloud/Fastmail)
-- Users authenticate CalDAV clients with email + app-password.
-- ============================================================
CREATE TABLE caldev.app_passwords (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    label           VARCHAR(256) NOT NULL DEFAULT '',
    password_hash   VARCHAR(512) NOT NULL,
    scopes          TEXT[] NOT NULL DEFAULT ARRAY['caldav','jmap'],
    last_used_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_app_passwords_user ON caldev.app_passwords(user_id);

-- ============================================================
-- Calendars
-- ============================================================
CREATE TABLE caldev.calendars (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
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

CREATE INDEX idx_calendars_tenant ON caldev.calendars(tenant_id);
CREATE INDEX idx_calendars_user ON caldev.calendars(user_id);
CREATE INDEX idx_calendars_tenant_user ON caldev.calendars(tenant_id, user_id);

-- ============================================================
-- Events (Calendar Objects)
-- ============================================================
CREATE TABLE caldev.events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    calendar_id     UUID NOT NULL REFERENCES caldev.calendars(id) ON DELETE CASCADE,
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

CREATE INDEX idx_events_tenant ON caldev.events(tenant_id);
CREATE INDEX idx_events_user ON caldev.events(user_id);
CREATE INDEX idx_events_calendar ON caldev.events(calendar_id);
CREATE INDEX idx_events_uid ON caldev.events(uid);
CREATE INDEX idx_events_dtstart ON caldev.events(dtstart);
CREATE INDEX idx_events_dtend ON caldev.events(dtend);
CREATE INDEX idx_events_etag ON caldev.events(etag);
CREATE INDEX idx_events_updated ON caldev.events(updated_at);
CREATE INDEX idx_events_time_range ON caldev.events(calendar_id, dtstart, dtend);
CREATE INDEX idx_events_component ON caldev.events(calendar_id, component_type);
CREATE INDEX idx_events_tenant_calendar ON caldev.events(tenant_id, calendar_id);

-- ============================================================
-- Sync Tokens (Change tracking)
-- ============================================================
CREATE TABLE caldev.sync_changes (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
    calendar_id     UUID NOT NULL REFERENCES caldev.calendars(id) ON DELETE CASCADE,
    event_uid       VARCHAR(512) NOT NULL,
    change_type     VARCHAR(16) NOT NULL, -- 'created', 'modified', 'deleted'
    sync_token      VARCHAR(256) NOT NULL,
    changed_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_sync_changes_calendar ON caldev.sync_changes(calendar_id);
CREATE INDEX idx_sync_changes_token ON caldev.sync_changes(calendar_id, sync_token);
CREATE INDEX idx_sync_changes_tenant ON caldev.sync_changes(tenant_id);

-- ============================================================
-- Scheduling (Invitations / Responses)
-- ============================================================
CREATE TABLE caldev.scheduling_messages (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
    sender_id       UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    recipient_email VARCHAR(512) NOT NULL,
    event_uid       VARCHAR(512) NOT NULL,
    method          VARCHAR(32) NOT NULL, -- REQUEST, REPLY, CANCEL
    status          VARCHAR(32) NOT NULL DEFAULT 'pending', -- pending, sent, failed
    raw_ics         TEXT NOT NULL,
    error_message   TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ
);

CREATE INDEX idx_scheduling_tenant ON caldev.scheduling_messages(tenant_id);
CREATE INDEX idx_scheduling_sender ON caldev.scheduling_messages(sender_id);
CREATE INDEX idx_scheduling_status ON caldev.scheduling_messages(status);
CREATE INDEX idx_scheduling_event ON caldev.scheduling_messages(event_uid);

-- ============================================================
-- Locks (WebDAV LOCK/UNLOCK)
-- ============================================================
CREATE TABLE caldev.locks (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
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

CREATE INDEX idx_locks_resource ON caldev.locks(resource_path);
CREATE INDEX idx_locks_token ON caldev.locks(lock_token);
CREATE INDEX idx_locks_expires ON caldev.locks(expires_at);
CREATE INDEX idx_locks_tenant ON caldev.locks(tenant_id);

-- ============================================================
-- JMAP State tracking
-- ============================================================
CREATE TABLE caldev.jmap_states (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    entity_type     VARCHAR(64) NOT NULL, -- 'CalendarEvent', 'Calendar'
    state           VARCHAR(256) NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(tenant_id, user_id, entity_type)
);

CREATE INDEX idx_jmap_states_user ON caldev.jmap_states(user_id);
CREATE INDEX idx_jmap_states_tenant ON caldev.jmap_states(tenant_id);

-- ============================================================
-- Background Jobs (audit trail — actual queue is in Valkey)
-- ============================================================
CREATE TABLE caldev.job_history (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    job_type        VARCHAR(64) NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}',
    status          VARCHAR(32) NOT NULL DEFAULT 'completed', -- completed, failed, dead
    error_message   TEXT NOT NULL DEFAULT '',
    duration_ms     INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_job_history_type ON caldev.job_history(job_type);
CREATE INDEX idx_job_history_status ON caldev.job_history(status);
CREATE INDEX idx_job_history_created ON caldev.job_history(created_at);

-- Auto-cleanup old job history (keep 7 days)
CREATE OR REPLACE FUNCTION caldev.cleanup_job_history()
RETURNS void AS $$
BEGIN
    DELETE FROM caldev.job_history WHERE created_at < NOW() - INTERVAL '7 days';
END;
$$ LANGUAGE plpgsql;

-- ============================================================
-- Functions & Triggers
-- ============================================================

-- Auto-update updated_at timestamp
CREATE OR REPLACE FUNCTION caldev.update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_tenants_updated_at BEFORE UPDATE ON caldev.tenants
    FOR EACH ROW EXECUTE FUNCTION caldev.update_updated_at_column();

CREATE TRIGGER trg_calendars_updated_at BEFORE UPDATE ON caldev.calendars
    FOR EACH ROW EXECUTE FUNCTION caldev.update_updated_at_column();

CREATE TRIGGER trg_events_updated_at BEFORE UPDATE ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_updated_at_column();

-- Auto-update calendar ctag when events change
CREATE OR REPLACE FUNCTION caldev.update_calendar_ctag()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE caldev.calendars
    SET ctag = encode(digest(uuid_generate_v4()::text, 'sha256'), 'hex'),
        sync_token = 'http://cal.example.com/sync/' || encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex')
    WHERE id = COALESCE(NEW.calendar_id, OLD.calendar_id);

    -- Record sync change
    INSERT INTO caldev.sync_changes (tenant_id, calendar_id, event_uid, change_type, sync_token)
    SELECT
        COALESCE(NEW.tenant_id, OLD.tenant_id),
        COALESCE(NEW.calendar_id, OLD.calendar_id),
        COALESCE(NEW.uid, OLD.uid),
        CASE
            WHEN TG_OP = 'INSERT' THEN 'created'
            WHEN TG_OP = 'UPDATE' THEN 'modified'
            WHEN TG_OP = 'DELETE' THEN 'deleted'
        END,
        (SELECT sync_token FROM caldev.calendars WHERE id = COALESCE(NEW.calendar_id, OLD.calendar_id));

    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_events_ctag_insert AFTER INSERT ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_calendar_ctag();

CREATE TRIGGER trg_events_ctag_update AFTER UPDATE ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_calendar_ctag();

CREATE TRIGGER trg_events_ctag_delete AFTER DELETE ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_calendar_ctag();

-- Auto-update JMAP state on event changes
CREATE OR REPLACE FUNCTION caldev.update_jmap_state()
RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO caldev.jmap_states (tenant_id, user_id, entity_type, state, updated_at)
    VALUES (
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

CREATE TRIGGER trg_events_jmap_state_insert AFTER INSERT ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_jmap_state();

CREATE TRIGGER trg_events_jmap_state_update AFTER UPDATE ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_jmap_state();

CREATE TRIGGER trg_events_jmap_state_delete AFTER DELETE ON caldev.events
    FOR EACH ROW EXECUTE FUNCTION caldev.update_jmap_state();

-- Cleanup expired locks
CREATE OR REPLACE FUNCTION caldev.cleanup_expired_locks()
RETURNS void AS $$
BEGIN
    DELETE FROM caldev.locks WHERE expires_at < NOW();
END;
$$ LANGUAGE plpgsql;
