package store

import (
	"fmt"
	"strings"
)

// prepareSchemaSQL creates CalDev's schema and moves every CalDev object an older version created
// in public (tables, their sequences/indexes/constraints/triggers, loose sequences, functions)
// into it with ALTER ... SET SCHEMA, and names the migration history schema_migrations. The other
// tables keep their caldev_ names here; migration 009 removes the prefix. Rows are not copied: the tables themselves move, so there is
// no window where data exists twice or not at all, and nothing is dropped. Safe to run on every
// start — once public holds no caldev_* object it does nothing.
//
// __SCHEMA__ is replaced by a validated identifier (config.SchemaName).
const prepareSchemaSQL = `
-- Extensions go into public. Without this, migration 001 would install them in the first schema of
-- the search_path (CalDev's own) on a new database. No-op when they exist anywhere already.
CREATE EXTENSION IF NOT EXISTS "uuid-ossp" WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS "pgcrypto" WITH SCHEMA public;

CREATE SCHEMA IF NOT EXISTS __SCHEMA__;

DO $caldev$
DECLARE
    target text := '__SCHEMA__';
    r record;
BEGIN
    -- Tables. Owned sequences (SERIAL), indexes, constraints and triggers move with each table.
    FOR r IN
        SELECT c.relname
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname LIKE 'caldev\_%'
    LOOP
        -- the same table already in the schema, under its old or its new (unprefixed) name
        IF to_regclass(format('%I.%I', target, r.relname)) IS NOT NULL
           OR to_regclass(format('%I.%I', target, substr(r.relname, 8))) IS NOT NULL
           OR (r.relname = 'caldev_migrations' AND to_regclass(format('%I.schema_migrations', target)) IS NOT NULL) THEN
            RAISE EXCEPTION 'public.% is also in schema %; keep one copy by hand before starting CalDev',
                r.relname, target;
        END IF;
        EXECUTE format('ALTER TABLE public.%I SET SCHEMA %I', r.relname, target);
        RAISE NOTICE 'moved table public.% to %', r.relname, target;
    END LOOP;

    -- The migration history must be found before migrations run (009 renames the other tables).
    IF to_regclass(format('%I.schema_migrations', target)) IS NULL THEN
        IF to_regclass(format('%I.caldev_migrations', target)) IS NOT NULL THEN
            EXECUTE format('ALTER TABLE %I.caldev_migrations RENAME TO schema_migrations', target);
        ELSIF to_regclass(format('%I.migrations', target)) IS NOT NULL THEN
            EXECUTE format('ALTER TABLE %I.migrations RENAME TO schema_migrations', target);
        END IF;
    END IF;

    -- Sequences not owned by a table
    FOR r IN
        SELECT c.relname
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relkind = 'S' AND c.relname LIKE 'caldev\_%'
    LOOP
        EXECUTE format('ALTER SEQUENCE public.%I SET SCHEMA %I', r.relname, target);
    END LOOP;

    -- Functions (trigger functions keep working: triggers reference them by id)
    FOR r IN
        SELECT p.oid::regprocedure AS sig
        FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public' AND p.proname LIKE 'caldev\_%'
    LOOP
        EXECUTE format('ALTER FUNCTION %s SET SCHEMA %I', r.sig, target);
    END LOOP;

    -- Function bodies name tables without a schema: pin their search_path so a trigger fired from
    -- any session (psql, the mail system) still finds CalDev's tables.
    FOR r IN
        SELECT p.oid::regprocedure AS sig
        FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = target AND p.proname LIKE 'caldev\_%'
    LOOP
        EXECUTE format('ALTER FUNCTION %s SET search_path = %I, public', r.sig, target);
    END LOOP;
END
$caldev$;
`

// PrepareSchema makes sure CalDev's schema exists and holds every CalDev object (see
// prepareSchemaSQL). schema must already be validated (config.DatabaseConfig.SchemaName).
func (s *Store) PrepareSchema(schema string) error {
	if schema == "" || schema == "public" || strings.ContainsAny(schema, `"';- `) {
		return fmt.Errorf("store: refusing schema %q", schema)
	}
	_, err := s.db.Exec(strings.ReplaceAll(prepareSchemaSQL, "__SCHEMA__", schema))
	return err
}
