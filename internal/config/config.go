package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration for the server.
type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig // Valkey-compatible (wire protocol == Redis)
	Auth     AuthConfig
	App      AppConfig
	TLS      TLSConfig
	Worker   WorkerConfig
	Sharing  SharingConfig
}

// SharingConfig controls who can see whose free/busy.
type SharingConfig struct {
	// OrgVisible lets users with the same non-empty mail_accounts.tenant_name see each
	// other's free/busy (never event details) without an explicit share. Default false.
	OrgVisible bool
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
	SSLMode         string
	Schema          string // DB_SCHEMA: read but not applied (see DSN)
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// RedisConfig holds Redis connection settings.
// Uses a URL so credentials can be added later: redis://user:pass@host:6379/0
type RedisConfig struct {
	URL string
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	JWTSecret        string
	JWTIssuer        string // shared issuer with mail/other services
	RequireExp       bool   // reject tokens without an exp claim (AUTH_REQUIRE_EXP, default true)
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

// TLSConfig holds TLS/HTTPS settings.
type TLSConfig struct {
	Enabled  bool
	CertFile string
	KeyFile  string
}

// DSN returns the PostgreSQL connection string. Values are quoted so spaces, quotes or
// backslashes (e.g. in the password) cannot break the string or inject extra parameters.
func (d *DatabaseConfig) DSN() string {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		dsnQuote(d.Host), d.Port, dsnQuote(d.User), dsnQuote(d.Password), dsnQuote(d.DBName), dsnQuote(d.SSLMode),
	)
	// d.Schema (DB_SCHEMA) is deliberately not applied as search_path: existing deployments keep
	// their tables in public, and switching schemas silently could point the app at other tables.
	return dsn
}

// dsnQuote quotes a key/value connection-string value per lib/pq: single quotes, with
// backslash and single quote escaped by a backslash.
func dsnQuote(v string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// Addr returns the server listen address.
func (s *ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// PublicURL returns the public base URL of CalDev (from CALDEV_PUBLIC_URL or http://localhost:PORT).
func (s *ServerConfig) PublicURL() string {
	if u := os.Getenv("CALDEV_PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return fmt.Sprintf("http://localhost:%d", s.Port)
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	c := &Config{
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
			SSLMode:         envStr("DB_SSLMODE", "disable"),
			Schema:          envStr("DB_SCHEMA", ""),
			MaxOpenConns:    envInt("DB_MAX_OPEN_CONNS", 50),
			MaxIdleConns:    envInt("DB_MAX_IDLE_CONNS", 10),
			ConnMaxLifetime: envDuration("DB_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnMaxIdleTime: envDuration("DB_CONN_MAX_IDLE_TIME", 5*time.Minute),
		},
		Redis: RedisConfig{
			URL: envStr("REDIS_URL", "redis://localhost:6379/0"),
		},
		Auth: AuthConfig{
			JWTSecret:        envStr("AUTH_JWT_SECRET", "change-me-in-production"),
			JWTIssuer:        envStr("AUTH_JWT_ISSUER", ""),
			RequireExp:       envBool("AUTH_REQUIRE_EXP", true),
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

		TLS: TLSConfig{
			Enabled:  envBool("TLS_ENABLED", false),
			CertFile: envStr("TLS_CERT_FILE", ""),
			KeyFile:  envStr("TLS_KEY_FILE", ""),
		},
		Sharing: SharingConfig{
			OrgVisible: envBool("AVAILABILITY_ORG_VISIBLE", false),
		},
		Worker: WorkerConfig{
			Concurrency:     envInt("WORKER_CONCURRENCY", 4),
			QueuePrefix:     envStr("WORKER_QUEUE_PREFIX", "caldev:jobs"),
			RetryMax:        envInt("WORKER_RETRY_MAX", 3),
			RetryDelay:      envDuration("WORKER_RETRY_DELAY", 5*time.Second),
			ShutdownTimeout: envDuration("WORKER_SHUTDOWN_TIMEOUT", 10*time.Second),
		},
	}
	if c.Worker.Concurrency <= 0 {
		log.Printf("config: WARNING WORKER_CONCURRENCY=%d is not positive, using 1", c.Worker.Concurrency)
		c.Worker.Concurrency = 1
	}
	return c
}

// weakSecrets are well-known placeholder values that must never sign production tokens.
var weakSecrets = map[string]bool{
	"":                        true,
	"change-me-in-production": true,
	"changeme":                true,
	"secret":                  true,
	"test":                    true,
}

// defaultAdminKey is the value shipped in docker-compose.yml.
const defaultAdminKey = "caldev-admin-secret-key-change-me"

// Validate checks security-critical settings. Fatal problems should stop startup; warnings
// should be logged. Set ALLOW_INSECURE_DEFAULTS=true to downgrade fatal problems to warnings
// (local development only).
func (c *Config) Validate() (fatal, warnings []string) {
	allowInsecure := envBool("ALLOW_INSECURE_DEFAULTS", false)
	add := func(msg string) {
		if allowInsecure {
			warnings = append(warnings, msg+" (allowed by ALLOW_INSECURE_DEFAULTS)")
		} else {
			fatal = append(fatal, msg+" (set ALLOW_INSECURE_DEFAULTS=true to bypass in local development)")
		}
	}

	if weakSecrets[strings.ToLower(strings.TrimSpace(c.Auth.JWTSecret))] {
		add("AUTH_JWT_SECRET is empty or a well-known placeholder: anyone could forge login tokens")
	} else if len(c.Auth.JWTSecret) < 16 {
		warnings = append(warnings, "AUTH_JWT_SECRET is shorter than 16 characters")
	}
	if c.Auth.JWTIssuer == "" {
		warnings = append(warnings, "AUTH_JWT_ISSUER is not set: tokens from any issuer with the same secret are accepted")
	}
	if !c.Auth.RequireExp {
		warnings = append(warnings, "AUTH_REQUIRE_EXP=false: tokens without an expiry are accepted forever")
	}
	if u := strings.ToLower(os.Getenv("CALDEV_PUBLIC_URL")); u == "" {
		warnings = append(warnings, "CALDEV_PUBLIC_URL is not set: links in emails and feeds will point at http://localhost")
	} else if strings.Contains(u, "://localhost") || strings.Contains(u, "://127.0.0.1") || strings.Contains(u, "://[::1]") {
		warnings = append(warnings, "CALDEV_PUBLIC_URL points at localhost: links in emails and feeds will not work for other people")
	}
	if c.Auth.AdminAPIKey == defaultAdminKey {
		warnings = append(warnings, "ADMIN_API_KEY is the default shipped in docker-compose.yml: anyone who has seen the repository knows it")
	}
	return fatal, warnings
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
		warnInvalid(key, v, fallback)
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		warnInvalid(key, v, fallback)
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		warnInvalid(key, v, fallback)
	}
	return fallback
}

// warnInvalid logs an unparseable environment value instead of silently using the default.
func warnInvalid(key, value string, fallback any) {
	log.Printf("config: WARNING invalid value %q for %s, using default %v (durations need a unit, e.g. 30s)", value, key, fallback)
}
