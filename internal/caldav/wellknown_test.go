package caldav

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

// /.well-known/caldav is probed by clients before they have credentials (RFC 6764): it must
// redirect to the DAV root without any auth, for every method clients use to probe it.
func TestWellKnownIsPublicRedirectToDAVRoot(t *testing.T) {
	app := fiber.New(fiber.Config{
		RequestMethods: append(fiber.DefaultMethods, "PROPFIND"),
	})
	h := &Handler{}
	app.All("/.well-known/caldav", h.WellKnown)

	for _, method := range []string{"GET", "HEAD", "OPTIONS", "PROPFIND"} {
		req := httptest.NewRequest(method, "/.well-known/caldav", nil) // no Authorization, no cookies
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if resp.StatusCode != 307 {
			t.Errorf("%s: status = %d, want 307", method, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/dav/" {
			t.Errorf("%s: Location = %q, want /dav/", method, loc)
		}
	}
}
