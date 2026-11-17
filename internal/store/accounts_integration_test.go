package store

// Integration test for the caldev schema move and CalDev's own accounts table.
// Runs only with a disposable PostgreSQL server:
//
//	CALDEV_TEST_DATABASE_URL=postgres://postgres:pw@localhost:55432/postgres?sslmode=disable \
//	    go test ./internal/store -run TestCaldevSchemaAndAccounts -v
//
// It creates (and drops) its own database: an Airsend-like public schema, CalDev's tables as an
// older version left them in public, then runs the new startup steps and checks the results.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
)

const airsendFixture = `
CREATE TABLE users (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email varchar UNIQUE NOT NULL, name varchar NOT NULL DEFAULT '');
CREATE TABLE mail_accounts (
    id SERIAL PRIMARY KEY, name varchar, tenant_name varchar NOT NULL, email varchar UNIQUE NOT NULL,
    password varchar NOT NULL, status varchar NOT NULL DEFAULT 'ACTIVE', type varchar NOT NULL DEFAULT 'USER',
    created_at timestamp NOT NULL DEFAULT now(), updated_at timestamp NOT NULL DEFAULT now());
CREATE TABLE account_settings (id serial PRIMARY KEY, email_id varchar UNIQUE REFERENCES mail_accounts(email), calender_config jsonb);
INSERT INTO users (email) VALUES ('admin@acme.com');
INSERT INTO mail_accounts (id, name, tenant_name, email, password, status) VALUES
 (1, 'Alice', 'admin@acme.com', 'alice@acme.com', 'hash', 'ACTIVE'),     -- uses the calendar already
 (2, 'Ext',   'partner.com',    'ext@partner.com', '',    'ACTIVE'),     -- old CalDev onboarding
 (3, 'Bob',   'admin@acme.com', 'Bob@Acme.com',    'hash', 'ACTIVE'),     -- calendar on, not used yet
 (4, 'Carol', 'admin@acme.com', 'carol@acme.com',  'hash', 'ACTIVE'),     -- calendar off
 (5, 'Dave',  'admin@acme.com', 'dave@acme.com',   'hash', 'SUSPENDED');  -- suspended in Airsend
SELECT setval('mail_accounts_id_seq', 5);
INSERT INTO account_settings (email_id, calender_config) VALUES
 ('alice@acme.com', '{"enable_calender": true}'), ('Bob@Acme.com', '{"enable_calender": true}'),
 ('carol@acme.com', '{"enable_calender": false}'), ('dave@acme.com', '{"enable_calender": true}');
`

const oldCaldevData = `
INSERT INTO caldev_calendars (id, tenant_id, user_id, name) VALUES
 ('11111111-1111-1111-1111-111111111111', 1, 1, 'Alice cal'),
 ('22222222-2222-2222-2222-222222222222', 2, 2, 'Ext cal');
INSERT INTO caldev_events (tenant_id, user_id, calendar_id, uid, summary, dtstart, etag, raw_ics)
VALUES (1, 1, '11111111-1111-1111-1111-111111111111', 'ev-1@caldev', 'Standup', now(), 'e1', 'BEGIN:VCALENDAR');
`

var oldMigrations = []string{
	"001_init.sql", "002_sharing_realtime.sql", "003_recurrence_overrides.sql", "004_reminders.sql",
	"005_calendar_byo_smtp.sql", "006_change_log_fixes.sql", "007_onboarding_api_keys.sql",
	"008_mail_accounts_email_lower.sql",
}

