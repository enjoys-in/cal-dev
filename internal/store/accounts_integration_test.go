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
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", "010_caldev_mail_accounts.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunMigration(string(b)); err != nil {
		t.Fatalf("migration 010: %v", err)
	}

	var leftInPublic, events int
	s.db.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname LIKE 'caldev\_%'`).Scan(&leftInPublic)
	if leftInPublic != 0 {
		t.Fatalf("%d CalDev objects still in public", leftInPublic)
	}
	s.db.QueryRow(`SELECT count(*) FROM caldev.caldev_events`).Scan(&events)
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
	mustExec(t, other, `INSERT INTO caldev.caldev_events (tenant_id, user_id, calendar_id, uid, summary, dtstart, etag, raw_ics)
		VALUES (1, 1, '11111111-1111-1111-1111-111111111111', 'ev-2@caldev', 'Review', now(), 'e2', 'BEGIN:VCALENDAR')`)
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
