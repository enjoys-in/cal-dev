-- 009: drop the caldev_ prefix from table names — CalDev has its own schema now
-- (caldev.events, caldev.calendars, ...). Runs before 010, which uses the new names.
--
-- Works on every starting point, idempotently:
--   * tables still named caldev_<name> in CalDev's schema (older install, or 001..008 just ran
--     on a new database) are renamed; indexes, constraints, triggers and data stay attached;
--   * tables already renamed by hand are left alone;
--   * both names present = stop, a person has to keep one.
-- Then every trigger function is recreated with the new table names (their bodies name tables)
-- and every trigger is recreated, so a hand-made copy without triggers also ends up complete.
-- Function names and the 'caldev_changes' NOTIFY channel keep their names.

DO $caldev$
DECLARE
    s text := current_schema();
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'api_keys',
        'app_passwords',
        'availability_shares',
        'calendar_shares',
        'calendar_smtp',
        'calendar_subscriptions',
        'calendars',
        'events',
        'jmap_states',
        'job_history',
        'locks',
        'reminder_deliveries',
        'scheduling_messages',
        'sync_changes'
    ]
    LOOP
        IF to_regclass(format('%I.%I', s, 'caldev_' || t)) IS NOT NULL THEN
            IF to_regclass(format('%I.%I', s, t)) IS NOT NULL THEN
                RAISE EXCEPTION 'both %.caldev_% and %.% exist; keep one by hand', s, t, s, t;
            END IF;
            EXECUTE format('ALTER TABLE %I.%I RENAME TO %I', s, 'caldev_' || t, t);
        END IF;
    END LOOP;
END
$caldev$;

CREATE OR REPLACE FUNCTION caldev_update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Housekeeping, called by background jobs
CREATE OR REPLACE FUNCTION caldev_cleanup_job_history()
RETURNS void AS $$
BEGIN
    DELETE FROM job_history WHERE created_at < NOW() - INTERVAL '7 days';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION caldev_cleanup_expired_locks()
RETURNS void AS $$
BEGIN
    DELETE FROM locks WHERE expires_at < NOW();
END;
$$ LANGUAGE plpgsql;

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
$$ LANGUAGE plpgsql;

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
$$ LANGUAGE plpgsql;

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
$$ LANGUAGE plpgsql;

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
$$ LANGUAGE plpgsql;

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
$$ LANGUAGE plpgsql;

-- ============================================================
-- Triggers
-- ============================================================

-- updated_at
DROP TRIGGER IF EXISTS trg_caldev_calendars_updated_at ON calendars;
CREATE TRIGGER trg_caldev_calendars_updated_at BEFORE UPDATE ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();

DROP TRIGGER IF EXISTS trg_caldev_events_updated_at ON events;
CREATE TRIGGER trg_caldev_events_updated_at BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();

DROP TRIGGER IF EXISTS trg_caldev_subscriptions_updated_at ON calendar_subscriptions;
CREATE TRIGGER trg_caldev_subscriptions_updated_at BEFORE UPDATE ON calendar_subscriptions
    FOR EACH ROW EXECUTE FUNCTION caldev_update_updated_at_column();

-- ctag / sync change log on every event write
DROP TRIGGER IF EXISTS trg_caldev_events_ctag_insert ON events;
CREATE TRIGGER trg_caldev_events_ctag_insert AFTER INSERT ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();

DROP TRIGGER IF EXISTS trg_caldev_events_ctag_update ON events;
CREATE TRIGGER trg_caldev_events_ctag_update AFTER UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();

DROP TRIGGER IF EXISTS trg_caldev_events_ctag_delete ON events;
CREATE TRIGGER trg_caldev_events_ctag_delete AFTER DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_ctag();

-- JMAP "Calendar" state
DROP TRIGGER IF EXISTS trg_caldev_calendars_state_ins ON calendars;
CREATE TRIGGER trg_caldev_calendars_state_ins AFTER INSERT ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

DROP TRIGGER IF EXISTS trg_caldev_calendars_state_upd ON calendars;
CREATE TRIGGER trg_caldev_calendars_state_upd
    AFTER UPDATE OF name, description, color, timezone, sort_order, is_default, is_visible, is_readonly
    ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

DROP TRIGGER IF EXISTS trg_caldev_calendars_state_del ON calendars;
CREATE TRIGGER trg_caldev_calendars_state_del AFTER DELETE ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

-- realtime NOTIFY
DROP TRIGGER IF EXISTS trg_caldev_events_notify ON events;
CREATE TRIGGER trg_caldev_events_notify AFTER INSERT OR UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

DROP TRIGGER IF EXISTS trg_caldev_calendars_notify_ins ON calendars;
CREATE TRIGGER trg_caldev_calendars_notify_ins AFTER INSERT OR DELETE ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

DROP TRIGGER IF EXISTS trg_caldev_calendars_notify_upd ON calendars;
CREATE TRIGGER trg_caldev_calendars_notify_upd
    AFTER UPDATE OF name, description, color, timezone, sort_order, is_default, is_visible, is_readonly
    ON calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

DROP TRIGGER IF EXISTS trg_caldev_avail_shares_notify ON availability_shares;
CREATE TRIGGER trg_caldev_avail_shares_notify AFTER INSERT OR DELETE ON availability_shares
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_share_change();

DROP TRIGGER IF EXISTS trg_caldev_cal_shares_notify ON calendar_shares;
CREATE TRIGGER trg_caldev_cal_shares_notify AFTER INSERT OR DELETE ON calendar_shares
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_share_change();

-- Function bodies name tables without a schema: pin search_path so triggers fired from any
-- session still find CalDev's tables.
DO $caldev$
DECLARE r record;
BEGIN
    FOR r IN
        SELECT p.oid::regprocedure AS sig
        FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = current_schema() AND p.proname LIKE 'caldev\_%'
    LOOP
        EXECUTE format('ALTER FUNCTION %s SET search_path = %I, public', r.sig, current_schema());
    END LOOP;
END
$caldev$;
