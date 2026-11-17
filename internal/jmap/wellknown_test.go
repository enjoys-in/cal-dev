package jmap

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// /.well-known/jmap is the public autodiscovery URL (RFC 8620 §2.2): no auth, redirects to the
// Session resource, which then does the authentication.
func TestWellKnownIsPublicRedirectToSession(t *testing.T) {
	app := fiber.New()
	h := &Handler{}
	app.Get("/.well-known/jmap", h.WellKnown)

	resp, err := app.Test(httptest.NewRequest("GET", "/.well-known/jmap", nil)) // no Authorization
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 301 {
		t.Errorf("status = %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/jmap/session" {
		t.Errorf("Location = %q, want /jmap/session", loc)
	}
}
