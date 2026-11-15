-- ============================================================
-- 007: API keys for user onboarding
--
-- External services that can't hold the shared JWT secret call
-- POST /api/users/onboard (or /api/calendar/onboard) with an X-API-Key.
-- A key is linked to an owner email and may only onboard users of that
-- email's domain. Only a SHA-256 hash of the key is stored; the key itself
-- is shown once, when it is created.
-- ============================================================
CREATE TABLE IF NOT EXISTS caldev_api_keys (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    owner_email   VARCHAR(512) NOT NULL,
    domain        VARCHAR(255) NOT NULL,          -- lower-case domain the key may onboard (*@domain)
    label         VARCHAR(255) NOT NULL DEFAULT '',
    key_prefix    VARCHAR(16)  NOT NULL,          -- first characters, to recognise a key in lists
    key_hash      CHAR(64)     NOT NULL UNIQUE,   -- hex SHA-256 of the full key
    created_by    INTEGER,                        -- mail_accounts.id of the creator; NULL = admin
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_caldev_api_keys_owner ON caldev_api_keys(lower(owner_email));
