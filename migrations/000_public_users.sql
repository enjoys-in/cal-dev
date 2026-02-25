-- ============================================================
-- Standalone / Development Seed
-- Creates public.users table (normally provided by mail system)
-- and inserts a test user, tenant, and default calendar.
-- ============================================================

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Minimal public.users table (matches what the mail system provides)
CREATE TABLE IF NOT EXISTS public.users (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id    UUID NOT NULL,
    email        VARCHAR(512) NOT NULL UNIQUE,
    display_name VARCHAR(256) NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================
-- Seed data: test tenant + user + app password + default calendar
-- ============================================================

-- Test tenant
INSERT INTO public.users (id, tenant_id, email, display_name)
VALUES (
    'a0000000-0000-0000-0000-000000000001',
    'b0000000-0000-0000-0000-000000000001',
    'test@caldev.local',
    'Test User'
) ON CONFLICT (email) DO NOTHING;
