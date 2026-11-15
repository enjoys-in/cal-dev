package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/google/uuid"
)

// GetCalendarSMTP returns the SMTP configuration for a specific calendar.
func (s *Store) GetCalendarSMTP(ctx context.Context, tenantID int, calendarID string) (*models.CalendarSMTP, error) {
	cfg := &models.CalendarSMTP{}
	query := `SELECT id, calendar_id, tenant_id, user_id, host, port, username,
		password_encrypted, from_email, from_name, reply_to, encryption, status,
		last_tested_at, last_error, created_at, updated_at
		FROM caldev_calendar_smtp
		WHERE tenant_id = $1 AND calendar_id = $2`
	err := s.db.QueryRowContext(ctx, query, tenantID, calendarID).
		Scan(&cfg.ID, &cfg.CalendarID, &cfg.TenantID, &cfg.UserID, &cfg.Host, &cfg.Port,
			&cfg.Username, &cfg.PasswordEncrypted, &cfg.FromEmail, &cfg.FromName, &cfg.ReplyTo,
			&cfg.Encryption, &cfg.Status, &cfg.LastTestedAt, &cfg.LastError, &cfg.CreatedAt, &cfg.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return cfg, err
}

// GetCalendarSMTPByCalendarID fetches calendar SMTP by calendar ID alone (used by background workers).
func (s *Store) GetCalendarSMTPByCalendarID(ctx context.Context, calendarID string) (*models.CalendarSMTP, error) {
	cfg := &models.CalendarSMTP{}
	query := `SELECT id, calendar_id, tenant_id, user_id, host, port, username,
		password_encrypted, from_email, from_name, reply_to, encryption, status,
		last_tested_at, last_error, created_at, updated_at
		FROM caldev_calendar_smtp
		WHERE calendar_id = $1`
	err := s.db.QueryRowContext(ctx, query, calendarID).
		Scan(&cfg.ID, &cfg.CalendarID, &cfg.TenantID, &cfg.UserID, &cfg.Host, &cfg.Port,
			&cfg.Username, &cfg.PasswordEncrypted, &cfg.FromEmail, &cfg.FromName, &cfg.ReplyTo,
			&cfg.Encryption, &cfg.Status, &cfg.LastTestedAt, &cfg.LastError, &cfg.CreatedAt, &cfg.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return cfg, err
}

// UpsertCalendarSMTP creates or updates the SMTP configuration for a calendar.
func (s *Store) UpsertCalendarSMTP(ctx context.Context, smtp *models.CalendarSMTP) error {
	if smtp.ID == "" {
		smtp.ID = uuid.New().String()
	}
	now := time.Now()
	query := `INSERT INTO caldev_calendar_smtp
		(id, calendar_id, tenant_id, user_id, host, port, username, password_encrypted,
		 from_email, from_name, reply_to, encryption, status, last_tested_at, last_error, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)
		ON CONFLICT (calendar_id) DO UPDATE SET
			host = EXCLUDED.host,
			port = EXCLUDED.port,
			username = EXCLUDED.username,
			password_encrypted = CASE WHEN EXCLUDED.password_encrypted != '' THEN EXCLUDED.password_encrypted ELSE caldev_calendar_smtp.password_encrypted END,
			from_email = EXCLUDED.from_email,
			from_name = EXCLUDED.from_name,
			reply_to = EXCLUDED.reply_to,
			encryption = EXCLUDED.encryption,
			status = EXCLUDED.status,
			last_tested_at = EXCLUDED.last_tested_at,
			last_error = EXCLUDED.last_error,
			updated_at = EXCLUDED.updated_at
		RETURNING id, created_at, updated_at`

	return s.db.QueryRowContext(ctx, query,
		smtp.ID, smtp.CalendarID, smtp.TenantID, smtp.UserID, smtp.Host, smtp.Port,
		smtp.Username, smtp.PasswordEncrypted, smtp.FromEmail, smtp.FromName, smtp.ReplyTo,
		smtp.Encryption, smtp.Status, smtp.LastTestedAt, smtp.LastError, now).
		Scan(&smtp.ID, &smtp.CreatedAt, &smtp.UpdatedAt)
}

// DeleteCalendarSMTP removes the SMTP configuration for a calendar.
func (s *Store) DeleteCalendarSMTP(ctx context.Context, tenantID int, calendarID string) error {
	query := `DELETE FROM caldev_calendar_smtp WHERE tenant_id = $1 AND calendar_id = $2`
	_, err := s.db.ExecContext(ctx, query, tenantID, calendarID)
	return err
}

// UpdateCalendarSMTPTestResult records the result of a connection test or delivery attempt.
func (s *Store) UpdateCalendarSMTPTestResult(ctx context.Context, calendarID string, status, lastError string) error {
	query := `UPDATE caldev_calendar_smtp
		SET status = $2, last_error = $3, last_tested_at = NOW(), updated_at = NOW()
		WHERE calendar_id = $1`
	_, err := s.db.ExecContext(ctx, query, calendarID, status, lastError)
	return err
}