func TestCaldevSchemaAndAccounts(t *testing.T) {
	raw := os.Getenv("CALDEV_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("CALDEV_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}

	admin, err := sql.Open("postgres", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	dbName := fmt.Sprintf("caldev_it_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")

	// ── the old layout: Airsend tables + CalDev 001..008 in public ──
	oldURL := *u
	oldURL.Path = "/" + dbName
	old, err := sql.Open("postgres", oldURL.String())
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, old, airsendFixture)
	mustExec(t, old, `CREATE TABLE caldev_migrations (id SERIAL PRIMARY KEY, filename VARCHAR(512) NOT NULL UNIQUE, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	for _, f := range oldMigrations {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", f))
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, old, string(b))
		mustExec(t, old, `INSERT INTO caldev_migrations (filename) VALUES ('migrations/`+f+`')`)
	}
	mustExec(t, old, oldCaldevData)
	old.Close()

	// ── the new startup: search_path caldev,public, PrepareSchema, migration 010 ──
	port, _ := strconv.Atoi(u.Port())
	pass, _ := u.User.Password()
	cfg := &config.DatabaseConfig{
		Host: u.Hostname(), Port: port, User: u.User.Username(), Password: pass, DBName: dbName,
		SSLMode: "disable", Schema: "caldev", MaxOpenConns: 4, MaxIdleConns: 2,
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 2; i++ { // twice: must be a no-op the second time
		if err := s.PrepareSchema(cfg.SchemaName()); err != nil {
			t.Fatalf("PrepareSchema #%d: %v", i+1, err)
		}
	}
	if applied, err := s.IsMigrationApplied("migrations/008_mail_accounts_email_lower.sql"); err != nil || !applied {
		t.Fatalf("migration history lost after the move: applied=%v err=%v", applied, err)
	}
	for _, f := range []string{"009_drop_table_prefix.sql", "010_caldev_mail_accounts.sql"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RunMigration(string(b)); err != nil {
			t.Fatalf("migration %s: %v", f, err)
		}
	}
	// no table keeps the old prefix inside the schema
	var prefixed int
	s.db.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'caldev' AND c.relkind = 'r' AND c.relname LIKE 'caldev\_%'`).Scan(&prefixed)
	if prefixed != 0 {
		t.Fatalf("%d tables still prefixed in schema caldev", prefixed)
	}

	var leftInPublic, events int
	s.db.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname LIKE 'caldev\_%'`).Scan(&leftInPublic)
	if leftInPublic != 0 {
		t.Fatalf("%d CalDev objects still in public", leftInPublic)
	}
	s.db.QueryRow(`SELECT count(*) FROM caldev.events`).Scan(&events)
	if events != 1 {
		t.Fatalf("events after move = %d, want 1", events)
	}

	// copied once, same ids
	expectUser(t, s, 1, "alice@acme.com", AccountOriginInternal)
	expectUser(t, s, 2, "ext@partner.com", AccountOriginExternal)
	if u, _ := s.GetUserByID(ctx, 3); u != nil {
		t.Fatal("bob must not be copied before he uses the calendar")
	}

	// first use with the calendar on: added as internal, same id, email lower-cased
	bob, err := s.GetUserByEmail(ctx, "BOB@acme.com")
	if err != nil || bob == nil || bob.ID != 3 || bob.AccountOrigin != AccountOriginInternal || bob.Email != "bob@acme.com" {
		t.Fatalf("bob provisioning: %+v err=%v", bob, err)
	}
	// calendar off / suspended in Airsend: not added
	for _, e := range []string{"carol@acme.com", "dave@acme.com", "nobody@acme.com"} {
		if u, err := s.GetUserByEmail(ctx, e); err != nil || u != nil {
			t.Fatalf("%s must not be provisioned: %+v err=%v", e, u, err)
		}
	}

	// onboarding: Airsend email with calendar off is refused, a new address becomes external
	if _, err := s.EnsureUserByEmail(ctx, "carol@acme.com", "Carol"); !errors.Is(err, ErrCalendarDisabled) {
		t.Fatalf("carol onboarding: want ErrCalendarDisabled, got %v", err)
	}
	ext, err := s.EnsureUserByEmail(ctx, "new@partner.com", "New")
	if err != nil || ext.AccountOrigin != AccountOriginExternal || ext.ID < 1_000_000_000 {
		t.Fatalf("external onboarding: %+v err=%v", ext, err)
	}
	if again, _ := s.EnsureUserByEmail(ctx, "NEW@partner.com", ""); again == nil || again.ID != ext.ID {
		t.Fatalf("onboarding twice must return the same account, got %+v", again)
	}

	// CalDev never wrote to public.mail_accounts
	var publicRows int
	s.db.QueryRow(`SELECT count(*) FROM public.mail_accounts`).Scan(&publicRows)
	if publicRows != 5 {
		t.Fatalf("public.mail_accounts rows = %d, want 5 (untouched)", publicRows)
	}

	// live checks against Airsend for internal accounts
	if ok, _, err := s.InternalAccountAllowed(ctx, 1); err != nil || !ok {
		t.Fatalf("alice allowed: ok=%v err=%v", ok, err)
	}
	mustExec(t, s.db, `UPDATE public.account_settings SET calender_config = '{"enable_calender": false}' WHERE email_id = 'alice@acme.com'`)
	if ok, reason, _ := s.InternalAccountAllowed(ctx, 1); ok || reason != "calendar disabled" {
		t.Fatalf("alice with calendar off: ok=%v reason=%q", ok, reason)
	}
	mustExec(t, s.db, `UPDATE public.mail_accounts SET status = 'BLOCKED' WHERE id = 3`)
	if ok, reason, _ := s.InternalAccountAllowed(ctx, 3); ok || reason != "mailbox blocked" {
		t.Fatalf("bob blocked: ok=%v reason=%q", ok, reason)
	}

	// CalDev's own status
	if err := s.SetAccountStatus(ctx, 2, "suspended"); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, s, 2, AccountSuspended)
	if err := s.SetAccountStatus(ctx, 2, "BANNED"); err == nil {
		t.Fatal("unknown status must be refused")
	}

	// triggers still work from a session that only knows public (e.g. psql, the mail system)
	plain := *u
	plain.Path = "/" + dbName
	other, err := sql.Open("postgres", plain.String())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	before := countChanges(t, other)
	mustExec(t, other, `INSERT INTO caldev.events (tenant_id, user_id, calendar_id, uid, summary, dtstart, etag, raw_ics)
		VALUES (1, 1, '11111111-1111-1111-1111-111111111111', 'ev-2@caldev', 'Review', now(), 'e2', 'BEGIN:VCALENDAR')`)
	if after := countChanges(t, other); after <= before {
		t.Fatalf("change-log trigger did not run after the rename (sync_changes %d → %d)", before, after)
	}
}

func countChanges(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM caldev.sync_changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, q)
	}
}

func expectUser(t *testing.T, s *Store, id int, email, origin string) {
	t.Helper()
	u, err := s.GetUserByID(context.Background(), id)
	if err != nil || u == nil || u.Email != email || u.AccountOrigin != origin {
		t.Fatalf("user %d: %+v err=%v (want %s, %s)", id, u, err, email, origin)
	}
}

func expectStatus(t *testing.T, s *Store, id int, status string) {
	t.Helper()
	u, _ := s.GetUserByID(context.Background(), id)
	if u == nil || u.Status != status {
		t.Fatalf("user %d status: %+v, want %s", id, u, status)
	}
}

// TestFullInitThenServerStartup: a database built from the raw schema/full_init.sql (no migration
// history) must start cleanly — the original migrations are skipped, not run on top of it — and a
// brand-new empty database must reach the same tables through the migrations.
func TestFullInitThenServerStartup(t *testing.T) {
	raw := os.Getenv("CALDEV_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("CALDEV_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("postgres", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	migrationFiles := []string{
		"migrations/001_init.sql", "migrations/002_sharing_realtime.sql", "migrations/003_recurrence_overrides.sql",
		"migrations/004_reminders.sql", "migrations/005_calendar_byo_smtp.sql", "migrations/006_change_log_fixes.sql",
		"migrations/007_onboarding_api_keys.sql", "migrations/008_mail_accounts_email_lower.sql",
		"migrations/009_drop_table_prefix.sql", "migrations/010_caldev_mail_accounts.sql",
	}
	read := func(f string) ([]byte, error) { return os.ReadFile(filepath.Join("..", "..", f)) }
	quiet := func(string, ...any) {}

	newDB := func(prefix string) (*Store, *sql.DB, func()) {
		name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatal(err)
		}
		du := *u
		du.Path = "/" + name
		plain, err := sql.Open("postgres", du.String())
		if err != nil {
			t.Fatal(err)
		}
		port, _ := strconv.Atoi(u.Port())
		pass, _ := u.User.Password()
		s, err := New(&config.DatabaseConfig{Host: u.Hostname(), Port: port, User: u.User.Username(), Password: pass,
			DBName: name, SSLMode: "disable", Schema: "caldev", MaxOpenConns: 4, MaxIdleConns: 2})
		if err != nil {
			t.Fatal(err)
		}
		return s, plain, func() {
			s.Close()
			plain.Close()
			admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		}
	}
	tableNames := func(s *Store) map[string]bool {
		rows, err := s.db.Query(`SELECT relname FROM pg_class WHERE relnamespace = 'caldev'::regnamespace AND relkind = 'r'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var n string
			rows.Scan(&n)
			out[n] = true
		}
		return out
	}

	// the raw file carries no migration bookkeeping
	sqlText, err := os.ReadFile(filepath.Join("..", "..", "schema", "full_init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sqlText), "schema_migrations") {
		t.Fatal("full_init.sql must not contain the migration history table")
	}

	// ── A: raw full_init.sql, then the server's startup ──
	s, plain, done := newDB("caldev_fi")
	defer done()
	mustExec(t, plain, string(sqlText))
	if names := tableNames(s); names["schema_migrations"] {
		t.Fatal("full_init.sql created a migration history table")
	}
	for i := 0; i < 2; i++ { // twice: the second start changes nothing
		if err := s.PrepareSchema("caldev"); err != nil {
			t.Fatal(err)
		}
		if err := s.ApplyMigrations(migrationFiles, migrationFiles[:8], read, quiet); err != nil {
			t.Fatalf("startup #%d on a full_init database: %v", i+1, err)
		}
	}
	names := tableNames(s)
	for n := range names {
		if strings.HasPrefix(n, "caldev_") {
			t.Fatalf("a prefixed table %q appeared: the original migrations ran on a built schema", n)
		}
	}
	var recorded int
	s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&recorded)
	if recorded != 10 {
		t.Fatalf("migrations recorded = %d, want 10 (8 baseline + 009 + 010)", recorded)
	}
	mustExec(t, plain, `INSERT INTO caldev.calendars (id, tenant_id, user_id, name) VALUES ('33333333-3333-3333-3333-333333333333', 1, 1, 'c')`)
	before := countChanges(t, plain)
	mustExec(t, plain, `INSERT INTO caldev.events (tenant_id, user_id, calendar_id, uid, summary, dtstart, etag, raw_ics)
		VALUES (1, 1, '33333333-3333-3333-3333-333333333333', 'x@y', 's', now(), 'e', 'ICS')`)
	if countChanges(t, plain) <= before {
		t.Fatal("change-log trigger did not fire on a database built from full_init.sql")
	}

	// ── B: brand-new empty database, migrations only ──
	s2, _, done2 := newDB("caldev_mg")
	defer done2()
	if err := s2.PrepareSchema("caldev"); err != nil {
		t.Fatal(err)
	}
	if err := s2.ApplyMigrations(migrationFiles, migrationFiles[:8], read, quiet); err != nil {
		t.Fatalf("migrations on an empty database: %v", err)
	}
	names2 := tableNames(s2)
	for n := range names {
		if !names2[n] {
			t.Errorf("table %q is in full_init.sql but the migrations did not create it", n)
		}
	}
	for n := range names2 {
		if !names[n] && n != "schema_migrations" {
			t.Errorf("table %q is created by the migrations but missing in full_init.sql", n)
		}
	}
}
