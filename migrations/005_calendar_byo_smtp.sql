-- ============================================================
-- Calendar BYO SMTP Configuration
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_calendar_smtp (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    calendar_id         UUID NOT NULL REFERENCES caldev_calendars(id) ON DELETE CASCADE,
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

CREATE INDEX IF NOT EXISTS idx_calendar_smtp_calendar ON caldev_calendar_smtp(calendar_id);
CREATE INDEX IF NOT EXISTS idx_calendar_smtp_tenant ON caldev_calendar_smtp(tenant_id);
CREATE INDEX IF NOT EXISTS idx_calendar_smtp_user ON caldev_calendar_smtp(user_id);
