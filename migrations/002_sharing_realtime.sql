-- ============================================================
-- 002: availability/calendar sharing, calendar state, realtime change feed
-- Idempotent: safe to run on databases that already have 001 applied.
-- ============================================================

-- ------------------------------------------------------------
-- Availability shares: owner lets grantee see their free/busy (never event details).
-- ------------------------------------------------------------
CREATE TABLE IF NOT EXISTS caldev_availability_shares (
    owner_user_id   INTEGER NOT NULL,
    grantee_user_id INTEGER NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_user_id, grantee_user_id),
    CHECK (owner_user_id <> grantee_user_id)
);

CREATE INDEX IF NOT EXISTS idx_caldev_avail_shares_grantee ON caldev_availability_shares (grantee_user_id);

-- ------------------------------------------------------------
-- Calendar shares: owner lets grantee read one calendar (read-only).
-- ------------------------------------------------------------
CREATE TABLE IF NOT EXISTS caldev_calendar_shares (
    calendar_id     UUID NOT NULL REFERENCES caldev_calendars(id) ON DELETE CASCADE,
    owner_user_id   INTEGER NOT NULL,
    grantee_user_id INTEGER NOT NULL,
    privilege       VARCHAR(16) NOT NULL DEFAULT 'read' CHECK (privilege IN ('read')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (calendar_id, grantee_user_id),
    CHECK (owner_user_id <> grantee_user_id)
);

CREATE INDEX IF NOT EXISTS idx_caldev_calendar_shares_grantee ON caldev_calendar_shares (grantee_user_id);
CREATE INDEX IF NOT EXISTS idx_caldev_calendar_shares_owner ON caldev_calendar_shares (owner_user_id);

-- ------------------------------------------------------------
-- JMAP "Calendar" state: bump only when calendar metadata changes
-- (not when the ctag/sync_token trigger touches the row after an event write).
-- ------------------------------------------------------------
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

    INSERT INTO caldev_jmap_states (id, tenant_id, user_id, entity_type, state, updated_at)
    VALUES (uuid_generate_v4(), t, u, 'Calendar',
            encode(digest(NOW()::text || uuid_generate_v4()::text, 'sha256'), 'hex'), NOW())
    ON CONFLICT (tenant_id, user_id, entity_type)
    DO UPDATE SET state = EXCLUDED.state, updated_at = NOW();

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_caldev_calendars_state_ins ON caldev_calendars;
CREATE TRIGGER trg_caldev_calendars_state_ins AFTER INSERT ON caldev_calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

DROP TRIGGER IF EXISTS trg_caldev_calendars_state_upd ON caldev_calendars;
CREATE TRIGGER trg_caldev_calendars_state_upd
    AFTER UPDATE OF name, description, color, timezone, sort_order, is_default, is_visible, is_readonly
    ON caldev_calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

DROP TRIGGER IF EXISTS trg_caldev_calendars_state_del ON caldev_calendars;
CREATE TRIGGER trg_caldev_calendars_state_del AFTER DELETE ON caldev_calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_update_calendar_state();

-- ------------------------------------------------------------
-- Realtime change feed: every write that matters to a live UI emits a
-- NOTIFY on channel 'caldev_changes'. The server LISTENs and fans out over SSE.
--   k = what changed ('event' | 'calendar' | 'avail_share' | 'cal_share')
--   u = owning user id (event/calendar) ; o/g = owner/grantee user ids (shares)
-- ------------------------------------------------------------
CREATE OR REPLACE FUNCTION caldev_notify_owner_change()
RETURNS TRIGGER AS $$
DECLARE
    kind TEXT;
    uid  INTEGER;
    tid  INTEGER;
BEGIN
    IF TG_TABLE_NAME = 'caldev_events' THEN kind := 'event'; ELSE kind := 'calendar'; END IF;
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
    IF TG_TABLE_NAME = 'caldev_availability_shares' THEN kind := 'avail_share'; ELSE kind := 'cal_share'; END IF;
    IF TG_OP = 'DELETE' THEN
        oid := OLD.owner_user_id; gid := OLD.grantee_user_id;
    ELSE
        oid := NEW.owner_user_id; gid := NEW.grantee_user_id;
    END IF;
    PERFORM pg_notify('caldev_changes', json_build_object('k', kind, 'o', oid, 'g', gid)::text);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_caldev_events_notify ON caldev_events;
CREATE TRIGGER trg_caldev_events_notify AFTER INSERT OR UPDATE OR DELETE ON caldev_events
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

DROP TRIGGER IF EXISTS trg_caldev_calendars_notify_ins ON caldev_calendars;
CREATE TRIGGER trg_caldev_calendars_notify_ins AFTER INSERT OR DELETE ON caldev_calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

DROP TRIGGER IF EXISTS trg_caldev_calendars_notify_upd ON caldev_calendars;
CREATE TRIGGER trg_caldev_calendars_notify_upd
    AFTER UPDATE OF name, description, color, timezone, sort_order, is_default, is_visible, is_readonly
    ON caldev_calendars
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_owner_change();

DROP TRIGGER IF EXISTS trg_caldev_avail_shares_notify ON caldev_availability_shares;
CREATE TRIGGER trg_caldev_avail_shares_notify AFTER INSERT OR DELETE ON caldev_availability_shares
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_share_change();

DROP TRIGGER IF EXISTS trg_caldev_cal_shares_notify ON caldev_calendar_shares;
CREATE TRIGGER trg_caldev_cal_shares_notify AFTER INSERT OR DELETE ON caldev_calendar_shares
    FOR EACH ROW EXECUTE FUNCTION caldev_notify_share_change();

-- Helps availability lookups (time-bounded scans per owner)
CREATE INDEX IF NOT EXISTS idx_caldev_events_owner_time ON caldev_events (tenant_id, user_id, dtstart);
