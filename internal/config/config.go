package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for the server.
type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig // Valkey-compatible (wire protocol == Redis)
	Auth     AuthConfig
	App      AppConfig
	SMTP     SMTPConfig
	TLS      TLSConfig
	Worker   WorkerConfig
}

// AppConfig holds application-level header validation settings.
type AppConfig struct {
	APIKey    string // expected value of x-api-key header
	Name      string // expected value of X-App-Name header
	Version   string // expected value of X-App-Version header
	Signature string // expected value of x-signature header
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host            string
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	Prefork         bool
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	Host            string
	Port            int
	User            string
	Password        string
	DBName          string
	Schema          string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Host     string
	Port     int
	Password string
	DB       int
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	JWTSecret        string
	JWTIssuer        string // shared issuer with mail/other services
	TokenExpiry      time.Duration
	OAuth2Enabled    bool
	BasicAuthEnabled bool
	AdminAPIKey      string // API key for admin endpoints (user provisioning, etc.)
}

// WorkerConfig holds background job worker settings.
type WorkerConfig struct {
	Concurrency     int
	QueuePrefix     string
	RetryMax        int
	RetryDelay      time.Duration
	ShutdownTimeout time.Duration
}

// SMTPConfig holds SMTP settings for scheduling invitations.
type SMTPConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	UseTLS   bool
}

// TLSConfig holds TLS/HTTPS settings.
type TLSConfig struct {
	Enabled  bool
	CertFile string
	KeyFile  string
}

// DSN returns the PostgreSQL connection string.
func (d *DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s search_path=%s,public",
		d.Host, d.Port, d.User, d.Password, d.DBName, d.SSLMode, d.Schema,
	)
}

// RedisAddr returns the Redis address.
func (r *RedisConfig) RedisAddr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

// Addr returns the server listen address.
func (s *ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Server: ServerConfig{
			Host:            envStr("SERVER_HOST", "0.0.0.0"),
			Port:            envInt("SERVER_PORT", 8443),
			ReadTimeout:     envDuration("SERVER_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:    envDuration("SERVER_WRITE_TIMEOUT", 60*time.Second),
			IdleTimeout:     envDuration("SERVER_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: envDuration("SERVER_SHUTDOWN_TIMEOUT", 15*time.Second),
			Prefork:         envBool("SERVER_PREFORK", false),
		},
		Database: DatabaseConfig{
			Host:            envStr("DB_HOST", "localhost"),
			Port:            envInt("DB_PORT", 5432),
			User:            envStr("DB_USER", "caldev"),
			Password:        envStr("DB_PASSWORD", "caldev"),
			DBName:          envStr("DB_NAME", "caldev"),
			Schema:          envStr("DB_SCHEMA", "caldev"),
			SSLMode:         envStr("DB_SSLMODE", "disable"),
			MaxOpenConns:    envInt("DB_MAX_OPEN_CONNS", 50),
			MaxIdleConns:    envInt("DB_MAX_IDLE_CONNS", 10),
			ConnMaxLifetime: envDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnMaxIdleTime: envDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
		},
		Redis: RedisConfig{
			Host:     envStr("REDIS_HOST", "localhost"),
			Port:     envInt("REDIS_PORT", 6379),
			Password: envStr("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0),
		},
		Auth: AuthConfig{
			JWTSecret:        envStr("AUTH_JWT_SECRET", "change-me-in-production"),
			JWTIssuer:        envStr("AUTH_JWT_ISSUER", ""),
			TokenExpiry:      envDuration("AUTH_TOKEN_EXPIRY", 24*time.Hour),
			OAuth2Enabled:    envBool("AUTH_OAUTH2_ENABLED", false),
			BasicAuthEnabled: envBool("AUTH_BASIC_ENABLED", true),
			AdminAPIKey:      envStr("ADMIN_API_KEY", ""),
		},
		App: AppConfig{
			APIKey:    envStr("APP_API_KEY", ""),
			Name:      envStr("APP_NAME", "AirSend"),
			Version:   envStr("APP_VERSION", "1.0.0"),
			Signature: envStr("APP_SIGNATURE", ""),
		},
		SMTP: SMTPConfig{
			Host:     envStr("SMTP_HOST", "localhost"),
			Port:     envInt("SMTP_PORT", 587),
			User:     envStr("SMTP_USER", ""),
			Password: envStr("SMTP_PASSWORD", ""),
			From:     envStr("SMTP_FROM", "calendar@localhost"),
			UseTLS:   envBool("SMTP_USE_TLS", true),
		},
		TLS: TLSConfig{
			Enabled:  envBool("TLS_ENABLED", false),
			CertFile: envStr("TLS_CERT_FILE", ""),
			KeyFile:  envStr("TLS_KEY_FILE", ""),
		},
		Worker: WorkerConfig{
			Concurrency:     envInt("WORKER_CONCURRENCY", 4),
			QueuePrefix:     envStr("WORKER_QUEUE_PREFIX", "caldev:jobs"),
			RetryMax:        envInt("WORKER_RETRY_MAX", 3),
			RetryDelay:      envDuration("WORKER_RETRY_DELAY", 5*time.Second),
			ShutdownTimeout: envDuration("WORKER_SHUTDOWN_TIMEOUT", 10*time.Second),
		},
	}
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
