package smtp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Config holds the parameters to connect and authenticate with an SMTP server.
type Config struct {
	Host       string
	Port       int
	Username   string
	Password   string
	FromEmail  string
	FromName   string
	ReplyTo    string
	Encryption string // "none", "ssl", "starttls"
}

// loginAuth implements smtp.Auth for the LOGIN SASL mechanism.
type loginAuth struct {
	username, password string
}

// LoginAuth returns an Auth that implements the LOGIN mechanism.
func LoginAuth(username, password string) smtp.Auth {
	return &loginAuth{username: username, password: password}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte(a.username), nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		prompt := strings.ToLower(string(fromServer))
		if strings.Contains(prompt, "user") {
			return []byte(a.username), nil
		}
		if strings.Contains(prompt, "pass") {
			return []byte(a.password), nil
		}
		return nil, fmt.Errorf("unknown auth prompt: %s", string(fromServer))
	}
	return nil, nil
}

// Dial connects to the SMTP server and handles TLS/STARTTLS + Authentication.
func Dial(cfg *Config) (*smtp.Client, error) {
	if cfg.Host == "" {
		return nil, errors.New("smtp: host is required")
	}
	port := cfg.Port
	if port <= 0 {
		port = 587
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, port)
	enc := strings.ToLower(cfg.Encryption)

	var (
		c   *smtp.Client
		err error
	)

	// Implicit SSL/TLS (typically port 465)
	if enc == "ssl" || port == 465 {
		tlsConfig := &tls.Config{
			ServerName: cfg.Host,
		}
		conn, dialErr := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, tlsConfig)
		if dialErr != nil {
			return nil, fmt.Errorf("tls.Dial to %s: %w", addr, dialErr)
		}
		c, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("smtp.NewClient: %w", err)
		}
	} else {
		// Plain TCP connection with optional STARTTLS
		conn, dialErr := net.DialTimeout("tcp", addr, 10*time.Second)
		if dialErr != nil {
			return nil, fmt.Errorf("net.Dial to %s: %w", addr, dialErr)
		}
		c, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("smtp.NewClient: %w", err)
		}

		// Upgrade to TLS via STARTTLS if supported/configured
		if enc == "starttls" || port == 587 {
			if ok, _ := c.Extension("STARTTLS"); ok {
				tlsConfig := &tls.Config{
					ServerName: cfg.Host,
				}
				if err := c.StartTLS(tlsConfig); err != nil {
					c.Close()
					return nil, fmt.Errorf("StartTLS failed: %w", err)
				}
			} else if enc == "starttls" {
				c.Close()
				return nil, fmt.Errorf("server %s does not support STARTTLS", cfg.Host)
			}
		}
	}

	// Authenticate if credentials are provided
	if cfg.Username != "" && cfg.Password != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := c.Auth(auth); err != nil {
			// Fall back to LOGIN authentication if PLAIN fails
			login := LoginAuth(cfg.Username, cfg.Password)
			if loginErr := c.Auth(login); loginErr != nil {
				c.Close()
				return nil, fmt.Errorf("smtp authentication failed: %w (fallback: %v)", err, loginErr)
			}
		}
	}

	return c, nil
}

// TestConnection attempts to connect, perform TLS handshake, and authenticate against the SMTP server.
func TestConnection(cfg *Config) error {
	client, err := Dial(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Quit()
}

// SendMessage sends an email message to a recipient using the configured SMTP server.
func SendMessage(cfg *Config, to string, rawMIME []byte) error {
	client, err := Dial(cfg)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer client.Close()

	sender := cfg.FromEmail
	if sender == "" {
		sender = cfg.Username
	}

	if err := client.Mail(sender); err != nil {
		return fmt.Errorf("mail from <%s>: %w", sender, err)
	}

	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to <%s>: %w", to, err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}

	if _, err := w.Write(rawMIME); err != nil {
		return fmt.Errorf("write mime: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("data close: %w", err)
	}

	return client.Quit()
}
