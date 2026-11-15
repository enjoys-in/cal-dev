-- ============================================================
-- Reminder Deliveries (Deduplication log for milestone reminders)
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_reminder_deliveries (
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
ON caldev_reminder_deliveries(event_uid, recurrence_id, recipient_email, milestone_minutes);
