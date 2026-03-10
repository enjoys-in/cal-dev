-- Migration 006: Restore UUID defaults and triggers after CASCADE drop of uuid-ossp
-- The DROP EXTENSION uuid-ossp CASCADE removed DEFAULT uuid_generate_v4() from all id columns.
-- This migration restores them and recreates trigger functions with explicit id values.

-- Self-healing: allow re-run
DELETE FROM caldev_migrations WHERE filename = 'migrations/006_restore_uuid_defaults.sql';

-- Ensure extensions exist in public schema
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Restore DEFAULT uuid_generate_v4() on all UUID id columns
ALTER TABLE caldev_app_passwords       ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_calendars            ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_events               ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_scheduling_messages  ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_locks                ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_jmap_states          ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_job_history          ALTER COLUMN id SET DEFAULT uuid_generate_v4();
ALTER TABLE caldev_calendar_subscriptions ALTER COLUMN id SET DEFAULT uuid_generate_v4();

-- Recreate trigger: update calendar ctag when events change
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

-- Recreate trigger: update JMAP state on event changes (explicit id)
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
