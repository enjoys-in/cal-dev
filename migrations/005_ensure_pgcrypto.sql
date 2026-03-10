-- Ensure pgcrypto extension exists (required for digest() in triggers)
CREATE EXTENSION IF NOT EXISTS pgcrypto;
