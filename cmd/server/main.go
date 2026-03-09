package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/enjoys-in/cal-dev/internal/auth"
	"github.com/enjoys-in/cal-dev/internal/caldav"
	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/feeds"
	"github.com/enjoys-in/cal-dev/internal/jmap"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/middleware"
	"github.com/enjoys-in/cal-dev/internal/models"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/scheduling"
	"github.com/enjoys-in/cal-dev/internal/store"
	extsync "github.com/enjoys-in/cal-dev/internal/sync"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/google/uuid"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Load .env file (if present) so os.Getenv picks up the values
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found or error loading it: %v (continuing with OS env)", err)
	}

	// Load configuration
	cfg := config.Load()
	log.Printf("Starting CalDAV+JMAP server on %s", cfg.Server.Addr())

	// Connect to database
	db, err := store.New(&cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()
	log.Println("Connected to PostgreSQL")

	// Run migrations if migration file exists
	if migrationSQL, err := os.ReadFile("migrations/001_init.sql"); err == nil {
		if err := db.RunMigration(string(migrationSQL)); err != nil {
			log.Printf("Migration warning (may already be applied): %v", err)
		} else {
			log.Println("Database migration applied successfully")
		}
	}

	// Connect to Valkey (Redis-compatible)
	jobQueue, err := jobs.NewQueue(&cfg.Redis, &cfg.Worker)
	if err != nil {
		log.Fatalf("Failed to connect to Valkey: %v", err)
	}
	defer jobQueue.Close()

	// Initialize services
	authSvc := auth.NewService(db, &cfg.Auth)
	schedulingSvc := scheduling.NewService(db, &cfg.SMTP, jobQueue)
	syncSvc := extsync.NewService(db, jobQueue)

	// Initialize background worker
	worker := jobs.NewWorker(jobQueue, db, &cfg.Worker)

	// Register job handlers
	worker.Register(jobs.JobSendSchedulingEmail, schedulingSvc.HandleSendEmailJob)
	worker.Register(jobs.JobCleanupLocks, func(ctx context.Context, job *jobs.Job) error {
		return db.CleanupExpiredLocks(ctx)
	})
	worker.Register(jobs.JobCleanupJobHistory, func(ctx context.Context, job *jobs.Job) error {
		return db.CleanupJobHistory(ctx)
	})
	worker.Register(extsync.JobSyncExternalCalendar, syncSvc.HandleSyncJob)
	worker.Register(extsync.JobEnqueueDueSyncs, syncSvc.HandleEnqueueDueSyncsJob)

	// Start workers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker.Start(ctx)

	// Initialize handlers
	caldavHandler := caldav.NewHandler(db)
	baseURL := "https://localhost:" + fmt.Sprintf("%d", cfg.Server.Port)
	if !cfg.TLS.Enabled {
		baseURL = "http://localhost:" + fmt.Sprintf("%d", cfg.Server.Port)
	}
	jmapHandler := jmap.NewHandler(db, baseURL)

	// Create Fiber app with WebDAV method support
	app := fiber.New(fiber.Config{
		ServerHeader:          "CalDev/1.0",
		ReadTimeout:           cfg.Server.ReadTimeout,
		WriteTimeout:          cfg.Server.WriteTimeout,
		IdleTimeout:           cfg.Server.IdleTimeout,
		Prefork:               cfg.Server.Prefork,
		DisableStartupMessage: false,
		BodyLimit:             10 * 1024 * 1024, // 10MB
		RequestMethods: append(fiber.DefaultMethods,
			"PROPFIND", "PROPPATCH", "MKCALENDAR", "REPORT",
			"LOCK", "UNLOCK", "MOVE", "COPY",
		),
	})

	// Global middleware
	app.Use(recover.New())
	app.Use(compress.New())
	app.Use(middleware.SecurityHeaders())
	app.Use(middleware.RequestLogger())
	app.Use(middleware.CORSHeaders())
	app.Use(middleware.AppHeaders(&cfg.App))
	app.Use(middleware.RateLimiter(1000, time.Minute))
	app.Use(middleware.XMLSizeLimit(10 * 1024 * 1024)) // 10MB XML limit

	// ============================================================
	// Health Check
	// ============================================================
	app.Get("/health", func(c *fiber.Ctx) error {
		return response.Success(c, "service is healthy", fiber.Map{
			"status":  "ok",
			"service": "caldev",
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})

	// ============================================================
	// Well-Known CalDAV Discovery (RFC 6764)
	// ============================================================
	app.All("/.well-known/caldav", middleware.DAVAuth(authSvc, db), caldavHandler.WellKnown)

	// ============================================================
	// CalDAV Routes — /dav/{tenant}/{user}/...
	// ============================================================
	davGroup := app.Group("/dav", middleware.DAVAuth(authSvc, db))

	// OPTIONS on all paths
	davGroup.Options("/*", caldavHandler.Options)

	// Principal
	davGroup.All("/:tenant/:user", caldavHandler.Propfind).Name("principal")
	davGroup.All("/:tenant/:user/", caldavHandler.Propfind)

	// Calendar Home
	davGroup.All("/:tenant/:user/calendars", caldavHandler.Propfind)
	davGroup.All("/:tenant/:user/calendars/", caldavHandler.Propfind)

	// Calendar Collection & Events
	davGroup.All("/:tenant/:user/calendars/:calendar", handleDAVMethod(caldavHandler))
	davGroup.All("/:tenant/:user/calendars/:calendar/", handleDAVMethod(caldavHandler))
	davGroup.All("/:tenant/:user/calendars/:calendar/:event", handleDAVMethod(caldavHandler))

	// ============================================================
	// JMAP Routes — /jmap/...
	// ============================================================
	jmapGroup := app.Group("/jmap", middleware.JMAPAuth(authSvc, db))

	// JMAP Session (RFC 8620 §2)
	jmapGroup.Get("/session", jmapHandler.Session)
	jmapGroup.Get("/.well-known/jmap", jmapHandler.Session)

	// JMAP API endpoint (RFC 8620 §3)
	jmapGroup.Post("/api", jmapHandler.API)

	// ============================================================
	// ICS Feed Routes — /feeds/{tenant}/{user}/... (Outlook, Google, Apple)
	// ============================================================
	feedHandler := feeds.NewHandler(db)
	feedGroup := app.Group("/feeds", middleware.DAVAuth(authSvc, db))

	// Feed index — lists available feeds with subscribe URLs
	feedGroup.Get("/:tenant/:user", feedHandler.FeedIndex)
	feedGroup.Get("/:tenant/:user/", feedHandler.FeedIndex)

	// Merged feed of all calendars
	feedGroup.Get("/:tenant/:user/all.ics", feedHandler.AllCalendarsFeed)

	// Single calendar feed
	feedGroup.Get("/:tenant/:user/:calendar", feedHandler.CalendarFeed)

	// ============================================================
	// App Password Management (authenticated)
	// ============================================================
	appPwGroup := app.Group("/api/app-passwords", middleware.JMAPAuth(authSvc, db))
	appPwGroup.Post("/", createAppPasswordHandler(db))
	appPwGroup.Delete("/:id", deleteAppPasswordHandler(db))

	// ============================================================
	// Scheduling — Inbound iMIP (RSVP replies) & User RSVP
	// ============================================================
	schedulingHandler := scheduling.NewHandler(schedulingSvc)
	schedGroup := app.Group("/api/scheduling", middleware.JMAPAuth(authSvc, db))
	schedGroup.Post("/incoming", schedulingHandler.IncomingIMIP)
	schedGroup.Post("/rsvp", schedulingHandler.RSVPRespond)

	// ============================================================
	// Calendar Subscriptions — Import from Google/Outlook/any ICS
	// ============================================================
	syncHandler := extsync.NewHandler(db, syncSvc)
	subGroup := app.Group("/api/subscriptions", middleware.JMAPAuth(authSvc, db))
	subGroup.Get("/", syncHandler.List)
	subGroup.Post("/", syncHandler.Create)
	subGroup.Get("/:id", syncHandler.Get)
	subGroup.Put("/:id", syncHandler.Update)
	subGroup.Delete("/:id", syncHandler.Delete)
	subGroup.Post("/:id/sync", syncHandler.SyncNow)

	// ============================================================
	// Admin API — API-key protected user provisioning
	// ============================================================
	adminGroup := app.Group("/api/admin", middleware.APIKeyAuth(cfg.Auth.AdminAPIKey))
	adminGroup.Post("/users", createUserHandler(db))

	// ============================================================
	// Graceful Shutdown
	// ============================================================
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Shutting down server...")
		cancel()
		worker.Stop()

		if err := app.ShutdownWithTimeout(cfg.Server.ShutdownTimeout); err != nil {
			log.Printf("Server forced shutdown: %v", err)
		}
	}()

	// Start server
	listenAddr := cfg.Server.Addr()
	if cfg.TLS.Enabled && cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		log.Printf("Starting HTTPS server on %s", listenAddr)
		if err := app.ListenTLS(listenAddr, cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	} else {
		log.Printf("Starting HTTP server on %s", listenAddr)
		if err := app.Listen(listenAddr); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}
}

// handleDAVMethod routes WebDAV methods to the correct handler.
func handleDAVMethod(h *caldav.Handler) fiber.Handler {
	return func(c *fiber.Ctx) error {
		switch c.Method() {
		case "OPTIONS":
			return h.Options(c)
		case "PROPFIND":
			return h.Propfind(c)
		case "PROPPATCH":
			return h.Proppatch(c)
		case "REPORT":
			return h.Report(c)
		case "GET", "HEAD":
			return h.Get(c)
		case "PUT":
			return h.Put(c)
		case "DELETE":
			return h.Delete(c)
		case "MKCALENDAR":
			return h.Mkcalendar(c)
		case "MOVE":
			return h.Move(c)
		case "COPY":
			return h.Copy(c)
		case "LOCK":
			return h.Lock(c)
		case "UNLOCK":
			return h.Unlock(c)
		default:
			return c.Status(405).SendString("Method Not Allowed")
		}
	}
}

// createAppPasswordHandler creates a new app password for CalDAV Basic Auth.
func createAppPasswordHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID := c.Locals("user_id").(string)

		var req struct {
			Label    string `json:"label"`
			Password string `json:"password"`
		}
		if err := c.BodyParser(&req); err != nil {
			return response.Error(c, 400, "invalid request")
		}
		if req.Password == "" {
			return response.Error(c, 400, "password required")
		}

		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			return response.Error(c, 500, "password hash failed")
		}

		ap := &store.AppPasswordInput{
			UserID:       userID,
			Label:        req.Label,
			PasswordHash: hash,
		}

		if err := db.CreateAppPasswordFromInput(c.UserContext(), ap); err != nil {
			return response.Error(c, 500, "create failed")
		}

		return response.Created(c, "App password created. Use your email + this password for CalDAV Basic Auth.", fiber.Map{
			"id":    ap.ID,
			"label": ap.Label,
		})
	}
}

