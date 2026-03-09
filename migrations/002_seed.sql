-- ============================================================
-- Seed: test app password and default calendar
-- Runs after 001_init.sql
-- Assumes a mail_accounts row already exists for testing.
-- ============================================================

-- To seed test data, first ensure a mail_accounts row exists.
-- Example: INSERT INTO mail_accounts (email, name, ...) VALUES (...);
-- Then use that mail_accounts.id for the seed data below.

-- NOTE: Replace '1' with the actual mail_accounts.id of your test user.

-- App password: "testpassword" (bcrypt hash)
-- Use email: <your_test_email>  password: testpassword
INSERT INTO caldev_app_passwords (id, user_id, label, password_hash, scopes)
VALUES (
    'c0000000-0000-0000-0000-000000000001',
    1, -- mail_accounts.id of test user
    'Development',
    -- bcrypt hash of "testpassword"
    '$2a$10$5dKhVwtarYYaw5Pe5ufaWOZATJR/1yXNaUJHYwKedYOLorpGRVIRq',
    ARRAY['caldav','jmap']
) ON CONFLICT (id) DO NOTHING;

-- Default calendar for test user
INSERT INTO caldev_calendars (id, tenant_id, user_id, name, description, color, timezone, is_default, ctag, sync_token)
VALUES (
    'd0000000-0000-0000-0000-000000000001',
    1, -- mail_accounts.id (used as tenant_id)
    1, -- mail_accounts.id (used as user_id)
    'Personal',
    'Default calendar',
    '#0078D4',
    'UTC',
    TRUE,
    uuid_generate_v4()::text,
    'sync-' || uuid_generate_v4()::text
) ON CONFLICT (id) DO NOTHING;
