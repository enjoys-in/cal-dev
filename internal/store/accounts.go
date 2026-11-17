package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/lib/pq"
)

// ============================================================
// Accounts — CalDev's own table, <schema>.mail_accounts (migration 010)
//
//   internal: an Airsend mailbox. A copy of public.mail_accounts with the same id and email
//             (email is the key), added the first time the user reaches CalDev while the
//             calendar is enabled in Airsend (public.account_settings.calender_config).
//   external: created by the onboarding API; exists only here (ids from 1,000,000,000 up).
//   status:   CalDev's own ACTIVE | INACTIVE | SUSPENDED.
//
// CalDev never writes to public.mail_accounts. Unqualified "mail_accounts" is CalDev's table
// (its schema is first on search_path); the mail system's tables are always named "public.".
// ============================================================

// Account origins.
const (
	AccountOriginInternal = "internal" // Airsend mailbox: calendar mail via airsend-api
	AccountOriginExternal = "external" // CalDev-only account: calendar mail via BYO SMTP
)

// CalDev account statuses.
const (
	AccountActive    = "ACTIVE"
	AccountInactive  = "INACTIVE"
	AccountSuspended = "SUSPENDED"
)

// ErrCalendarDisabled: the email belongs to an Airsend mailbox whose calendar is turned off (or
// which is not active), so CalDev must not create a separate account for it.
var ErrCalendarDisabled = errors.New("airsend account exists but its calendar is not enabled")

const userColumns = `id, COALESCE(name, '') AS name, email, COALESCE(tenant_name, '') AS tenant_name,
	upper(COALESCE(status, 'ACTIVE')) AS status, COALESCE(type, 'USER') AS type,
	COALESCE(account_origin, 'external') AS account_origin, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(r rowScanner) (*models.User, error) {
	u := &models.User{}
	err := r.Scan(&u.ID, &u.Name, &u.Email, &u.TenantName, &u.Status, &u.Type, &u.AccountOrigin, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func normEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// missingMailSystem reports a query that failed only because the mail system's tables (or a
// column of them) are not there — a standalone CalDev without Airsend.
func missingMailSystem(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && (pqErr.Code == "42P01" || pqErr.Code == "42703")
}

// lookupUserByEmail finds an account in CalDev's table only (no provisioning).
func (s *Store) lookupUserByEmail(ctx context.Context, email string) (*models.User, error) {
	email = normEmail(email)
	if email == "" {
		return nil, nil
	}
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM mail_accounts WHERE email = $1`, email))
	if errors.Is(err, sql.ErrNoRows) {
		// a row stored with capitals
		u, err = scanUser(s.db.QueryRowContext(ctx,
			`SELECT `+userColumns+` FROM mail_accounts WHERE lower(email) = $1 ORDER BY id LIMIT 1`, email))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// GetUserByEmail returns the CalDev account for email. When CalDev has none yet but the email is
// an active Airsend mailbox with the calendar enabled, the account is added first (internal copy).
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*models.User, error) {
	u, err := s.lookupUserByEmail(ctx, email)
	if err != nil || u != nil {
		return u, err
	}
	return s.ProvisionInternalAccount(ctx, email)
}

