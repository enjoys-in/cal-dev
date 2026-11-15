package smtp

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/google/uuid"
)

// InvitationData holds metadata required to render the invitation email.
type InvitationData struct {
	SenderEmail    string
	SenderName     string
	RecipientEmail string
	ReplyTo        string
	Subject        string
	Method         string // "REQUEST", "CANCEL", "REPLY"
	Summary        string
	Description    string
	Location       string
	StartFormatted string
	EndFormatted   string
	Organizer      string
	RawICS         string
	RSVPAcceptURL  string
	RSVPTentURL    string
	RSVPDeclURL    string
}

// BuildInvitationMIME builds an RFC 5322 MIME multipart/alternative message
// containing a text part, an HTML part with 1-click RSVP buttons, and the RFC 5545 .ics payload.
func BuildInvitationMIME(data *InvitationData) ([]byte, error) {
	var buf bytes.Buffer

	// Headers
	// Header values must never contain CR/LF (header injection); non-ASCII is RFC 2047 encoded.
	fromHeader := stripCRLF(data.SenderEmail)
	if data.SenderName != "" {
		fromHeader = (&mail.Address{Name: stripCRLF(data.SenderName), Address: fromHeader}).String()
	}
	messageID := fmt.Sprintf("<%s@caldev>", uuid.New().String())
	dateHeader := time.Now().UTC().Format(time.RFC1123Z)

	buf.WriteString(fmt.Sprintf("From: %s\r\n", fromHeader))
	buf.WriteString(fmt.Sprintf("To: <%s>\r\n", stripCRLF(data.RecipientEmail)))
	if replyTo := stripCRLF(data.ReplyTo); replyTo != "" {
		buf.WriteString(fmt.Sprintf("Reply-To: %s\r\n", replyTo))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", mime.QEncoding.Encode("utf-8", stripCRLF(data.Subject))))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", dateHeader))
	buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", messageID))
	buf.WriteString("MIME-Version: 1.0\r\n")

	// Multipart Alternative
	writer := multipart.NewWriter(&buf)
	buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", writer.Boundary()))

	// 1. Text Plain Part
	textHeader := make(textproto.MIMEHeader)
	textHeader.Set("Content-Type", "text/plain; charset=UTF-8")
	textHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	textPart, err := writer.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	if err := writeQuotedPrintable(textPart, generatePlainTextBody(data)); err != nil {
		return nil, err
	}

	// 2. HTML Part
	htmlHeader := make(textproto.MIMEHeader)
	htmlHeader.Set("Content-Type", "text/html; charset=UTF-8")
	htmlHeader.Set("Content-Transfer-Encoding", "quoted-printable")
	htmlPart, err := writer.CreatePart(htmlHeader)
	if err != nil {
		return nil, err
	}
	if err := writeQuotedPrintable(htmlPart, generateHTMLBody(data)); err != nil {
		return nil, err
	}

	// 3. Calendar .ics Part
	method := data.Method
	if method == "" {
		method = "REQUEST"
	}
	icsHeader := make(textproto.MIMEHeader)
	icsHeader.Set("Content-Type", fmt.Sprintf("text/calendar; method=%s; charset=UTF-8; name=\"invite.ics\"", method))
	icsHeader.Set("Content-Transfer-Encoding", "7bit")
	icsHeader.Set("Content-Disposition", "inline; filename=\"invite.ics\"")
	icsPart, err := writer.CreatePart(icsHeader)
	if err != nil {
		return nil, err
	}
	// Ensure CRLF endings in raw ICS
	icsNormalized := strings.ReplaceAll(data.RawICS, "\r\n", "\n")
	icsNormalized = strings.ReplaceAll(icsNormalized, "\n", "\r\n")
	if _, err := icsPart.Write([]byte(icsNormalized)); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func generatePlainTextBody(d *InvitationData) string {
	var sb strings.Builder
	switch d.Method {
	case "CANCEL":
		sb.WriteString("EVENT CANCELLATION NOTICE\n\n")
		sb.WriteString(fmt.Sprintf("The event %q has been canceled.\n\n", d.Summary))
	case "REPLY":
		sb.WriteString("CALENDAR RSVP UPDATE\n\n")
		sb.WriteString(fmt.Sprintf("Response received for: %s\n\n", d.Summary))
	default:
		sb.WriteString("CALENDAR INVITATION\n\n")
		sb.WriteString(fmt.Sprintf("You are invited to: %s\n\n", d.Summary))
	}

	if d.StartFormatted != "" {
		sb.WriteString(fmt.Sprintf("When: %s", d.StartFormatted))
		if d.EndFormatted != "" {
			sb.WriteString(fmt.Sprintf(" - %s", d.EndFormatted))
		}
		sb.WriteString("\n")
	}
	if d.Location != "" {
		sb.WriteString(fmt.Sprintf("Where: %s\n", d.Location))
	}
	if d.Organizer != "" {
		sb.WriteString(fmt.Sprintf("Organizer: %s\n", d.Organizer))
	}
	if d.Description != "" {
		sb.WriteString(fmt.Sprintf("\nDetails:\n%s\n", d.Description))
	}

	if d.Method == "REQUEST" && d.RSVPAcceptURL != "" {
		sb.WriteString("\nRSVP via web:\n")
		sb.WriteString(fmt.Sprintf("  Accept:    %s\n", d.RSVPAcceptURL))
		if d.RSVPTentURL != "" {
			sb.WriteString(fmt.Sprintf("  Tentative: %s\n", d.RSVPTentURL))
		}
		sb.WriteString(fmt.Sprintf("  Decline:   %s\n", d.RSVPDeclURL))
	}

	return sb.String()
}

