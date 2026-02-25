-- External Calendar Subscriptions
-- Allows users to subscribe to external ICS feeds (Google Calendar, Outlook, etc.)
-- Events are periodically fetched and imported into a local calendar.

SET search_path TO caldev, public;

-- ============================================================
-- Calendar Subscriptions
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev.calendar_subscriptions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id       UUID NOT NULL REFERENCES caldev.tenants(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    calendar_id     UUID NOT NULL REFERENCES caldev.calendars(id) ON DELETE CASCADE,

    -- Source configuration
    source_type     VARCHAR(32) NOT NULL DEFAULT 'ics_url',   -- ics_url, google, outlook
    source_url      TEXT NOT NULL,                             -- ICS feed URL
    display_name    VARCHAR(512) NOT NULL DEFAULT '',          -- User-friendly name
    color           VARCHAR(32) NOT NULL DEFAULT '#6B7280',

    -- Authentication for protected feeds
    auth_type       VARCHAR(32) NOT NULL DEFAULT 'none',       -- none, basic, bearer, api_key
    auth_credentials TEXT NOT NULL DEFAULT '',                  -- encrypted credentials (basic: user:pass, bearer: token)

    -- Sync state
    is_enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    sync_interval   INT NOT NULL DEFAULT 3600,                 -- seconds (default 1 hour)
    last_synced_at  TIMESTAMPTZ,
    last_etag       VARCHAR(256) NOT NULL DEFAULT '',          -- ETag from last fetch
    last_error      TEXT NOT NULL DEFAULT '',
    error_count     INT NOT NULL DEFAULT 0,
    events_count    INT NOT NULL DEFAULT 0,

    -- Metadata
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_tenant ON caldev.calendar_subscriptions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_user ON caldev.calendar_subscriptions(user_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_calendar ON caldev.calendar_subscriptions(calendar_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_enabled ON caldev.calendar_subscriptions(is_enabled, last_synced_at);

-- Auto-update updated_at
CREATE TRIGGER trg_subscriptions_updated_at BEFORE UPDATE ON caldev.calendar_subscriptions
    FOR EACH ROW EXECUTE FUNCTION caldev.update_updated_at_column();
