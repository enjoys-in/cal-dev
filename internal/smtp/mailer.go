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

	// Shown in the HTML part
	RecipientName      string // guest's display name, for the greeting
	DayOfMonth         string // "18": the start day in the event's zone, for the date circle
	IsUpdate           bool   // REQUEST that changes an earlier invitation
	MeetingURL         string // video meeting link, shown as "Join meeting"
	GoogleCalendarURL  string // "Add to calendar" links (REQUEST only)
	OutlookCalendarURL string
	ICSURL             string // .ics download, used for Apple and "Other"
}

// EmailIconBaseURL hosts the PNG icons of the "Add to calendar" buttons (Gmail shows no SVG).
const EmailIconBaseURL = "https://airsend.in/email-icons/"

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

// Email HTML: tables and inline styles only, so Gmail and Outlook render it the same way.
const (
	mailFont       = "-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif"
	mailBlue       = "#2F8CF0"
	mailInk        = "#1A1F36"
	mailMuted      = "#7A8199"
	mailPillStyle  = "display:inline-block;margin:0 8px 10px 0;padding:10px 18px;border:2px solid #E3E5EB;border-radius:14px;background:#ffffff;color:#1F2937;font-size:16px;font-weight:700;line-height:20px;text-decoration:none;white-space:nowrap"
	mailIconStyle  = "width:20px;height:20px;border:0;vertical-align:-4px;margin-right:10px"
	mailDotPattern = "<span style=\"display:inline-block;width:10px;height:10px;border-radius:5px;background:%s;margin-right:8px\"></span>"
)

// mailPill is an outlined, rounded button: an optional icon (or colored dot) and a label.
func mailPill(href, label, lead string) string {
	return fmt.Sprintf("<a href=\"%s\" target=\"_blank\" style=\"%s\">%s%s</a>", escapeHTML(href), mailPillStyle, lead, escapeHTML(label))
}

func mailIcon(name, alt string) string {
	return fmt.Sprintf("<img src=\"%s%s.png\" width=\"20\" height=\"20\" alt=\"%s\" style=\"%s\">", EmailIconBaseURL, name, alt, mailIconStyle)
}

// mailGreetingName: the guest's first name, else the part of the address before "@".
func mailGreetingName(name, email string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	local, _, _ := strings.Cut(email, "@")
	return local
}