func generateHTMLBody(d *InvitationData) string {
	title := "Calendar Invitation"
	badgeColor := "#0078D4"
	if d.Method == "CANCEL" {
		title = "Event Canceled"
		badgeColor = "#D13438"
	} else if d.Method == "REPLY" {
		title = "RSVP Update"
		badgeColor = "#6B7280"
	}

	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><head><meta charset=\"UTF-8\">")
	sb.WriteString("<style>")
	sb.WriteString("body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif; background-color: #f4f6f8; margin: 0; padding: 24px; color: #1e293b; }")
	sb.WriteString(".card { max-width: 560px; margin: 0 auto; background: #ffffff; border-radius: 12px; border: 1px solid #e2e8f0; overflow: hidden; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.05); }")
	sb.WriteString(".header { padding: 20px 24px; border-bottom: 1px solid #f1f5f9; }")
	sb.WriteString(fmt.Sprintf(".badge { display: inline-block; font-size: 11px; font-weight: 700; text-transform: uppercase; color: #fff; background: %s; padding: 3px 8px; border-radius: 6px; letter-spacing: 0.5px; }", badgeColor))
	sb.WriteString(".title { font-size: 20px; font-weight: 700; margin: 10px 0 0 0; color: #0f172a; }")
	sb.WriteString(".body { padding: 24px; }")
	sb.WriteString(".meta-row { display: flex; margin-bottom: 12px; font-size: 14px; line-height: 1.5; }")
	sb.WriteString(".meta-label { font-weight: 600; width: 85px; color: #64748b; flex-shrink: 0; }")
	sb.WriteString(".meta-val { color: #1e293b; }")
	sb.WriteString(".desc { margin-top: 18px; padding-top: 18px; border-top: 1px solid #f1f5f9; font-size: 14px; color: #334155; white-space: pre-wrap; }")
	sb.WriteString(".rsvp-section { padding: 20px 24px; background: #f8fafc; border-top: 1px solid #e2e8f0; text-align: center; }")
	sb.WriteString(".rsvp-title { font-size: 13px; font-weight: 600; color: #64748b; margin-bottom: 12px; }")
	sb.WriteString(".btn { display: inline-block; padding: 8px 18px; margin: 0 4px; font-size: 13px; font-weight: 600; text-decoration: none; border-radius: 6px; transition: opacity 0.2s; }")
	sb.WriteString(".btn-accept { background: #10B981; color: #ffffff; }")
	sb.WriteString(".btn-tentative { background: #F59E0B; color: #ffffff; }")
	sb.WriteString(".btn-decline { background: #EF4444; color: #ffffff; }")
	sb.WriteString("</style></head><body>")

	sb.WriteString("<div class=\"card\">")
	sb.WriteString("<div class=\"header\">")
	sb.WriteString(fmt.Sprintf("<span class=\"badge\">%s</span>", title))
	sb.WriteString(fmt.Sprintf("<h2 class=\"title\">%s</h2>", escapeHTML(d.Summary)))
	sb.WriteString("</div>")

	sb.WriteString("<div class=\"body\">")
	if d.StartFormatted != "" {
		sb.WriteString("<div class=\"meta-row\"><span class=\"meta-label\">When:</span><span class=\"meta-val\">")
		sb.WriteString(escapeHTML(d.StartFormatted))
		if d.EndFormatted != "" {
			sb.WriteString(fmt.Sprintf(" – %s", escapeHTML(d.EndFormatted)))
		}
		sb.WriteString("</span></div>")
	}
	if d.Location != "" {
		sb.WriteString(fmt.Sprintf("<div class=\"meta-row\"><span class=\"meta-label\">Where:</span><span class=\"meta-val\">%s</span></div>", escapeHTML(d.Location)))
	}
	if d.Organizer != "" {
		sb.WriteString(fmt.Sprintf("<div class=\"meta-row\"><span class=\"meta-label\">Organizer:</span><span class=\"meta-val\">%s</span></div>", escapeHTML(d.Organizer)))
	}
	if d.Description != "" {
		sb.WriteString(fmt.Sprintf("<div class=\"desc\">%s</div>", escapeHTML(d.Description)))
	}
	sb.WriteString("</div>")

	if d.Method == "REQUEST" && d.RSVPAcceptURL != "" {
		sb.WriteString("<div class=\"rsvp-section\">")
		sb.WriteString("<div class=\"rsvp-title\">Going? Respond to this invitation:</div>")
		sb.WriteString(fmt.Sprintf("<a href=\"%s\" class=\"btn btn-accept\">Accept</a>", escapeHTML(d.RSVPAcceptURL)))
		if d.RSVPTentURL != "" {
			sb.WriteString(fmt.Sprintf("<a href=\"%s\" class=\"btn btn-tentative\">Tentative</a>", escapeHTML(d.RSVPTentURL)))
		}
		sb.WriteString(fmt.Sprintf("<a href=\"%s\" class=\"btn btn-decline\">Decline</a>", escapeHTML(d.RSVPDeclURL)))
		sb.WriteString("</div>")
	}

	sb.WriteString("</div></body></html>")
	return sb.String()
}

var escapeHTML = html.EscapeString

var crlfStripper = strings.NewReplacer("\r", "", "\n", "")

// stripCRLF removes CR and LF so a value cannot start a new header line.
func stripCRLF(s string) string {
	return crlfStripper.Replace(s)
}

// writeQuotedPrintable writes body to w as quoted-printable (UTF-8 safe, lines wrapped at 76 chars).
func writeQuotedPrintable(w io.Writer, body string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(body)); err != nil {
		return err
	}
	return qp.Close()
}
