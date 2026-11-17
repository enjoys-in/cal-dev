package store

import (
	"fmt"
	"strings"
)

// prepareSchemaSQL creates CalDev's schema and moves every CalDev object an older version created
// in public (tables, their sequences/indexes/constraints/triggers, loose sequences, functions)
// into it with ALTER ... SET SCHEMA. Rows are not copied: the tables themselves move, so there is
// no window where data exists twice or not at all, and nothing is dropped. Safe to run on every
// start — once public holds no caldev_* object it does nothing.
//
// __SCHEMA__ is replaced by a validated identifier (config.SchemaName).
const prepareSchemaSQL = `
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
        IF to_regclass(format('%I.%I', target, r.relname)) IS NOT NULL THEN
            RAISE EXCEPTION 'both public.% and %.% exist; keep one by hand before starting CalDev',
                r.relname, target, r.relname;
        END IF;
        EXECUTE format('ALTER TABLE public.%I SET SCHEMA %I', r.relname, target);
        RAISE NOTICE 'moved table public.% to %', r.relname, target;
    END LOOP;

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