func generateHTMLBody(d *InvitationData) string {
	cancelled := d.Method == "CANCEL"
	who := mailGreetingName(d.RecipientName, d.RecipientEmail)
	greeting, accent := fmt.Sprintf("Hello %s! You're invited, add this event to your calendar", who), mailBlue
	switch {
	case cancelled:
		greeting, accent = fmt.Sprintf("Hello %s! This event has been cancelled", who), "#E5484D"
	case d.Method == "REPLY":
		greeting = "A guest replied to your invitation"
	case d.IsUpdate:
		greeting = fmt.Sprintf("Hello %s! This event has been updated", who)
	}

	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><head><meta charset=\"UTF-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"></head>")
	sb.WriteString(fmt.Sprintf("<body style=\"margin:0;padding:0;background:#ffffff;font-family:%s;color:%s\">", mailFont, mailInk))
	sb.WriteString("<table role=\"presentation\" width=\"100%\" cellpadding=\"0\" cellspacing=\"0\" border=\"0\"><tr><td align=\"center\" style=\"padding:32px 16px\">")
	sb.WriteString("<table role=\"presentation\" width=\"100%\" cellpadding=\"0\" cellspacing=\"0\" border=\"0\" style=\"max-width:600px\"><tr><td align=\"left\">")

	sb.WriteString(fmt.Sprintf("<p style=\"margin:0 0 32px;font-size:18px;font-weight:700;color:%s\">%s</p>", accent, escapeHTML(greeting)))

	// Date circle | title, description, when, where
	sb.WriteString("<table role=\"presentation\" width=\"100%\" cellpadding=\"0\" cellspacing=\"0\" border=\"0\"><tr>")
	if d.DayOfMonth != "" {
		circle := "#3B8EEA"
		if cancelled {
			circle = "#A0A6B8"
		}
		sb.WriteString("<td width=\"96\" valign=\"top\" style=\"padding:0 28px 0 0\">")
		sb.WriteString(fmt.Sprintf("<div style=\"width:96px;height:96px;border-radius:48px;background:%s;color:#ffffff;font-size:44px;font-weight:800;line-height:96px;text-align:center\">%s</div>", circle, escapeHTML(d.DayOfMonth)))
		sb.WriteString("</td>")
	}
	sb.WriteString("<td valign=\"top\">")
	titleDeco := ""
	if cancelled {
		titleDeco = "text-decoration:line-through;"
	}
	sb.WriteString(fmt.Sprintf("<p style=\"margin:4px 0 6px;font-size:26px;font-weight:700;line-height:32px;color:%s;%s\">%s</p>", mailInk, titleDeco, escapeHTML(d.Summary)))
	if d.Description != "" {
		sb.WriteString(fmt.Sprintf("<p style=\"margin:0;font-size:18px;line-height:26px;color:%s;white-space:pre-wrap\">%s</p>", mailMuted, escapeHTML(d.Description)))
	}
	if d.StartFormatted != "" {
		when := d.StartFormatted
		if d.EndFormatted != "" {
			when += " – " + d.EndFormatted
		}
		sb.WriteString(fmt.Sprintf("<p style=\"margin:18px 0 0;font-size:16px;font-weight:700;color:%s\">%s</p>", mailMuted, escapeHTML(when)))
	}
	if d.Location != "" {
		sb.WriteString(fmt.Sprintf("<p style=\"margin:14px 0 0;font-size:16px;color:%s\">%s</p>", mailMuted, escapeHTML(d.Location)))
	}
	if d.Organizer != "" {
		sb.WriteString(fmt.Sprintf("<p style=\"margin:8px 0 0;font-size:14px;color:%s\">Organizer: %s</p>", mailMuted, escapeHTML(strings.TrimPrefix(strings.TrimPrefix(d.Organizer, "mailto:"), "MAILTO:"))))
	}
	if d.MeetingURL != "" && !cancelled {
		sb.WriteString(fmt.Sprintf("<p style=\"margin:14px 0 0;font-size:16px;font-weight:700\"><a href=\"%s\" target=\"_blank\" style=\"color:%s;text-decoration:none\">Join meeting &rarr;</a></p>", escapeHTML(d.MeetingURL), mailBlue))
	}
	sb.WriteString("</td></tr></table>")

	if d.Method == "REQUEST" && d.RSVPAcceptURL != "" {
		sb.WriteString(fmt.Sprintf("<p style=\"margin:36px 0 12px;font-size:15px;font-weight:700;color:%s\">Going?</p><div>", mailInk))
		sb.WriteString(mailPill(d.RSVPAcceptURL, "Yes", fmt.Sprintf(mailDotPattern, "#10B981")))
		if d.RSVPTentURL != "" {
			sb.WriteString(mailPill(d.RSVPTentURL, "Maybe", fmt.Sprintf(mailDotPattern, "#F59E0B")))
		}
		sb.WriteString(mailPill(d.RSVPDeclURL, "No", fmt.Sprintf(mailDotPattern, "#EF4444")))
		sb.WriteString("</div>")
	}

	if d.Method == "REQUEST" && (d.GoogleCalendarURL != "" || d.OutlookCalendarURL != "" || d.ICSURL != "") {
		sb.WriteString(fmt.Sprintf("<p style=\"margin:24px 0 12px;font-size:15px;font-weight:700;color:%s\">Add to calendar</p><div>", mailInk))
		if d.GoogleCalendarURL != "" {
			sb.WriteString(mailPill(d.GoogleCalendarURL, "Google", mailIcon("google", "")))
		}
		if d.OutlookCalendarURL != "" {
			sb.WriteString(mailPill(d.OutlookCalendarURL, "Outlook", mailIcon("outlook", "")))
		}
		if d.ICSURL != "" {
			sb.WriteString(mailPill(d.ICSURL, "Apple", mailIcon("apple", "")))
			sb.WriteString(mailPill(d.ICSURL, "Other", mailIcon("plus", "")))
		}
		sb.WriteString("</div>")
	}

	sb.WriteString("<p style=\"margin:36px 0 0;font-size:12px;color:#A0A6B8\">Sent with Airsend Calendar</p>")
	sb.WriteString("</td></tr></table></td></tr></table></body></html>")
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
