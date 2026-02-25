-- ============================================================
-- Seed: test tenant, app password, and default calendar
-- Runs after 001_init.sql (caldev schema exists)
-- ============================================================

-- Test tenant
INSERT INTO caldev.tenants (id, slug, name)
VALUES (
    'b0000000-0000-0000-0000-000000000001',
    'default',
    'Default Tenant'
) ON CONFLICT (id) DO NOTHING;

-- App password: "testpassword" (bcrypt hash)
-- Use email: test@caldev.local  password: testpassword
INSERT INTO caldev.app_passwords (id, user_id, label, password_hash, scopes)
VALUES (
    'c0000000-0000-0000-0000-000000000001',
    'a0000000-0000-0000-0000-000000000001',
    'Development',
    -- bcrypt hash of "testpassword"
    '$2a$10$5dKhVwtarYYaw5Pe5ufaWOZATJR/1yXNaUJHYwKedYOLorpGRVIRq',
    ARRAY['caldav','jmap']
) ON CONFLICT (id) DO NOTHING;

-- Default calendar for test user
INSERT INTO caldev.calendars (id, tenant_id, user_id, name, description, color, timezone, is_default, ctag, sync_token)
VALUES (
    'd0000000-0000-0000-0000-000000000001',
    'b0000000-0000-0000-0000-000000000001',
    'a0000000-0000-0000-0000-000000000001',
    'Personal',
    'Default calendar',
    '#0078D4',
    'UTC',
    TRUE,
    uuid_generate_v4()::text,
    'sync-' || uuid_generate_v4()::text
) ON CONFLICT (id) DO NOTHING;
