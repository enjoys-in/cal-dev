package scheduling

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidRSVPToken = errors.New("invalid or expired RSVP token")
)

// RSVPTokenPayload holds data decoded from a signed RSVP token.
type RSVPTokenPayload struct {
	TenantID      int    `json:"t"`
	EventUID      string `json:"u"`
	AttendeeEmail string `json:"e"`
	ExpiresAt     int64  `json:"x"`
}

// GenerateRSVPToken generates a signed HMAC token for 1-click web RSVP.
// Format: base64url(JSON payload) + "." + base64url(HMAC-SHA256 of the encoded payload).
func GenerateRSVPToken(tenantID int, eventUID, email, secret string, ttl time.Duration) string {
	if ttl == 0 {
		ttl = 30 * 24 * time.Hour // 30 days default
	}
	raw, _ := json.Marshal(RSVPTokenPayload{
		TenantID:      tenantID,
		EventUID:      eventUID,
		AttendeeEmail: email,
		ExpiresAt:     time.Now().Add(ttl).Unix(),
	})
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + base64.RawURLEncoding.EncodeToString(computeHMAC(body, secret))
}

// ParseRSVPToken validates and extracts the payload from an RSVP token.
func ParseRSVPToken(tokenStr, secret string) (*RSVPTokenPayload, error) {
	body, sigStr, ok := strings.Cut(tokenStr, ".")
	if !ok {
		return nil, ErrInvalidRSVPToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil || !hmac.Equal(sig, computeHMAC(body, secret)) {
		return nil, ErrInvalidRSVPToken
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalidRSVPToken
	}
	var payload RSVPTokenPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, ErrInvalidRSVPToken
	}
	if time.Now().Unix() > payload.ExpiresAt {
		return nil, ErrInvalidRSVPToken
	}
	return &payload, nil
}

// computeHMAC signs message with a key dedicated to RSVP tokens (derived from secret,
// so the raw JWT secret is never used directly as the HMAC key).
func computeHMAC(message, secret string) []byte {
	key := sha256.Sum256([]byte("caldev-rsvp:" + secret))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(message))
	return mac.Sum(nil)
}
