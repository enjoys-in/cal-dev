-- 010: CalDev's own account table, <schema>.mail_accounts (search_path puts CalDev's schema first).
--
--   internal = an Airsend mailbox: a copy of public.mail_accounts with the SAME id and email
--              (email is the key). Added the first time the user reaches CalDev with the
--              calendar enabled (account_settings.calender_config.enable_calender).
--   external = created by CalDev's onboarding API; exists only here. New ids come from
--              external_account_id_seq (starts at 1,000,000,000) so they never meet an
--              Airsend id: every CalDev table stores only the number.
--   status   = CalDev's own switch for the calendar: ACTIVE | INACTIVE | SUSPENDED.
--
-- CalDev never writes to public.mail_accounts. Existing accounts already used by CalDev are copied
-- here once, keeping their ids, so all calendars/events keep pointing at the right account.

CREATE TABLE IF NOT EXISTS mail_accounts (
    id          INTEGER PRIMARY KEY,
    name        VARCHAR(255) NOT NULL DEFAULT '',
    email       VARCHAR(255) NOT NULL UNIQUE,
    tenant_name VARCHAR(255) NOT NULL DEFAULT '',
    status      VARCHAR(50)  NOT NULL DEFAULT 'ACTIVE',
    type        VARCHAR(50)  NOT NULL DEFAULT 'USER',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
-- (on a brand-new database, migration 001 already created this table with id SERIAL — same columns)
ALTER TABLE mail_accounts ADD COLUMN IF NOT EXISTS account_origin VARCHAR(16);

CREATE SEQUENCE IF NOT EXISTS external_account_id_seq START 1000000000;
ALTER TABLE mail_accounts ALTER COLUMN id SET DEFAULT nextval('external_account_id_seq');

CREATE UNIQUE INDEX IF NOT EXISTS idx_caldev_mail_accounts_email_lower ON mail_accounts (lower(email));

-- ── one-time copy from the mail system's table (skipped when there is none: standalone CalDev) ──
DO $caldev$
DECLARE
    has_origin   boolean;
    has_password boolean;
    origin_expr  text;
BEGIN
    IF to_regclass('public.mail_accounts') IS NULL THEN
        RETURN;
    END IF;

    SELECT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = 'public' AND table_name = 'mail_accounts' AND column_name = 'account_origin')
      INTO has_origin;
    SELECT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = 'public' AND table_name = 'mail_accounts' AND column_name = 'password')
      INTO has_password;

    -- Origin: the mail system's own value when set; otherwise an account without a password was
    -- created by CalDev's onboarding (external), one with a password is an Airsend mailbox.
    origin_expr := CASE
        WHEN has_password THEN $e$CASE WHEN COALESCE(m.password, '') = '' THEN 'external' ELSE 'internal' END$e$
        ELSE $e$'external'$e$
    END;
    IF has_origin THEN
        origin_expr := format('COALESCE(m.account_origin, %s)', origin_expr);
    END IF;

    -- Copy every account CalDev already uses (referenced by its data) plus every external one.
    EXECUTE format($q$
        INSERT INTO mail_accounts (id, name, email, tenant_name, status, type, account_origin, created_at, updated_at)
        SELECT m.id,
               COALESCE(m.name, ''),
               lower(m.email),
               COALESCE(m.tenant_name, ''),
               CASE
                   WHEN %1$s = 'internal' THEN 'ACTIVE'            -- Airsend status is checked live
                   WHEN upper(COALESCE(m.status, 'ACTIVE')) = 'ACTIVE' THEN 'ACTIVE'
                   WHEN upper(m.status) = 'INACTIVE' THEN 'INACTIVE'
                   ELSE 'SUSPENDED'
               END,
               COALESCE(m.type, 'USER'),
               %1$s,
               COALESCE(m.created_at::timestamptz, NOW()),
               NOW()
        FROM public.mail_accounts m
        WHERE %1$s = 'external'
           OR m.id IN (SELECT user_id   FROM calendars
                       UNION SELECT tenant_id FROM calendars
                       UNION SELECT user_id   FROM events
                       UNION SELECT user_id   FROM app_passwords
                       UNION SELECT owner_user_id   FROM availability_shares
                       UNION SELECT grantee_user_id FROM availability_shares
                       UNION SELECT owner_user_id   FROM calendar_shares
                       UNION SELECT grantee_user_id FROM calendar_shares)
        ORDER BY m.id
        ON CONFLICT DO NOTHING
    $q$, origin_expr);
END
$caldev$;

ALTER TABLE mail_accounts ALTER COLUMN account_origin SET DEFAULT 'external';
UPDATE mail_accounts SET account_origin = 'external' WHERE account_origin IS NULL;
ALTER TABLE mail_accounts ALTER COLUMN account_origin SET NOT NULL;
UPDATE mail_accounts SET status = upper(status);
UPDATE mail_accounts SET status = 'SUSPENDED' WHERE status NOT IN ('ACTIVE', 'INACTIVE', 'SUSPENDED');

DO $caldev$ BEGIN
    ALTER TABLE mail_accounts ADD CONSTRAINT chk_caldev_mail_accounts_origin
        CHECK (account_origin IN ('internal', 'external'));
EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL; END
$caldev$;

DO $caldev$ BEGIN
    ALTER TABLE mail_accounts ADD CONSTRAINT chk_caldev_mail_accounts_status
        CHECK (status IN ('ACTIVE', 'INACTIVE', 'SUSPENDED'));
EXCEPTION WHEN duplicate_object OR duplicate_table THEN NULL; END
$caldev$;