// GetUsersByEmails is GetUserByEmail for many addresses. The map is keyed by the lower-cased,
// trimmed address; addresses without an account (and not provisionable) are absent.
func (s *Store) GetUsersByEmails(ctx context.Context, emails []string) (map[string]*models.User, error) {
	out := make(map[string]*models.User, len(emails))
	if len(emails) == 0 {
		return out, nil
	}
	collect := func(where string, list []string) error {
		rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM mail_accounts WHERE `+where+` ORDER BY id`, pq.Array(list))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				return err
			}
			if k := normEmail(u.Email); out[k] == nil {
				out[k] = u
			}
		}
		return rows.Err()
	}

	if err := collect("email = ANY($1)", emails); err != nil {
		return nil, err
	}
	var missing []string
	for _, e := range emails {
		if k := normEmail(e); k != "" && out[k] == nil {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	if err := collect("lower(email) = ANY($1)", missing); err != nil {
		return nil, err
	}
	for _, k := range missing {
		if out[k] != nil {
			continue
		}
		u, err := s.ProvisionInternalAccount(ctx, k)
		if err != nil {
			return nil, err
		}
		if u != nil {
			out[k] = u
		}
	}
	return out, nil
}

// GetUserByID returns the CalDev account with this id (CalDev's table only).
func (s *Store) GetUserByID(ctx context.Context, id int) (*models.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM mail_accounts WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// ProvisionInternalAccount copies an Airsend mailbox into CalDev's table — same id and email —
// when it is active and has the calendar enabled. Returns nil (no error) when the email is not
// such a mailbox, or when there is no mail system (standalone CalDev).
func (s *Store) ProvisionInternalAccount(ctx context.Context, email string) (*models.User, error) {
	email = normEmail(email)
	if email == "" {
		return nil, nil
	}
	u, err := scanUser(s.db.QueryRowContext(ctx, `
		INSERT INTO mail_accounts (id, name, email, tenant_name, status, type, account_origin)
		SELECT m.id, COALESCE(m.name, ''), lower(m.email), COALESCE(m.tenant_name, ''), 'ACTIVE',
		       COALESCE(m.type, 'USER'), 'internal'
		FROM public.mail_accounts m
		JOIN public.account_settings st ON st.email_id = m.email
		WHERE lower(m.email) = $1
		  AND upper(COALESCE(m.status, 'ACTIVE')) = 'ACTIVE'
		  AND st.calender_config->>'enable_calender' = 'true'
		ORDER BY m.id
		LIMIT 1
		ON CONFLICT DO NOTHING
		RETURNING `+userColumns, email))
	switch {
	case err == nil:
		return u, nil
	case errors.Is(err, sql.ErrNoRows):
		// not provisionable, or another request added it a moment ago
		return s.lookupUserByEmail(ctx, email)
	case missingMailSystem(err):
		return nil, nil
	default:
		return nil, err
	}
}

// InternalAccountAllowed checks the mail system for an internal account: the Airsend mailbox must
// still exist, be active and have the calendar enabled. Always true without a mail system.
func (s *Store) InternalAccountAllowed(ctx context.Context, id int) (bool, string, error) {
	var status, enabled string
	err := s.db.QueryRowContext(ctx, `
		SELECT upper(COALESCE(m.status, 'ACTIVE')), COALESCE(st.calender_config->>'enable_calender', 'false')
		FROM public.mail_accounts m
		LEFT JOIN public.account_settings st ON st.email_id = m.email
		WHERE m.id = $1`, id).Scan(&status, &enabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, "mailbox removed", nil
	case missingMailSystem(err):
		return true, "", nil
	case err != nil:
		return false, "", err
	case status != "ACTIVE":
		return false, "mailbox " + strings.ToLower(status), nil
	case enabled != "true":
		return false, "calendar disabled", nil
	}
	return true, "", nil
}

// EnsureUserByEmail returns the account for email, creating it when needed (onboarding API):
//  1. an existing CalDev account is returned as it is (status and origin never change here;
//     an empty name is filled in);
//  2. an Airsend mailbox with the calendar enabled is added as internal (same id);
//     one with the calendar off returns ErrCalendarDisabled — email is the key, so no
//     second account is made for it;
//  3. otherwise a new external account (tenant_name = email domain).
func (s *Store) EnsureUserByEmail(ctx context.Context, email, name string) (*models.User, error) {
	email = normEmail(email)
	if name == "" {
		name = email
	}

	if u, err := s.lookupUserByEmail(ctx, email); err != nil || u != nil {
		if u != nil && u.Name == "" {
			if _, err := s.db.ExecContext(ctx, `UPDATE mail_accounts SET name = $2, updated_at = NOW() WHERE id = $1 AND name = ''`, u.ID, name); err == nil {
				u.Name = name
			}
		}
		return u, err
	}

	if u, err := s.ProvisionInternalAccount(ctx, email); err != nil || u != nil {
		return u, err
	}
	if exists, err := s.mailSystemHasEmail(ctx, email); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrCalendarDisabled
	}

	u, err := scanUser(s.db.QueryRowContext(ctx, `
		INSERT INTO mail_accounts (email, name, tenant_name, status, type, account_origin)
		VALUES ($1, $2, $3, 'ACTIVE', 'USER', 'external')
		ON CONFLICT DO NOTHING
		RETURNING `+userColumns, email, name, EmailDomain(email)))
	if errors.Is(err, sql.ErrNoRows) {
		return s.lookupUserByEmail(ctx, email) // created by a concurrent request
	}
	return u, err
}

// mailSystemHasEmail reports whether public.mail_accounts has this address (false without one).
func (s *Store) mailSystemHasEmail(ctx context.Context, email string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM public.mail_accounts WHERE lower(email) = $1)`, normEmail(email)).Scan(&exists)
	if missingMailSystem(err) {
		return false, nil
	}
	return exists, err
}

// GetAccountOrigin returns the account's origin ("internal" / "external"), "" when unknown.
func (s *Store) GetAccountOrigin(ctx context.Context, id int) (string, error) {
	var origin string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(account_origin, '') FROM mail_accounts WHERE id = $1`, id).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return origin, err
}

// SetAccountStatus sets CalDev's own status of an account (ACTIVE | INACTIVE | SUSPENDED).
func (s *Store) SetAccountStatus(ctx context.Context, id int, status string) error {
	status = strings.ToUpper(strings.TrimSpace(status))
	switch status {
	case AccountActive, AccountInactive, AccountSuspended:
	default:
		return errors.New("status must be ACTIVE, INACTIVE or SUSPENDED")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE mail_accounts SET status = $2, updated_at = NOW() WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SearchPeople lists the active accounts viewer would pick from when inviting guests: people of
// the viewer's own organisation (same tenant name or same email domain), matched on the start of
// their email or name (or any part of the name). An empty q lists the first people alphabetically.
// The viewer is not in the list.
func (s *Store) SearchPeople(ctx context.Context, viewer *models.User, q string, limit int) ([]*models.User, error) {
	if viewer == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	domain := ""
	if at := strings.LastIndex(viewer.Email, "@"); at >= 0 {
		domain = strings.ToLower(viewer.Email[at+1:])
	}
	tenant := strings.ToLower(strings.TrimSpace(viewer.TenantName))
	if tenant == "" && domain == "" {
		return nil, nil // nothing to scope the search to: never list everyone
	}
	like := strings.NewReplacer(`\`, `\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(strings.TrimSpace(q)))

	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM mail_accounts
		WHERE id <> $1
		  AND upper(COALESCE(status, 'ACTIVE')) = 'ACTIVE'
		  AND ( ($2::text <> '' AND lower(tenant_name) = $2::text)
		     OR ($3::text <> '' AND lower(split_part(email, '@', 2)) = $3::text) )
		  AND ( $4::text = ''
		     OR lower(email) LIKE $4::text || '%' ESCAPE '\'
		     OR lower(name) LIKE '%' || $4::text || '%' ESCAPE '\' )
		ORDER BY lower(COALESCE(NULLIF(name, ''), email)), email
		LIMIT $5`, viewer.ID, tenant, domain, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
