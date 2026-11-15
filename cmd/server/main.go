package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/enjoys-in/cal-dev/internal/auth"
	"github.com/enjoys-in/cal-dev/internal/availability"
	"github.com/enjoys-in/cal-dev/internal/caldav"
	"github.com/enjoys-in/cal-dev/internal/config"
	"github.com/enjoys-in/cal-dev/internal/feeds"
	"github.com/enjoys-in/cal-dev/internal/jmap"
	"github.com/enjoys-in/cal-dev/internal/jobs"
	"github.com/enjoys-in/cal-dev/internal/middleware"
	"github.com/enjoys-in/cal-dev/internal/publisher"
	"github.com/enjoys-in/cal-dev/internal/realtime"
	"github.com/enjoys-in/cal-dev/internal/response"
	"github.com/enjoys-in/cal-dev/internal/scheduling"
	"github.com/enjoys-in/cal-dev/internal/sharing"
	"github.com/enjoys-in/cal-dev/internal/store"
	extsync "github.com/enjoys-in/cal-dev/internal/sync"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Load .env file (if present) so os.Getenv picks up the values
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found or error loading it: %v (continuing with OS env)", err)
	}

	// Load configuration
	cfg := config.Load()
	if fatal, warnings := cfg.Validate(); len(fatal) > 0 || len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("SECURITY WARNING: %s", w)
		}
		if len(fatal) > 0 {
			for _, f := range fatal {
				log.Printf("SECURITY ERROR: %s", f)
			}
			log.Fatalf("refusing to start with insecure configuration (%d problem(s) above)", len(fatal))
		}
	}
	log.Printf("Starting CalDAV+JMAP server on %s", cfg.Server.Addr())

	// Connect to database
	db, err := store.New(&cfg.Database)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()
	log.Println("Connected to PostgreSQL")

	// Ensure migration tracking table exists (runs outside the main migration transaction)
	db.RunMigration(`CREATE TABLE IF NOT EXISTS caldev_migrations (
		id SERIAL PRIMARY KEY,
		filename VARCHAR(512) NOT NULL UNIQUE,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)

	// Run migrations in order
	migrationFiles := []string{
		"migrations/001_init.sql",
		"migrations/002_sharing_realtime.sql",
		"migrations/003_recurrence_overrides.sql",
		"migrations/004_reminders.sql",
	}
	for _, mf := range migrationFiles {
		sqlBytes, err := os.ReadFile(mf)
		if err != nil {
			log.Printf("Migration file %s not found, skipping", mf)
			continue
		}
		applied, err := db.IsMigrationApplied(mf)
		if err != nil {
			applied = false
		}
		if applied {
			log.Printf("Migration %s already applied, skipping", mf)
			continue
		}
		if err := db.RunMigration(string(sqlBytes)); err != nil {
			log.Printf("Migration %s failed: %v", mf, err)
			continue
		}
		log.Printf("Migration %s applied successfully", mf)
		if err := db.RecordMigration(mf); err != nil {
			log.Printf("Failed to record migration %s: %v", mf, err)
		}
	}

	// Split resources saved before recurrence overrides had their own rows (idempotent).
	if n, err := db.RepairFlattenedResources(context.Background()); err != nil {
		log.Printf("WARNING: repairing flattened recurring events failed: %v", err)
	} else if n > 0 {
		log.Printf("Repaired %d recurring event(s) that had been stored without their overrides", n)
	}

	// Connect to Valkey (Redis-compatible)
	jobQueue, err := jobs.NewQueue(&cfg.Redis, &cfg.Worker)
	if err != nil {
		log.Fatalf("Failed to connect to Valkey: %v", err)
	}
	defer jobQueue.Close()

	// Initialize services
	authSvc := auth.NewService(db, &cfg.Auth)
	mailPub := publisher.New(jobQueue.Client())
	schedulingSvc := scheduling.NewService(db, jobQueue, mailPub)
	syncSvc := extsync.NewService(db, jobQueue)

	// Initialize background worker
	worker := jobs.NewWorker(jobQueue, db, &cfg.Worker)

	// Register job handlers
	worker.Register(jobs.JobSendSchedulingEmail, schedulingSvc.HandleSendEmailJob)
	worker.Register(jobs.JobCalendarReminder, schedulingSvc.HandleCalendarReminderJob)
	worker.Register(jobs.JobSweepReminders, schedulingSvc.HandleSweepRemindersJob)
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

	// Availability (free/busy) + realtime push. The hub listens on Postgres NOTIFY, so it sees
	// every write regardless of which API (CalDAV, JMAP, sync, scheduling) made it.
	availSvc := availability.NewService(db, cfg.Sharing.OrgVisible)
	rtHub := realtime.NewHub(db, availSvc)
	rtHub.Start(ctx, cfg.Database.DSN())

	// Initialize handlers
	caldavHandler := caldav.NewHandler(db, schedulingSvc)
	caldavHandler.SetAvailability(availSvc)
	baseURL := "https://localhost:" + fmt.Sprintf("%d", cfg.Server.Port)
	if !cfg.TLS.Enabled {
		baseURL = "http://localhost:" + fmt.Sprintf("%d", cfg.Server.Port)
	}
	jmapHandler := jmap.NewHandler(db, baseURL, schedulingSvc)
	jmapHandler.SetRealtime(rtHub)
	jmapHandler.SetAvailability(availSvc)

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
	// Never compress the SSE stream: gzip would buffer events until the buffer fills.
	app.Use(compress.New(compress.Config{
		Next: func(c *fiber.Ctx) bool { return c.Path() == "/jmap/eventsource" },
	}))
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
	davGroup.All("/:tenant/:user", handleDAVMethod(caldavHandler)).Name("principal")
	davGroup.All("/:tenant/:user/", handleDAVMethod(caldavHandler))

	// Scheduling inbox/outbox (RFC 6638): free-busy requests are POSTed to the outbox
	davGroup.All("/:tenant/:user/inbox", handleDAVMethod(caldavHandler))
	davGroup.All("/:tenant/:user/inbox/", handleDAVMethod(caldavHandler))
	davGroup.All("/:tenant/:user/outbox", handleDAVMethod(caldavHandler))
	davGroup.All("/:tenant/:user/outbox/", handleDAVMethod(caldavHandler))

	// Calendar Home
	davGroup.All("/:tenant/:user/calendars", handleDAVMethod(caldavHandler))
	davGroup.All("/:tenant/:user/calendars/", handleDAVMethod(caldavHandler))

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

	// JMAP EventSource (RFC 8620 §7)
	jmapGroup.Get("/eventsource", jmapHandler.EventSource)

	// ============================================================
	// Team availability & sharing (REST)
	// ============================================================
	sharingHandler := sharing.NewHandler(db, availSvc, cfg.Sharing.OrgVisible)

	// Free/busy of teammates who shared with you: GET /api/availability?emails=a@x,b@x&start=&end=
	availGroup := app.Group("/api/availability", middleware.JMAPAuth(authSvc, db))
	availGroup.Get("/", sharingHandler.Lookup)
	availGroup.Get("/shares", sharingHandler.ListAvailabilityShares)
	availGroup.Post("/shares", sharingHandler.CreateAvailabilityShare)
	availGroup.Delete("/shares/:user_id", sharingHandler.DeleteAvailabilityShare)

	// Share one of your calendars read-only
	calShareGroup := app.Group("/api/calendars", middleware.JMAPAuth(authSvc, db))
	calShareGroup.Get("/:id/shares", sharingHandler.ListCalendarShares)
	calShareGroup.Post("/:id/shares", sharingHandler.CreateCalendarShare)
	calShareGroup.Delete("/:id/shares/:user_id", sharingHandler.DeleteCalendarShare)

	// Calendars others shared with you
	sharedGroup := app.Group("/api/shared/calendars", middleware.JMAPAuth(authSvc, db))
	sharedGroup.Get("/", sharingHandler.ListSharedCalendars)
	sharedGroup.Get("/:id/events", sharingHandler.SharedCalendarEvents)

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
	// User Onboarding & Admin API
	// ============================================================
	onboardHandler := createUserHandler(db, authSvc)
	app.Post("/api/calendar/onboard", onboardHandler)
	app.Post("/api/users/onboard", onboardHandler)

	adminGroup := app.Group("/api/admin", middleware.APIKeyAuth(cfg.Auth.AdminAPIKey))
	adminGroup.Post("/users", onboardHandler)

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
		case "POST":
			return h.Post(c)
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
		userID := c.Locals("user_id").(int)

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
			return response.Error(c, 500, "create failed", err.Error())
		}

		return response.Created(c, "App password created. Use your email + this password for CalDAV Basic Auth.", fiber.Map{
			"id":    ap.ID,
			"label": ap.Label,
		})
	}
}

// deleteAppPasswordHandler deletes an app password scoped to user.
func deleteAppPasswordHandler(db *store.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		userID, _ := c.Locals("user_id").(int)
		if userID == 0 {
			return response.Error(c, 401, "unauthorized")
		}
		id := c.Params("id")
		if err := db.DeleteAppPasswordForUser(c.UserContext(), userID, id); err != nil {
			return response.Error(c, 500, "delete failed")
		}
		return c.SendStatus(204)
	}
}

// createUserHandler provisions a user in mail_accounts (one per email) and creates their default calendar.
func createUserHandler(db *store.Store, authSvc *auth.Service) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		_ = c.BodyParser(&req)

		// If email not in body, try to extract from Bearer token
		if req.Email == "" {
			if authHeader := c.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
				token := strings.TrimPrefix(authHeader, "Bearer ")
				if claims, err := authSvc.ValidateToken(token); err == nil && claims.Email != "" {
					req.Email = claims.Email
					if req.Name == "" {
						req.Name = claims.DisplayName
					}
				}
			}
		}

		if req.Email == "" {
			return response.Error(c, 400, "email is required")
		}

		// Look up or insert user into mail_accounts (ensuring 1 user per email)
		user, err := db.EnsureUserByEmail(c.UserContext(), req.Email, req.Name)
		if err != nil {
			return response.Error(c, 500, "failed to provision user: "+err.Error())
		}

		// Ensure default calendar exists (tenant_id == user.ID)
		if err := db.EnsureDefaultCalendar(c.UserContext(), user.ID, user.ID); err != nil {
			log.Printf("Warning: ensure default calendar: %v", err)
		}

		cals, _ := db.GetCalendarsByUser(c.UserContext(), user.ID, user.ID)
		var calID string
		if len(cals) > 0 {
			calID = cals[0].ID
		}

		return response.Success(c, "User connected to Calendar Service", fiber.Map{
			"user_id":     user.ID,
			"email":       user.Email,
			"tenant_id":   user.ID,
			"calendar_id": calID,
			"calendars":   cals,
		})
	}
}

// Ensure fmt is used
var _ = fmt.Sprintf