// deleteAppPasswordHandler deletes an app password.
func deleteAppPasswordHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Params("id")
		if err := db.DeleteAppPassword(c.UserContext(), id); err != nil {
			return response.Error(c, 500, "delete failed")
		}
		return c.SendStatus(204)
	}
}

// createUserHandler looks up an existing mail_accounts user and creates their default calendar.
func createUserHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req struct {
			Email string `json:"email"`
		}
		if err := c.BodyParser(&req); err != nil {
			return response.Error(c, 400, "invalid request body")
		}
		if req.Email == "" {
			return response.Error(c, 400, "email is required")
		}

		// Look up user from mail_accounts
		user, err := db.GetUserByEmail(c.UserContext(), req.Email)
		if err != nil || user == nil {
			return response.Error(c, 404, "user not found in mail_accounts")
		}

		// Create default calendar (tenant_id = user.ID)
		cal := &models.Calendar{
			ID:                  uuid.New().String(),
			TenantID:            user.ID,
			UserID:              user.ID,
			Name:                "Personal",
			Description:         "Default calendar",
			Color:               "#0078D4",
			Timezone:            "UTC",
			IsDefault:           true,
			IsVisible:           true,
			CTag:                uuid.New().String(),
			SyncToken:           "sync-" + uuid.New().String(),
			SupportedComponents: []string{"VEVENT", "VTODO"},
			MaxResourceSize:     10485760,
			Properties:          []byte("{}"),
		}
		if err := db.CreateCalendar(c.UserContext(), cal); err != nil {
			log.Printf("Warning: default calendar may already exist: %v", err)
		}

		return response.Created(c, "User provisioned for CalDev", fiber.Map{
			"user_id":     user.ID,
			"email":       user.Email,
			"tenant_id":   user.ID,
			"calendar_id": cal.ID,
		})
	}
}

// Ensure fmt is used
var _ = fmt.Sprintf
