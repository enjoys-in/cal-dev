package store

// Integration test for SearchPeople (the guest picker). Needs a disposable PostgreSQL server:
//
//	CALDEV_TEST_DATABASE_URL=postgres://postgres:pw@localhost:55432/postgres?sslmode=disable \
//	    go test ./internal/store -run TestSearchPeople -v

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/models"
)

func TestSearchPeople(t *testing.T) {
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
	dbName := fmt.Sprintf("caldev_people_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")

	dbURL := *u
	dbURL.Path = "/" + dbName
	setup, err := sql.Open("postgres", dbURL.String())
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, setup, `CREATE SCHEMA caldev;
CREATE TABLE caldev.mail_accounts (
    id INTEGER PRIMARY KEY, name VARCHAR(255) NOT NULL DEFAULT '', email VARCHAR(255) NOT NULL UNIQUE,
    tenant_name VARCHAR(255) NOT NULL DEFAULT '', status VARCHAR(50) NOT NULL DEFAULT 'ACTIVE',
    type VARCHAR(50) NOT NULL DEFAULT 'USER', account_origin VARCHAR(16) NOT NULL DEFAULT 'external',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW());
INSERT INTO caldev.mail_accounts (id, name, email, tenant_name, status) VALUES
 (1, 'Me Myself',  'me@acme.com',      'acme.com',    'ACTIVE'),
 (2, 'Annie Lee',  'annie@acme.com',   'acme.com',    'ACTIVE'),
 (3, 'Bob Stone',  'bob@acme.com',     'acme.com',    'ACTIVE'),
 (4, 'Joanna Fox', 'jo@acme.com',      '',            'ACTIVE'),     -- same domain, no tenant name
 (5, 'Dave Gone',  'dave@acme.com',    'acme.com',    'SUSPENDED'),  -- not active
 (6, 'Eve Other',  'eve@other.com',    'other.com',   'ACTIVE'),     -- another organisation
 (7, '100% Real',  'real_1@acme.com',  'acme.com',    'ACTIVE');     -- LIKE wildcards in data`)
	setup.Close()

	port, _ := strconv.Atoi(u.Port())
	pass, _ := u.User.Password()
	s, err := New(&config.DatabaseConfig{
		Host: u.Hostname(), Port: port, User: u.User.Username(), Password: pass, DBName: dbName,
		SSLMode: "disable", Schema: "caldev", MaxOpenConns: 2, MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	me := &models.User{ID: 1, Email: "me@acme.com", TenantName: "acme.com"}
	emails := func(q string, limit int) []string {
		t.Helper()
		got, err := s.SearchPeople(ctx, me, q, limit)
		if err != nil {
			t.Fatalf("SearchPeople(%q): %v", q, err)
		}
		var out []string
		for _, p := range got {
			out = append(out, p.Email)
		}
		return out
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}

	// own organisation only, active only, never the viewer, alphabetical by name
	eq("everyone", emails("", 10), []string{"real_1@acme.com", "annie@acme.com", "bob@acme.com", "jo@acme.com"})
	eq("email prefix", emails("bo", 10), []string{"bob@acme.com"})
	eq("part of the name", emails("fox", 10), []string{"jo@acme.com"})
	eq("case", emails("ANN", 10), []string{"annie@acme.com"})
	eq("other organisation hidden", emails("eve", 10), nil)
	eq("suspended hidden", emails("dave", 10), nil)
	eq("wildcards are literal", emails("%", 10), []string{"real_1@acme.com"})
	eq("underscore is literal", emails("real_", 10), []string{"real_1@acme.com"})
	eq("limit", emails("", 2), []string{"real_1@acme.com", "annie@acme.com"})

	// a viewer without tenant name falls back to the email domain
	got, err := s.SearchPeople(ctx, &models.User{ID: 4, Email: "jo@acme.com"}, "annie", 10)
	if err != nil || len(got) != 1 || got[0].Email != "annie@acme.com" {
		t.Errorf("domain fallback: %v %v", got, err)
	}
	// nothing to scope to: never list everyone
	got, err = s.SearchPeople(ctx, &models.User{ID: 9, Email: "weird"}, "", 10)
	if err != nil || len(got) != 0 {
		t.Errorf("unscoped viewer must see nobody: %v %v", got, err)
	}
}
