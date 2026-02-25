package response

import "github.com/gofiber/fiber/v2"

// Success returns a standardised success envelope.
//
//	{ "success": true, "message": "...", "result": { ... } }
func Success(c *fiber.Ctx, message string, result interface{}) error {
	return c.JSON(fiber.Map{
		"success": true,
		"message": message,
		"result":  result,
	})
}

// Created returns a 201 success envelope.
func Created(c *fiber.Ctx, message string, result interface{}) error {
	return c.Status(201).JSON(fiber.Map{
		"success": true,
		"message": message,
		"result":  result,
	})
}

// Error returns a standardised error envelope.
//
//	{ "success": false, "message": "...", "result": null, "error": { "details": "..." } }
func Error(c *fiber.Ctx, status int, message string, details ...string) error {
	errMap := fiber.Map{}
	if len(details) > 0 && details[0] != "" {
		errMap["details"] = details[0]
	}
	return c.Status(status).JSON(fiber.Map{
		"success": false,
		"message": message,
		"result":  nil,
		"error":   errMap,
	})
}
