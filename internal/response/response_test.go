package response

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func setupApp() *fiber.App {
	return fiber.New()
}

func parseBody(t *testing.T, resp *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	body, _ := io.ReadAll(resp.Result().Body)
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("failed to parse response body: %v\nbody: %s", err, string(body))
	}
	return m
}

func TestSuccess(t *testing.T) {
	app := setupApp()
	app.Get("/ok", func(c *fiber.Ctx) error {
		return Success(c, "it works", fiber.Map{"foo": "bar"})
	})

	req := httptest.NewRequest("GET", "/ok", nil)
	resp, _ := app.Test(req)

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	json.Unmarshal(body, &m)

	if m["success"] != true {
		t.Error("expected success=true")
	}
	if m["message"] != "it works" {
		t.Errorf("expected message 'it works', got '%v'", m["message"])
	}
	result, ok := m["result"].(map[string]interface{})
	if !ok {
		t.Fatal("expected result to be an object")
	}
	if result["foo"] != "bar" {
		t.Errorf("expected result.foo='bar', got '%v'", result["foo"])
	}
}

func TestCreated(t *testing.T) {
	app := setupApp()
	app.Post("/create", func(c *fiber.Ctx) error {
		return Created(c, "resource created", fiber.Map{"id": "123"})
	})

	req := httptest.NewRequest("POST", "/create", nil)
	resp, _ := app.Test(req)

	if resp.StatusCode != 201 {
		t.Errorf("expected 201, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	json.Unmarshal(body, &m)

	if m["success"] != true {
		t.Error("expected success=true")
	}
	if m["message"] != "resource created" {
		t.Errorf("expected message 'resource created', got '%v'", m["message"])
	}
}

func TestError_WithoutDetails(t *testing.T) {
	app := setupApp()
	app.Get("/err", func(c *fiber.Ctx) error {
		return Error(c, 400, "bad request")
	})

	req := httptest.NewRequest("GET", "/err", nil)
	resp, _ := app.Test(req)

	if resp.StatusCode != 400 {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	json.Unmarshal(body, &m)

	if m["success"] != false {
		t.Error("expected success=false")
	}
	if m["message"] != "bad request" {
		t.Errorf("expected message 'bad request', got '%v'", m["message"])
	}
	if m["result"] != nil {
		t.Error("expected result=null")
	}
}

func TestError_WithDetails(t *testing.T) {
	app := setupApp()
	app.Get("/err2", func(c *fiber.Ctx) error {
		return Error(c, 500, "sync failed", "HTTP 404: Not Found")
	})

	req := httptest.NewRequest("GET", "/err2", nil)
	resp, _ := app.Test(req)

	if resp.StatusCode != 500 {
		t.Errorf("expected 500, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	json.Unmarshal(body, &m)

	if m["success"] != false {
		t.Error("expected success=false")
	}
	errObj, ok := m["error"].(map[string]interface{})
	if !ok {
		t.Fatal("expected error to be an object")
	}
	if errObj["details"] != "HTTP 404: Not Found" {
		t.Errorf("expected error.details='HTTP 404: Not Found', got '%v'", errObj["details"])
	}
}

func TestError_404(t *testing.T) {
	app := setupApp()
	app.Get("/notfound", func(c *fiber.Ctx) error {
		return Error(c, 404, "not found")
	})

	req := httptest.NewRequest("GET", "/notfound", nil)
	resp, _ := app.Test(req)

	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestSuccess_NilResult(t *testing.T) {
	app := setupApp()
	app.Get("/nil", func(c *fiber.Ctx) error {
		return Success(c, "ok", nil)
	})

	req := httptest.NewRequest("GET", "/nil", nil)
	resp, _ := app.Test(req)

	body, _ := io.ReadAll(resp.Body)
	var m map[string]interface{}
	json.Unmarshal(body, &m)

	if m["success"] != true {
		t.Error("expected success=true")
	}
	if m["result"] != nil {
		t.Error("expected result=null for nil input")
	}
}
