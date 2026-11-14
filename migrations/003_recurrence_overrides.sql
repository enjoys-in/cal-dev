-- ============================================================
-- 003: recurrence overrides as separate rows
--
-- A CalDAV resource (one .ics) may hold a master VEVENT plus one VEVENT per modified or cancelled
-- occurrence (same UID, distinct RECURRENCE-ID). Each component now has its own row, so the unique
-- key is (calendar_id, uid, recurrence_id) instead of (calendar_id, uid). The master has
-- recurrence_id = '' (so it sorts first); overrides carry a normalised key: a UTC instant such as
-- 20261116T033000Z, or a plain date (20261116) for all-day series.
--
-- Idempotent: safe to run on databases that already have 001/002 applied.
-- ============================================================

-- 1. Replace UNIQUE(calendar_id, uid) whatever it happens to be named.
DO $$
DECLARE
    c TEXT;
BEGIN
    FOR c IN
        SELECT con.conname
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        WHERE rel.relname = 'caldev_events'
          AND con.contype = 'u'
          AND (
              SELECT array_agg(att.attname::text ORDER BY att.attname::text)
              FROM unnest(con.conkey) AS k(attnum)
              JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = k.attnum
          ) = ARRAY['calendar_id', 'uid']
    LOOP
        EXECUTE format('ALTER TABLE caldev_events DROP CONSTRAINT %I', c);
    END LOOP;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'caldev_events_component_key'
    ) THEN
        ALTER TABLE caldev_events
            ADD CONSTRAINT caldev_events_component_key UNIQUE (calendar_id, uid, recurrence_id);
    END IF;
END $$;

-- Resource lookups ("all rows of this UID, master first") use this index.
CREATE INDEX IF NOT EXISTS idx_caldev_events_resource ON caldev_events (tenant_id, calendar_id, uid, recurrence_id);

-- 2. Change tracking must say WHICH row changed.
--    event_id lets JMAP report destroyed events by their real id (it used to fall back to the UID,
--    which no client knows), recurrence_id lets a changed override be reported as an update of its master.
ALTER TABLE caldev_sync_changes ADD COLUMN IF NOT EXISTS recurrence_id VARCHAR(256) NOT NULL DEFAULT '';
ALTER TABLE caldev_sync_changes ADD COLUMN IF NOT EXISTS event_id UUID;

CREATE OR REPLACE FUNCTION caldev_update_calendar_ctag()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE caldev_calendars
    SET ctag = encode(digest(uuid_generate_v4()::text, 'sha256'), 'hex'),
        sync_token = 'http://cal.example.com/sync/' || encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex')
    WHERE id = COALESCE(NEW.calendar_id, OLD.calendar_id);

    -- Record sync change
    INSERT INTO caldev_sync_changes (tenant_id, calendar_id, event_uid, recurrence_id, event_id, change_type, sync_token)
    SELECT
        COALESCE(NEW.tenant_id, OLD.tenant_id),
        COALESCE(NEW.calendar_id, OLD.calendar_id),
        COALESCE(NEW.uid, OLD.uid),
        COALESCE(NEW.recurrence_id, OLD.recurrence_id, ''),
        COALESCE(NEW.id, OLD.id),
        CASE
            WHEN TG_OP = 'INSERT' THEN 'created'
            WHEN TG_OP = 'UPDATE' THEN 'modified'
            WHEN TG_OP = 'DELETE' THEN 'deleted'
        END,
        (SELECT sync_token FROM caldev_calendars WHERE id = COALESCE(NEW.calendar_id, OLD.calendar_id));

    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;
