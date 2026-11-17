package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/enjoys-in/cal-dev/internal/airsend"
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
	"github.com/enjoys-in/cal-dev/internal/smtp"
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

	// CalDev lives in its own schema (DB_SCHEMA, default caldev). Before anything else, move
	// CalDev objects an older version left in public into it (data kept, nothing dropped), so the
	// migration table below is found there instead of being created empty.
	if err := db.PrepareSchema(cfg.Database.SchemaName()); err != nil {
		log.Fatalf("Preparing schema %q failed: %v", cfg.Database.SchemaName(), err)
	}
	log.Printf("Using schema %q", cfg.Database.SchemaName())

	// Run migrations in order (the history table is created first, inside CalDev's schema)
	migrationFiles := []string{
		"migrations/001_init.sql",
		"migrations/002_sharing_realtime.sql",
		"migrations/003_recurrence_overrides.sql",
		"migrations/004_reminders.sql",
		"migrations/005_calendar_byo_smtp.sql",
		"migrations/006_change_log_fixes.sql",
		"migrations/007_onboarding_api_keys.sql",
		"migrations/008_mail_accounts_email_lower.sql",
		"migrations/009_drop_table_prefix.sql",
		"migrations/010_caldev_mail_accounts.sql",
	}
	// The original layout: schema/full_init.sql already contains what these built. A database
	// created from that file (no migration history) skips them; 009 onward still run, which is
	// safe on the final shape. When full_init.sql absorbs more of the history, extend this list.
	baselineMigrations := migrationFiles[:8]
	// A missing or failed migration stops startup: running on a partial schema breaks later features.
	if err := db.ApplyMigrations(migrationFiles, baselineMigrations, os.ReadFile, log.Printf); err != nil {
		log.Fatalf("Migrations failed: %v", err)
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
	schedulingSvc.SetSecrets(cfg.Auth.JWTSecret, cfg.Server.PublicURL())
	if c := airsend.New(cfg.Airsend.InternalURL, cfg.Airsend.Token, cfg.Airsend.Secret, cfg.Airsend.Timeout); c != nil {
		schedulingSvc.SetAirsendClient(c)
		log.Printf("Calendar emails of internal accounts go to airsend-api (%s)", cfg.Airsend.InternalURL)
	} else {
		log.Printf("WARNING: AIRSEND_INTERNAL_URL is not set (check that docker-compose passes it): invitation emails of Airsend accounts cannot be sent; only calendars with their own SMTP can send")
	}
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
	hubCtx, stopHub := context.WithCancel(ctx) // stopped first at shutdown so SSE streams end
	rtHub.Start(hubCtx, cfg.Database.DSN())

	// Initialize handlers
	caldavHandler := caldav.NewHandler(db, schedulingSvc)
	caldavHandler.SetAvailability(availSvc)
	// Advertised in the JMAP session (apiUrl, eventSourceUrl): the public URL (CALDEV_PUBLIC_URL, or
	// https://calender.airsend.in by default).
	baseURL := cfg.Server.PublicURL()
	jmapHandler := jmap.NewHandler(db, baseURL, schedulingSvc)
	jmapHandler.SetRealtime(rtHub)
	jmapHandler.SetAvailability(availSvc)
	jmapHandler.SetLocker(jobQueue.Client(), cfg.Worker.QueuePrefix) // ifInState lock shared by all instances

	// Create Fiber app with WebDAV method support
	app := fiber.New(fiber.Config{
		ServerHeader:          "CalDev/1.0",
		ReadTimeout:           cfg.Server.ReadTimeout,
		WriteTimeout:          cfg.Server.WriteTimeout,
		IdleTimeout:           cfg.Server.IdleTimeout,
		Prefork:               cfg.Server.Prefork,
		DisableStartupMessage: false,
		BodyLimit:             10 * 1024 * 1024, // 10MB
		// Behind the reverse proxy, take the client IP from the proxy header (rate limiting, logs),
		// but only when the request really comes from a trusted proxy.
		ProxyHeader:             proxyHeader(),
		EnableTrustedProxyCheck: true,
		TrustedProxies:          trustedProxies(),
		EnableIPValidation:      true,
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
	// Well-Known Discovery — public on purpose: clients probe these before they have
	// credentials, and each only redirects (the target does the authentication).
	//   RFC 6764 §6   /.well-known/caldav → /dav/
	//   RFC 8620 §2.2 /.well-known/jmap   → /jmap/session
	// ============================================================
	app.All("/.well-known/caldav", caldavHandler.WellKnown)
	app.Get("/.well-known/jmap", jmapHandler.WellKnown)

	// ============================================================
	// CalDAV Routes — /dav/{tenant}/{user}/...
	// ============================================================
	davGroup := app.Group("/dav", middleware.DAVAuth(authSvc, db))

	// OPTIONS on all paths
	davGroup.Options("/*", caldavHandler.Options)

	// DAV context root: where /.well-known/caldav redirects. PROPFIND here returns
	// current-user-principal so clients can find /dav/{tenant}/{user}/ after signing in.
	davGroup.Add("PROPFIND", "/", caldavHandler.Propfind)

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
	availGroup.Get("/people", sharingHandler.People)
	availGroup.Get("/shares", sharingHandler.ListAvailabilityShares)
	availGroup.Post("/shares", sharingHandler.CreateAvailabilityShare)
	availGroup.Delete("/shares/:user_id", sharingHandler.DeleteAvailabilityShare)

	// Share one of your calendars read-only
	calShareGroup := app.Group("/api/calendars", middleware.JMAPAuth(authSvc, db))
	calShareGroup.Get("/:id/shares", sharingHandler.ListCalendarShares)
	calShareGroup.Post("/:id/shares", sharingHandler.CreateCalendarShare)
	calShareGroup.Delete("/:id/shares/:user_id", sharingHandler.DeleteCalendarShare)

	// Calendar BYO SMTP configuration
	smtpHandler := smtp.NewHandler(db, cfg.Auth.JWTSecret)
	calShareGroup.Get("/:id/smtp", smtpHandler.GetSMTP)
	calShareGroup.Put("/:id/smtp", smtpHandler.UpsertSMTP)
	calShareGroup.Post("/:id/smtp/test", smtpHandler.TestSMTP)
	calShareGroup.Delete("/:id/smtp", smtpHandler.DeleteSMTP)

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

	// Public web RSVP for email recipients (no login). It lives outside /api/scheduling because
	// that group's auth middleware runs for every path under it. GET shows a page, POST records.
	app.Get("/api/public/rsvp", rsvpPageHeaders, schedulingHandler.PublicWebRSVP)
	app.Post("/api/public/rsvp", rsvpPageHeaders, schedulingHandler.PublicWebRSVP)

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
	// Onboarding: X-API-Key (onboards users of the key's domain) or AirSend JWT (own email only).
	onboardAuth := middleware.OnboardAuth(authSvc, db)
	onboardHandler := createUserHandler(db, true)
	app.Post("/api/calendar/onboard", onboardAuth, onboardHandler)
	app.Post("/api/users/onboard", onboardAuth, onboardHandler)

	// Onboarding API keys, managed by the signed-in user for their own email
	apiKeyGroup := app.Group("/api/api-keys", middleware.JMAPAuth(authSvc, db))
	apiKeyGroup.Get("/", listOwnAPIKeysHandler(db))
	apiKeyGroup.Post("/", createOwnAPIKeyHandler(db))
	apiKeyGroup.Delete("/:id", revokeOwnAPIKeyHandler(db))

	adminGroup := app.Group("/api/admin", middleware.APIKeyAuth(cfg.Auth.AdminAPIKey))
	adminGroup.Post("/users", createUserHandler(db, false))
	adminGroup.Get("/api-keys", adminListAPIKeysHandler(db))
	adminGroup.Post("/api-keys", adminCreateAPIKeyHandler(db))
	adminGroup.Delete("/api-keys/:id", adminRevokeAPIKeyHandler(db))

	// ============================================================
	// Graceful Shutdown
	// ============================================================
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Listen returns as soon as the listener closes, so main waits on shutdownDone for in-flight
	// requests to finish before the deferred db/queue Close calls run.
	shutdownDone := make(chan struct{})
	go func() {
		<-quit
		log.Println("Shutting down server...")
		// End the long-lived SSE streams, stop taking requests, then the background work.
		stopHub()
		if err := app.ShutdownWithTimeout(cfg.Server.ShutdownTimeout); err != nil {
			log.Printf("Server forced shutdown: %v", err)
		}
		cancel()
		worker.Stop()
		close(shutdownDone)
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
	<-shutdownDone
}

// rsvpPageHeaders lets the public RSVP pages use their inline styles and post their form
// (the global Content-Security-Policy is default-src 'self', which blocks inline CSS).
func rsvpPageHeaders(c *fiber.Ctx) error {
	c.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	c.Set("Cache-Control", "no-store")
	return c.Next()
}

// proxyHeader is the header carrying the client IP set by the reverse proxy (PROXY_HEADER,
// default X-Forwarded-For).
func proxyHeader() string {
	if h := strings.TrimSpace(os.Getenv("PROXY_HEADER")); h != "" {
		return h
	}
	return fiber.HeaderXForwardedFor
}

// trustedProxies lists the proxy IPs/CIDRs whose PROXY_HEADER is believed (TRUSTED_PROXIES,
// comma-separated; default loopback and private ranges, which covers a Docker bridge).
func trustedProxies() []string {
	if v := strings.TrimSpace(os.Getenv("TRUSTED_PROXIES")); v != "" {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return []string{"127.0.0.1", "::1", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
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
// scoped=true (the public onboarding routes, behind middleware.OnboardAuth) limits who may be
// onboarded: an API key only users of its domain, a JWT only its own email. scoped=false is the
// admin route (ADMIN_API_KEY), which may onboard any email.
func createUserHandler(db *store.Store, scoped bool) fiber.Handler {
	return func(c *fiber.Ctx) error {
		var req struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		_ = c.BodyParser(&req)
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		if scoped {
			if own, _ := c.Locals("onboard_email").(string); own != "" {
				if req.Email != "" && req.Email != own {
					return response.Error(c, 403, "a token can only onboard its own email")
				}
				req.Email = own
				if req.Name == "" {
					req.Name, _ = c.Locals("onboard_name").(string)
				}
			} else if domain, _ := c.Locals("onboard_domain").(string); domain != "" {
				if req.Email != "" && store.EmailDomain(req.Email) != domain {
					return response.Error(c, 403, "this API key can only onboard users of "+domain)
				}
			} else {
				return response.Error(c, 401, "X-API-Key or Bearer token required")
			}
		}

		if req.Email == "" || store.EmailDomain(req.Email) == "" {
			return response.Error(c, 400, "a valid email is required")
		}

		// Look up or add the account in CalDev's own table (one account per email; an Airsend
		// mailbox keeps its Airsend id, anything else becomes an external account)
		user, err := db.EnsureUserByEmail(c.UserContext(), req.Email, req.Name)
		if errors.Is(err, store.ErrCalendarDisabled) {
			return response.Error(c, 409, "this email is an Airsend mailbox whose calendar is not enabled; enable the calendar in Airsend")
		}
		if err != nil {
			log.Printf("onboard: provision %s: %v", req.Email, err)
			return response.Error(c, 500, "failed to provision user")
		}
		if !strings.EqualFold(user.Status, "ACTIVE") {
			return response.Error(c, 403, "account is not active")
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
