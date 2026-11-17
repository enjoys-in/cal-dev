package store

import (
	"fmt"
)

// MigrationHistoryTable is CalDev's own record of applied migration files (inside its schema).
const migrationHistoryDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	id SERIAL PRIMARY KEY,
	filename VARCHAR(512) NOT NULL UNIQUE,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// ApplyMigrations brings the schema up to date: it creates the history table, recognises a schema
// that was built from schema/full_init.sql, then runs every file in files that is not recorded yet,
// in order. A missing, unreadable or failing migration is an error (running on a partial schema
// breaks later features).
//
//   - read returns a migration file's SQL;
//   - baseline lists the migrations whose result schema/full_init.sql already contains (they only
//     built the original layout). When the schema already holds CalDev's final tables and no
//     migration has been recorded, those are recorded as applied instead of run — running them
//     would create a second, empty, prefixed copy of every table. Later migrations still run:
//     they are safe on the final shape;
//   - logf receives one line per migration.
func (s *Store) ApplyMigrations(files, baseline []string, read func(string) ([]byte, error), logf func(format string, args ...any)) error {
	if _, err := s.db.Exec(migrationHistoryDDL); err != nil {
		return fmt.Errorf("create migration history: %w", err)
	}

	if recorded, err := s.baselineBuiltSchema(baseline); err != nil {
		return fmt.Errorf("check for a schema built from full_init.sql: %w", err)
	} else if recorded > 0 {
		logf("Schema was built from schema/full_init.sql: marked %d original migrations as applied", recorded)
	}

	for _, mf := range files {
		sqlBytes, err := read(mf)
		if err != nil {
			return fmt.Errorf("migration file %s not readable (is the working directory the app root?): %w", mf, err)
		}
		applied, err := s.IsMigrationApplied(mf)
		if err != nil {
			return fmt.Errorf("checking migration %s: %w", mf, err)
		}
		if applied {
			logf("Migration %s already applied, skipping", mf)
			continue
		}
		if err := s.RunMigration(string(sqlBytes)); err != nil {
			return fmt.Errorf("migration %s failed: %w", mf, err)
		}
		logf("Migration %s applied successfully", mf)
		if err := s.RecordMigration(mf); err != nil {
			logf("Failed to record migration %s: %v", mf, err)
		}
	}
	return nil
}

// baselineBuiltSchema records the baseline migrations as applied when CalDev's schema already holds
// its final tables (events is the one every version has) and nothing is recorded yet. Returns how
// many were recorded. An old install (history present) or a new empty schema is left alone.
func (s *Store) baselineBuiltSchema(baseline []string) (int, error) {
	var built, hasHistory bool
	err := s.db.QueryRow(`SELECT to_regclass(current_schema() || '.events') IS NOT NULL,
		EXISTS (SELECT 1 FROM schema_migrations)`).Scan(&built, &hasHistory)
	if err != nil {
		return 0, err
	}
	if !built || hasHistory {
		return 0, nil
	}
	n := 0
	for _, f := range baseline {
		res, err := s.db.Exec(`INSERT INTO schema_migrations (filename) VALUES ($1) ON CONFLICT (filename) DO NOTHING`, f)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, nil
}
