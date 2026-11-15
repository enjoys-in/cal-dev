package scheduling

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidRSVPToken = errors.New("invalid or expired RSVP token")
)

// RSVPTokenPayload holds data decoded from a signed RSVP token.
type RSVPTokenPayload struct {
	TenantID      int
	EventUID      string
	AttendeeEmail string
	ExpiresAt     int64
}

// GenerateRSVPToken generates a signed HMAC token for 1-click web RSVP.
func GenerateRSVPToken(tenantID int, eventUID, email, secret string, ttl time.Duration) string {
	if ttl == 0 {
		ttl = 30 * 24 * time.Hour // 30 days default
	}
	expiresAt := time.Now().Add(ttl).Unix()
	raw := fmt.Sprintf("%d|%s|%s|%d", tenantID, eventUID, email, expiresAt)
	sig := computeHMAC(raw, secret)
	tokenData := fmt.Sprintf("%s|%s", raw, sig)
	return base64.RawURLEncoding.EncodeToString([]byte(tokenData))
}

// ParseRSVPToken validates and extracts the payload from an RSVP token.
func ParseRSVPToken(tokenStr, secret string) (*RSVPTokenPayload, error) {
	bytes, err := base64.RawURLEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, ErrInvalidRSVPToken
	}

	parts := strings.Split(string(bytes), "|")
	if len(parts) != 5 {
		return nil, ErrInvalidRSVPToken
	}

	raw := fmt.Sprintf("%s|%s|%s|%s", parts[0], parts[1], parts[2], parts[3])
	expectedSig := computeHMAC(raw, secret)
	if parts[4] != expectedSig {
		return nil, ErrInvalidRSVPToken
	}

	tenantID, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, ErrInvalidRSVPToken
	}

	expiresAt, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || time.Now().Unix() > expiresAt {
		return nil, ErrInvalidRSVPToken
	}

	return &RSVPTokenPayload{
		TenantID:      tenantID,
		EventUID:      parts[1],
		AttendeeEmail: parts[2],
		ExpiresAt:     expiresAt,
	}, nil
}

func computeHMAC(message, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
