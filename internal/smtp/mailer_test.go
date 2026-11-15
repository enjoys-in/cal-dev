package smtp

import (
	"strings"
	"testing"
)

func TestBuildInvitationMIME(t *testing.T) {
	data := &InvitationData{
		SenderEmail:    "alice@example.com",
		SenderName:     "Alice Smith",
		RecipientEmail: "bob@example.com",
		Subject:        "Invitation: Project Review",
		Method:         "REQUEST",
		Summary:        "Project Review",
		Description:    "Quarterly roadmap review",
		Location:       "Conference Room A",
		StartFormatted: "Monday, Nov 17, 2026 at 10:00 AM UTC",
		EndFormatted:   "11:00 AM UTC",
		Organizer:      "Alice Smith <alice@example.com>",
		RawICS:         "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nSUMMARY:Project Review\r\nEND:VEVENT\r\nEND:VCALENDAR",
		RSVPAcceptURL:  "https://cal.example.com/api/scheduling/rsvp?token=123&action=ACCEPTED",
		RSVPTentURL:    "https://cal.example.com/api/scheduling/rsvp?token=123&action=TENTATIVE",
		RSVPDeclURL:    "https://cal.example.com/api/scheduling/rsvp?token=123&action=DECLINED",
	}

	raw, err := BuildInvitationMIME(data)
	if err != nil {
		t.Fatalf("BuildInvitationMIME failed: %v", err)
	}

	rawStr := string(raw)

	if !strings.Contains(rawStr, `From: "Alice Smith" <alice@example.com>`) {
		t.Errorf("Missing From header: %s", rawStr)
	}
	if !strings.Contains(rawStr, `To: <bob@example.com>`) {
		t.Errorf("Missing To header: %s", rawStr)
	}
	if !strings.Contains(rawStr, `Subject: Invitation: Project Review`) {
		t.Errorf("Missing Subject header: %s", rawStr)
	}
	if !strings.Contains(rawStr, `Content-Type: text/calendar; method=REQUEST`) {
		t.Errorf("Missing text/calendar content type: %s", rawStr)
	}
	if !strings.Contains(rawStr, `Accept:    https://cal.example.com/api/scheduling/rsvp`) {
		t.Errorf("Missing RSVP text link: %s", rawStr)
	}
	if !strings.Contains(rawStr, `action=ACCEPTED`) {
		t.Errorf("Missing HTML RSVP button link: %s", rawStr)
	}
}
