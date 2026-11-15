package scheduling

import (
	"testing"
	"time"
)

func TestRSVPTokenRoundTrip(t *testing.T) {
	secret := "test-hmac-secret-xyz"
	token := GenerateRSVPToken(10, "event-uid-99", "attendee@example.com", secret, 1*time.Hour)

	payload, err := ParseRSVPToken(token, secret)
	if err != nil {
		t.Fatalf("ParseRSVPToken failed: %v", err)
	}

	if payload.TenantID != 10 {
		t.Errorf("Expected TenantID 10, got %d", payload.TenantID)
	}
	if payload.EventUID != "event-uid-99" {
		t.Errorf("Expected EventUID 'event-uid-99', got %s", payload.EventUID)
	}
	if payload.AttendeeEmail != "attendee@example.com" {
		t.Errorf("Expected AttendeeEmail 'attendee@example.com', got %s", payload.AttendeeEmail)
	}
}

func TestRSVPTokenExpired(t *testing.T) {
	secret := "test-hmac-secret-xyz"
	token := GenerateRSVPToken(10, "event-uid-99", "attendee@example.com", secret, -1*time.Minute)

	_, err := ParseRSVPToken(token, secret)
	if err == nil {
		t.Fatal("Expected error for expired token, but got nil")
	}
}

func TestRSVPTokenTampered(t *testing.T) {
	secret := "test-hmac-secret-xyz"
	token := GenerateRSVPToken(10, "event-uid-99", "attendee@example.com", secret, 1*time.Hour)

	_, err := ParseRSVPToken(token+"tampered", secret)
	if err == nil {
		t.Fatal("Expected error for tampered token, but got nil")
	}
}
