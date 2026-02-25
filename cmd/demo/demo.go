// Package main provides a standalone demo that exercises CalDev's core
// components without requiring PostgreSQL or Valkey.
//
// Run:
//
//	go run cmd/demo/demo.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/enjoys-in/cal-dev/internal/auth"
	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/ics"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/xmlutil"
	"github.com/google/uuid"
)

func main() {
	fmt.Println("╔══════════════════════════════════════════════╗")
	fmt.Println("║         CalDev — Component Demo              ║")
	fmt.Println("║   CalDAV + JMAP Server (no DB required)      ║")
	fmt.Println("╚══════════════════════════════════════════════╝")
	fmt.Println()

	passed := 0
	failed := 0

	// ============================================================
	// 1. Configuration
	// ============================================================
	section("1. Configuration Loading")

	cfg := config.Load()
	fmt.Printf("  Server address : %s\n", cfg.Server.Addr())
	fmt.Printf("  DB schema      : %s\n", cfg.Database.Schema)
	fmt.Printf("  JWT issuer     : %q\n", cfg.Auth.JWTIssuer)
	fmt.Printf("  Worker threads : %d\n", cfg.Worker.Concurrency)
	check("Config loads without panic", true, &passed, &failed)

	// ============================================================
	// 2. JWT Token Generation & Validation
	// ============================================================
	section("2. JWT Token (Shared Secret + Issuer)")

	// Simulate shared secret/issuer from env
	authCfg := &config.AuthConfig{
		JWTSecret:   "demo-shared-secret-32bytes-long!!",
		JWTIssuer:   "airsend-auth",
		TokenExpiry: 1 * time.Hour,
	}
	authSvc := auth.NewService(nil, authCfg) // nil store — token ops don't need DB

	userID := uuid.New().String()
	tenantID := uuid.New().String()
	email := "demo@airsend.example"

	token, err := authSvc.GenerateToken(userID, tenantID, email)
	check("Generate JWT token", err == nil, &passed, &failed)
	fmt.Printf("  Token (first 50 chars): %s...\n", token[:min(50, len(token))])

	claims, err := authSvc.ValidateToken(token)
	check("Validate JWT token", err == nil && claims != nil, &passed, &failed)
	if claims != nil {
		fmt.Printf("  Claims.UserID   = %s\n", claims.UserID)
		fmt.Printf("  Claims.TenantID = %s\n", claims.TenantID)
		fmt.Printf("  Claims.Email    = %s\n", claims.Email)
		fmt.Printf("  Claims.Issuer   = %s\n", claims.Issuer)
		check("Issuer matches", claims.Issuer == "airsend-auth", &passed, &failed)
		check("UserID matches", claims.UserID == userID, &passed, &failed)
		check("Email matches", claims.Email == email, &passed, &failed)
	}

	// Reject wrong secret
	badCfg := &config.AuthConfig{JWTSecret: "wrong-secret-entirely!!!!!!!!!", JWTIssuer: "airsend-auth", TokenExpiry: time.Hour}
	badSvc := auth.NewService(nil, badCfg)
	_, err = badSvc.ValidateToken(token)
	check("Reject token with wrong secret", err != nil, &passed, &failed)

	// Reject wrong issuer
	wrongIssuerCfg := &config.AuthConfig{JWTSecret: "demo-shared-secret-32bytes-long!!", JWTIssuer: "wrong-issuer", TokenExpiry: time.Hour}
	wrongIssuerSvc := auth.NewService(nil, wrongIssuerCfg)
	_, err = wrongIssuerSvc.ValidateToken(token)
	check("Reject token with wrong issuer", err != nil, &passed, &failed)

	// ============================================================
	// 3. Password Hashing (App Passwords)
	// ============================================================
	section("3. App Password Hashing (bcrypt)")

	appPw := "my-caldav-app-password"
	hash, err := auth.HashPassword(appPw)
	check("Hash password", err == nil, &passed, &failed)
	fmt.Printf("  Hash (first 40 chars): %s...\n", hash[:min(40, len(hash))])

	check("Verify correct password", auth.CheckPassword(appPw, hash), &passed, &failed)
	check("Reject wrong password", !auth.CheckPassword("wrong-pw", hash), &passed, &failed)

	// ============================================================
	// 4. Basic Auth Parsing
	// ============================================================
	section("4. Basic Auth Parsing")

	basicResult, err := auth.ParseBasicAuth("Basic ZGVtb0BhaXJzZW5kLmV4YW1wbGU6bXktYXBwLXBhc3N3b3Jk") // demo@airsend.example:my-app-password
	check("Parse Basic Auth header", err == nil && basicResult.Valid, &passed, &failed)
	if basicResult != nil && basicResult.Valid {
		fmt.Printf("  Email    : %s\n", basicResult.Email)
		fmt.Printf("  Password : %s\n", basicResult.Password)
		check("Email parsed correctly", basicResult.Email == "demo@airsend.example", &passed, &failed)
	}

	nonBasic, err := auth.ParseBasicAuth("Bearer some-token")
	check("Reject non-Basic header", err == nil && !nonBasic.Valid, &passed, &failed)

	// ============================================================
	// 5. ICS Parser & Generator
	// ============================================================
	section("5. ICS Parser & Generator (RFC 5545)")

	sampleICS := `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//CalDev//Demo//EN
BEGIN:VEVENT
UID:demo-event-001@airsend.example
SUMMARY:Team Standup
DESCRIPTION:Daily standup meeting
LOCATION:Conference Room A
DTSTART:20260301T090000Z
DTEND:20260301T093000Z
STATUS:CONFIRMED
ORGANIZER;CN=Alice:mailto:alice@airsend.example
ATTENDEE;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;CN=Bob:mailto:bob@airsend.example
ATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=NEEDS-ACTION;CN=Charlie:mailto:charlie@airsend.example
RRULE:FREQ=DAILY;COUNT=30
SEQUENCE:0
END:VEVENT
END:VCALENDAR`

	demoCalID := uuid.New().String()
	event, err := ics.ParseICS(sampleICS, tenantID, userID, demoCalID)
	check("Parse ICS", err == nil && event != nil, &passed, &failed)
	if event != nil {
		fmt.Printf("  UID       : %s\n", event.UID)
		fmt.Printf("  Summary   : %s\n", event.Summary)
		fmt.Printf("  Location  : %s\n", event.Location)
		fmt.Printf("  Start     : %s\n", event.DTStart.Format(time.RFC3339))
		fmt.Printf("  Organizer : %s\n", event.Organizer)
		fmt.Printf("  Recurrence: %s\n", event.RecurrenceRule)

		check("UID parsed", event.UID == "demo-event-001@airsend.example", &passed, &failed)
		check("Summary parsed", event.Summary == "Team Standup", &passed, &failed)
		check("Location parsed", event.Location == "Conference Room A", &passed, &failed)
		check("Has recurrence", event.RecurrenceRule != "", &passed, &failed)

		// Parse attendees
		var attendees []models.Attendee
		if err := json.Unmarshal(event.Attendees, &attendees); err == nil {
			fmt.Printf("  Attendees : %d\n", len(attendees))
			for _, a := range attendees {
				fmt.Printf("    - %s <%s> [%s]\n", a.DisplayName, a.Email, a.Status)
			}
			check("Attendees parsed", len(attendees) == 2, &passed, &failed)
		}

		// Round-trip: generate ICS from parsed event
		generated := ics.GenerateICS(event)
		check("Generate ICS from event", len(generated) > 100, &passed, &failed)
		check("Generated ICS has VCALENDAR", strings.Contains(generated, "BEGIN:VCALENDAR"), &passed, &failed)
		check("Generated ICS has UID", strings.Contains(generated, event.UID), &passed, &failed)
		check("Generated ICS has SUMMARY", strings.Contains(generated, "Team Standup"), &passed, &failed)

		fmt.Printf("\n  --- Generated ICS (first 500 chars) ---\n")
		printTruncated(generated, 500)

		// ETag generation
		etag := ics.GenerateETag(event.UID + "0")
		check("ETag generated", len(etag) > 0, &passed, &failed)
		fmt.Printf("  ETag: %s\n", etag)
	}

	// Validate ICS
	err = ics.ValidateICS(sampleICS)
	check("Validate valid ICS", err == nil, &passed, &failed)

	err = ics.ValidateICS("not valid ics data")
	check("Reject invalid ICS", err != nil, &passed, &failed)

	// ============================================================
	// 6. Scheduling ICS Generation
	// ============================================================
	section("6. Scheduling ICS (RFC 6638 iMIP)")

	if event != nil {
		inviteICS := ics.GenerateSchedulingICS(event, "REQUEST")
		check("Generate REQUEST ICS", strings.Contains(inviteICS, "METHOD:REQUEST"), &passed, &failed)
		fmt.Printf("  Scheduling ICS (first 300 chars):\n")
		printTruncated(inviteICS, 300)

		cancelICS := ics.GenerateSchedulingICS(event, "CANCEL")
		check("Generate CANCEL ICS", strings.Contains(cancelICS, "METHOD:CANCEL"), &passed, &failed)
	}

	// ============================================================
	// 7. XML Utilities (WebDAV)
	// ============================================================
	section("7. WebDAV XML Parsing")

	propfindXML := `<?xml version="1.0" encoding="utf-8" ?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">
  <D:prop>
    <D:displayname/>
    <D:resourcetype/>
    <C:calendar-description/>
    <C:supported-calendar-component-set/>
  </D:prop>
</D:propfind>`

	pfReq, err := xmlutil.ParsePropfindRequest(strings.NewReader(propfindXML))
	check("Parse PROPFIND XML", err == nil && pfReq != nil, &passed, &failed)
	if pfReq != nil && pfReq.Prop != nil {
		fmt.Printf("  Requested properties: %d\n", len(pfReq.Prop.Props))
		for _, p := range pfReq.Prop.Props {
			if p.Space != "" {
				fmt.Printf("    - {%s}%s\n", p.Space, p.Local)
			} else {
				fmt.Printf("    - %s\n", p.Local)
			}
		}
		check("Properties parsed", len(pfReq.Prop.Props) == 4, &passed, &failed)
	}

	// Multi-status response
	davResponses := []xmlutil.DAVResponse{
		{
			Href: "/dav/tenant1/user1/calendars/",
			PropStats: []xmlutil.PropStat{
				{
					Props: map[string]string{
						"displayname":  `<D:displayname>My Calendars</D:displayname>`,
						"resourcetype": `<D:resourcetype><D:collection/></D:resourcetype>`,
					},
					Status: 200,
				},
			},
		},
		{
			Href: "/dav/tenant1/user1/calendars/default/",
			PropStats: []xmlutil.PropStat{
				{
					Props: map[string]string{
						"displayname":  `<D:displayname>Default Calendar</D:displayname>`,
						"resourcetype": `<D:resourcetype><D:collection/><C:calendar/></D:resourcetype>`,
					},
					Status: 200,
				},
			},
		},
	}

	var msBuf bytes.Buffer
	err = xmlutil.WriteMultiStatus(&msBuf, davResponses)
	multiStatus := msBuf.String()
	check("Generate multistatus XML", err == nil && len(multiStatus) > 100, &passed, &failed)
	check("Has <D:multistatus>", strings.Contains(multiStatus, "multistatus"), &passed, &failed)
	fmt.Printf("  Multi-status response (first 500 chars):\n")
	printTruncated(multiStatus, 500)

	// ============================================================
	// 8. JMAP Session Resource
	// ============================================================
	section("8. JMAP Session (RFC 8620)")

	session := models.JMAPSession{
		Capabilities: map[string]interface{}{
			"urn:ietf:params:jmap:core": map[string]interface{}{
				"maxSizeUpload": 50000000,
			},
			"urn:ietf:params:jmap:calendars": map[string]interface{}{
				"supportsRecurrences": true,
			},
		},
		Accounts: map[string]interface{}{
			tenantID + ":" + userID: map[string]interface{}{
				"name":       email,
				"isPersonal": true,
				"isReadOnly": false,
			},
		},
		PrimaryAccounts: map[string]string{
			"urn:ietf:params:jmap:calendars": tenantID + ":" + userID,
		},
		Username:       email,
		APIURL:         "http://localhost:8443/jmap/api",
		DownloadURL:    "http://localhost:8443/jmap/download/{accountId}/{blobId}/{name}",
		UploadURL:      "http://localhost:8443/jmap/upload/{accountId}/",
		EventSourceURL: "http://localhost:8443/jmap/eventsource",
		State:          uuid.New().String()[:8],
	}

	sessionJSON, err := json.MarshalIndent(session, "  ", "  ")
	check("Serialize JMAP session", err == nil, &passed, &failed)
	fmt.Printf("  Session JSON:\n")
	printTruncated(string(sessionJSON), 600)

	// ============================================================
	// 9. Model Serialization
	// ============================================================
	section("9. Model Serialization")

	cal := &models.Calendar{
		ID:                  uuid.New().String(),
		TenantID:            tenantID,
		UserID:              userID,
		Name:                "Work Calendar",
		Description:         "Work meetings and deadlines",
		Color:               "#0078D4",
		Timezone:            "America/New_York",
		IsDefault:           true,
		IsVisible:           true,
		SupportedComponents: []string{"VEVENT", "VTODO"},
		MaxResourceSize:     10485760,
		Properties:          json.RawMessage(`{}`),
	}

	calJSON, err := json.MarshalIndent(cal, "  ", "  ")
	check("Serialize Calendar", err == nil, &passed, &failed)
	fmt.Printf("  Calendar:\n  %s\n", string(calJSON)[:min(400, len(calJSON))])

	appPwModel := &models.AppPassword{
		ID:     uuid.New().String(),
		UserID: userID,
		Label:  "MacBook Calendar",
		Scopes: []string{"caldav", "jmap"},
	}
	apJSON, err := json.MarshalIndent(appPwModel, "  ", "  ")
	check("Serialize AppPassword", err == nil, &passed, &failed)
	fmt.Printf("  AppPassword:\n  %s\n", string(apJSON))

	jobResult := &models.JobResult{
		ID:         uuid.New().String(),
		JobType:    "send_scheduling_email",
		Payload:    json.RawMessage(`{"message_id":"abc-123","tenant_id":"` + tenantID + `"}`),
		Status:     "completed",
		DurationMs: 142,
	}
	jrJSON, err := json.MarshalIndent(jobResult, "  ", "  ")
	check("Serialize JobResult", err == nil, &passed, &failed)
	fmt.Printf("  JobResult:\n  %s\n", string(jrJSON))

	// ============================================================
	// 10. CalDAV URL Patterns
	// ============================================================
	section("10. CalDAV URL Patterns")

	patterns := []struct {
		path string
		desc string
	}{
		{"/.well-known/caldav", "Discovery endpoint"},
		{"/dav/tenant1/user1/", "Principal"},
		{"/dav/tenant1/user1/calendars/", "Calendar home"},
		{"/dav/tenant1/user1/calendars/default/", "Calendar collection"},
		{"/dav/tenant1/user1/calendars/default/event-001.ics", "Event resource"},
		{"/jmap/session", "JMAP session"},
		{"/jmap/api", "JMAP API"},
		{"/api/app-passwords/", "App password management"},
		{"/health", "Health check"},
	}
	for _, p := range patterns {
		fmt.Printf("  %-60s  %s\n", p.path, p.desc)
	}
	check("URL patterns documented", true, &passed, &failed)

	// ============================================================
	// Summary
	// ============================================================
	fmt.Println()
	fmt.Println("══════════════════════════════════════════════")
	fmt.Printf("  RESULTS: %d passed, %d failed\n", passed, failed)
	fmt.Println("══════════════════════════════════════════════")

	if failed > 0 {
		fmt.Println()
		fmt.Println("  Some tests failed! Check output above.")
		os.Exit(1)
	} else {
		fmt.Println()
		fmt.Println("  All tests passed!")
		fmt.Println()
		fmt.Println("  Next steps:")
		fmt.Println("    1. docker-compose up --build   # Start full stack")
		fmt.Println("    2. Create app password via API")
		fmt.Println("    3. Configure Apple Calendar / Thunderbird")
		fmt.Println()
	}
}

// ── Helpers ──────────────────────────────────────────────────

func section(title string) {
	fmt.Printf("\n── %s ──\n\n", title)
}

func check(name string, ok bool, passed, failed *int) {
	if ok {
		fmt.Printf("  [PASS] %s\n", name)
		*passed++
	} else {
		fmt.Printf("  [FAIL] %s\n", name)
		*failed++
	}
}

func printTruncated(s string, maxLen int) {
	if len(s) > maxLen {
		fmt.Printf("  %s...\n", s[:maxLen])
	} else {
		fmt.Printf("  %s\n", s)
	}
}
