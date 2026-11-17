package smtp

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// decodeParts parses the MIME message and returns its headers and decoded part bodies keyed by media type.
func decodeParts(t *testing.T, raw []byte) (mail.Header, map[string]string) {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("ParseMediaType: %v", err)
	}
	parts := map[string]string{}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart() // transparently decodes quoted-printable
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		body, _ := io.ReadAll(p)
		mediaType, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts[mediaType] = string(body)
	}
	return msg.Header, parts
}

func TestBuildInvitationMIME(t *testing.T) {
	data := &InvitationData{
		SenderEmail:    "alice@example.com",
		SenderName:     "Alice Smith",
		RecipientEmail: "bob@example.com",
		ReplyTo:        "replies@example.com",
		Subject:        "Invitation: Project Review",
		Method:         "REQUEST",
		Summary:        "Project Review",
		Description:    "Quarterly roadmap review",
		Location:       "Conference Room A",
		StartFormatted: "Monday, Nov 17, 2026 at 10:00 AM UTC",
		EndFormatted:   "11:00 AM UTC",
		Organizer:      "Alice Smith <alice@example.com>",
		RawICS:         "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nSUMMARY:Project Review\r\nEND:VEVENT\r\nEND:VCALENDAR",
		RSVPAcceptURL:  "https://cal.example.com/api/public/rsvp?token=123&action=ACCEPTED",
		RSVPTentURL:    "https://cal.example.com/api/public/rsvp?token=123&action=TENTATIVE",
		RSVPDeclURL:    "https://cal.example.com/api/public/rsvp?token=123&action=DECLINED",
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
	if !strings.Contains(rawStr, "Reply-To: replies@example.com\r\n") {
		t.Errorf("Missing Reply-To header: %s", rawStr)
	}
	if !strings.Contains(rawStr, `Subject: Invitation: Project Review`) {
		t.Errorf("Missing Subject header: %s", rawStr)
	}
	if !strings.Contains(rawStr, `Content-Type: text/calendar; method=REQUEST`) {
		t.Errorf("Missing text/calendar content type: %s", rawStr)
	}

	// RFC 5322: no line may exceed 998 characters.
	for i, line := range strings.Split(rawStr, "\r\n") {
		if len(line) > 998 {
			t.Errorf("line %d is %d chars long", i, len(line))
		}
	}

	_, parts := decodeParts(t, raw)
	if !strings.Contains(parts["text/plain"], `Accept:    https://cal.example.com/api/public/rsvp`) {
		t.Errorf("Missing RSVP text link: %s", parts["text/plain"])
	}
	if !strings.Contains(parts["text/html"], `action=ACCEPTED`) {
		t.Errorf("Missing HTML RSVP button link: %s", parts["text/html"])
	}
	if !strings.Contains(parts["text/html"], "–") {
		t.Errorf("UTF-8 en dash not preserved in HTML part")
	}
}

func TestBuildInvitationMIMEHeaderInjectionAndEncoding(t *testing.T) {
	data := &InvitationData{
		SenderEmail:    "alice@example.com",
		SenderName:     "Zoë Müller",
		RecipientEmail: "bob@example.com\r\nBcc: evil@example.com",
		Subject:        "Réunion\r\nBcc: evil@example.com",
		Method:         "REPLY",
		Summary:        "Réunion",
		RSVPAcceptURL:  "https://cal.example.com/api/public/rsvp?token=1&action=ACCEPTED",
		RSVPDeclURL:    "https://cal.example.com/api/public/rsvp?token=1&action=DECLINED",
	}
	raw, err := BuildInvitationMIME(data)
	if err != nil {
		t.Fatalf("BuildInvitationMIME failed: %v", err)
	}
	header, parts := decodeParts(t, raw)

	if strings.Contains(string(raw), "\r\nBcc:") {
		t.Fatalf("header injection: %s", raw)
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(header.Get("Subject"))
	if err != nil || subject != "RéunionBcc: evil@example.com" {
		t.Errorf("unexpected subject %q (%v)", subject, err)
	}
	from, err := mail.ParseAddress(header.Get("From"))
	if err != nil || from.Name != "Zoë Müller" {
		t.Errorf("unexpected From %q (%v)", header.Get("From"), err)
	}

	// REPLY emails go to the organizer: no RSVP buttons, own badge.
	if strings.Contains(parts["text/html"], "action=ACCEPTED") || strings.Contains(parts["text/plain"], "RSVP via web") {
		t.Errorf("REPLY email must not contain RSVP links")
	}
	if !strings.Contains(parts["text/html"], "replied to your invitation") {
		t.Errorf("REPLY email should say a guest replied")
	}
}
