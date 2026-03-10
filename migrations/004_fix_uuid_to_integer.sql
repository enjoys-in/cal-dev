-- ============================================================
-- Fix: Convert tenant_id / user_id / sender_id from UUID to INTEGER
-- in ALL caldev_ tables. Idempotent — safe to re-run.
-- mail_accounts.id is INTEGER; these columns reference it.
-- ============================================================

-- Delete old migration record so this can re-run
DELETE FROM caldev_migrations WHERE filename = 'migrations/004_fix_uuid_to_integer.sql';

-- Dynamically fix ALL UUID columns in caldev_ tables
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT table_name, column_name
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name LIKE 'caldev_%'
          AND column_name IN ('tenant_id', 'user_id', 'sender_id')
          AND data_type = 'uuid'
    LOOP
        EXECUTE format('ALTER TABLE %I ALTER COLUMN %I TYPE INTEGER USING %I::text::integer',
                       r.table_name, r.column_name, r.column_name);
        RAISE NOTICE 'Fixed %.%', r.table_name, r.column_name;
    END LOOP;
END $$;
