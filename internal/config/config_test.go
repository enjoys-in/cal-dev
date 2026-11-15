package config

import "testing"

func cfg(secret string) *Config {
	return &Config{Auth: AuthConfig{JWTSecret: secret, JWTIssuer: "ENJOYS", RequireExp: true}}
}

func TestValidateRejectsPlaceholderSecrets(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_DEFAULTS", "")
	for _, s := range []string{"", "change-me-in-production", "CHANGE-ME-IN-PRODUCTION", "test", "secret", " changeme "} {
		if fatal, _ := cfg(s).Validate(); len(fatal) == 0 {
			t.Errorf("secret %q should be fatal", s)
		}
	}
}

func TestValidateAcceptsRealSecret(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_DEFAULTS", "")
	if fatal, _ := cfg("unit-test-secret-not-a-real-one-0123456789").Validate(); len(fatal) != 0 {
		t.Fatalf("a real secret must pass: %v", fatal)
	}
}

func TestAllowInsecureDowngradesToWarning(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_DEFAULTS", "true")
	fatal, warnings := cfg("change-me-in-production").Validate()
	if len(fatal) != 0 || len(warnings) == 0 {
		t.Fatalf("want warning only, got fatal=%v warnings=%v", fatal, warnings)
	}
}

func TestWarnsAboutShippedAdminKey(t *testing.T) {
	t.Setenv("ALLOW_INSECURE_DEFAULTS", "")
	c := cfg("unit-test-secret-not-a-real-one-0123456789")
	c.Auth.AdminAPIKey = defaultAdminKey
	if _, warnings := c.Validate(); len(warnings) == 0 {
		t.Fatal("default admin key should produce a warning")
	}
}

func TestDSNQuotesValues(t *testing.T) {
	if got, want := dsnQuote(`p w'd\x`), `'p w\'d\\x'`; got != want {
		t.Fatalf("dsnQuote = %s, want %s", got, want)
	}
	d := DatabaseConfig{Host: "h", Port: 5432, User: "u", Password: "a b sslmode=disable", DBName: "db", SSLMode: "require", Schema: "caldev"}
	// Schema is not applied to the DSN (see DatabaseConfig.DSN).
	want := `host='h' port=5432 user='u' password='a b sslmode=disable' dbname='db' sslmode='require'`
	if got := d.DSN(); got != want {
		t.Fatalf("DSN = %s", got)
	}
}
