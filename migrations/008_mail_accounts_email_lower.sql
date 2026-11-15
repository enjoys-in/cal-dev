-- 008: index for case-insensitive account lookups (GetUserByEmail's lower(email) fallback).
-- CONCURRENTLY: mail_accounts is shared with the mail system, so don't block its writes while
-- building. CONCURRENTLY can't run inside a transaction block, so this file must stay a single
-- statement (the server sends each migration file as one query).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_mail_accounts_email_lower ON mail_accounts (lower(email));
