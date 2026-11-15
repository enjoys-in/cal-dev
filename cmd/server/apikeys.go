package main

import (
	"log"
	"strings"

	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/store"
	"github.com/gofiber/fiber/v2"
)

// maxOwnAPIKeys caps the active onboarding keys one user may hold.
const maxOwnAPIKeys = 10

// publicMailDomains are shared webmail domains. A key scoped to one of them would let its owner
// onboard (and read the calendars of) strangers who merely use the same provider, so users can't
// create keys for them. The admin still can, deliberately.
var publicMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true,
	"live.com": true, "msn.com": true, "yahoo.com": true, "ymail.com": true, "icloud.com": true,
	"me.com": true, "mac.com": true, "aol.com": true, "proton.me": true, "protonmail.com": true,
	"gmx.com": true, "gmx.net": true, "mail.com": true, "yandex.com": true, "yandex.ru": true,
	"zohomail.com": true, "qq.com": true, "163.com": true, "rediffmail.com": true,
}

// createdKeyResponse is returned once: the key text can't be retrieved again.
func createdKeyResponse(c *fiber.Ctx, key string, k any) error {
	return response.Created(c, "API key created. Store the key now: it is not shown again.", fiber.Map{
		"key":     key,
		"api_key": k,
	})
}

// --- Signed-in user: keys for their own email (scoped to its domain) ---

func listOwnAPIKeysHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		email, _ := c.Locals("email").(string)
		keys, err := db.ListAPIKeys(c.UserContext(), email)
		if err != nil {
			log.Printf("api-keys: list for %s: %v", email, err)
			return response.Error(c, 500, "failed to list API keys")
		}
		return response.Success(c, "API keys", fiber.Map{"api_keys": keys})
	}
}

func createOwnAPIKeyHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		email, _ := c.Locals("email").(string)
		userID, _ := c.Locals("user_id").(int)
		var req struct {
			Label string `json:"label"`
		}
		_ = c.BodyParser(&req)

		domain := store.EmailDomain(email)
		if domain == "" {
			return response.Error(c, 400, "your account has no email domain")
		}
		if publicMailDomains[domain] {
			return response.Error(c, 403, "API keys can't be created for public email domains like "+domain)
		}
		keys, err := db.ListAPIKeys(c.UserContext(), email)
		if err != nil {
			log.Printf("api-keys: list for %s: %v", email, err)
			return response.Error(c, 500, "failed to create API key")
		}
		active := 0
		for _, k := range keys {
			if k.RevokedAt == nil {
				active++
			}
		}
		if active >= maxOwnAPIKeys {
			return response.Error(c, 409, "too many active API keys; revoke one first")
		}

		key, k, err := db.CreateAPIKey(c.UserContext(), email, req.Label, &userID)
		if err != nil {
			log.Printf("api-keys: create for %s: %v", email, err)
			return response.Error(c, 500, "failed to create API key")
		}
		return createdKeyResponse(c, key, k)
	}
}

func revokeOwnAPIKeyHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		email, _ := c.Locals("email").(string)
		ok, err := db.RevokeAPIKey(c.UserContext(), c.Params("id"), email)
		if err != nil {
			log.Printf("api-keys: revoke for %s: %v", email, err)
			return response.Error(c, 500, "failed to revoke API key")
		}
		if !ok {
			return response.Error(c, 404, "API key not found")
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}

// --- Admin (ADMIN_API_KEY): keys for any owner email ---

func adminListAPIKeysHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		keys, err := db.ListAPIKeys(c.UserContext(), c.Query("owner_email"))
		if err != nil {
			log.Printf("api-keys: admin list: %v", err)
			return response.Error(c, 500, "failed to list API keys")
		}
		return response.Success(c, "API keys", fiber.Map{"api_keys": keys})
	}
}

func adminCreateAPIKeyHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req struct {
			OwnerEmail string `json:"owner_email"`
			Label      string `json:"label"`
		}
		if err := c.BodyParser(&req); err != nil {
			return response.Error(c, 400, "invalid request body")
		}
		if store.EmailDomain(strings.TrimSpace(req.OwnerEmail)) == "" {
			return response.Error(c, 400, "owner_email must be a valid email")
		}
		key, k, err := db.CreateAPIKey(c.UserContext(), req.OwnerEmail, req.Label, nil)
		if err != nil {
			log.Printf("api-keys: admin create for %s: %v", req.OwnerEmail, err)
			return response.Error(c, 500, "failed to create API key")
		}
		return createdKeyResponse(c, key, k)
	}
}

func adminRevokeAPIKeyHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ok, err := db.RevokeAPIKey(c.UserContext(), c.Params("id"), "")
		if err != nil {
			log.Printf("api-keys: admin revoke: %v", err)
			return response.Error(c, 500, "failed to revoke API key")
		}
		if !ok {
			return response.Error(c, 404, "API key not found")
		}
		return c.SendStatus(fiber.StatusNoContent)
	}
}
