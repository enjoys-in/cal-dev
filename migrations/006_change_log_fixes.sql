-- ============================================================
-- 006: event change-log trigger fixes
--
--  * Deleting a calendar cascades to its events; their AFTER DELETE trigger used to log a change
--    for the calendar being deleted (NULL sync_token / FK violation), so the delete failed.
--    Nothing is logged for a calendar that no longer exists.
--  * Moving an event to another calendar (UPDATE of calendar_id) used to bump and log only the new
--    calendar. The old calendar now gets a ctag bump and a 'deleted' change, the new one 'created'.
--  * Change rows of one tenant are inserted under a per-tenant transaction advisory lock, so their
--    BIGSERIAL ids commit in id order and MAX(id) (the JMAP state) never skips a later commit of a
--    lower id.
--  * The per-row 'CalendarEvent' upsert into caldev_jmap_states is dropped: that state is
--    MAX(caldev_sync_changes.id) and the row was never read.
--
-- Idempotent.
-- ============================================================

CREATE OR REPLACE FUNCTION caldev_log_event_change(
    p_tenant INTEGER, p_calendar UUID, p_uid TEXT, p_recurrence_id TEXT, p_event_id UUID, p_change TEXT)
RETURNS void AS $$
DECLARE
    tok TEXT;
BEGIN
    UPDATE caldev_calendars
    SET ctag = encode(digest(uuid_generate_v4()::text, 'sha256'), 'hex'),
        sync_token = 'http://cal.example.com/sync/' || encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex')
    WHERE id = p_calendar
    RETURNING sync_token INTO tok;

    IF NOT FOUND THEN
        RETURN; -- the calendar itself is being deleted (cascade): there is nothing left to sync
    END IF;

    INSERT INTO caldev_sync_changes (tenant_id, calendar_id, event_uid, recurrence_id, event_id, change_type, sync_token)
    VALUES (p_tenant, p_calendar, p_uid, COALESCE(p_recurrence_id, ''), p_event_id, p_change, tok);
END;
$$ LANGUAGE plpgsql;

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
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_caldev_events_jmap_state_insert ON caldev_events;
DROP TRIGGER IF EXISTS trg_caldev_events_jmap_state_update ON caldev_events;
DROP TRIGGER IF EXISTS trg_caldev_events_jmap_state_delete ON caldev_events;
