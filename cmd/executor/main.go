package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/frontend"
	"github.com/JetManiack/mcp-sandbox/internal/health"
	"github.com/JetManiack/mcp-sandbox/internal/humanauth"
	"github.com/JetManiack/mcp-sandbox/internal/mcpserver"
	"github.com/JetManiack/mcp-sandbox/internal/restapi"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
	sandboxtools "github.com/JetManiack/mcp-sandbox/internal/tools/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/workerhub"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

// version is stamped at build time by the Makefile (-X main.version=...).
var version = "dev"

func newRootCommand() *cli.Command {
	return &cli.Command{
		Name:    "executor",
		Usage:   "MCP server giving AI agents a jailed filesystem and command execution on worker pods, with a live read-only terminal for humans",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "listen-addr",
				Value:   ":8080",
				Usage:   "HTTP server listen address",
				Sources: cli.EnvVars("LISTEN_ADDR"),
			},
			&cli.StringFlag{
				Name:    "db-dsn",
				Value:   "data/executor.db",
				Usage:   "SQLite file path, or a postgres:// connection string",
				Sources: cli.EnvVars("DB_DSN"),
			},
			&cli.StringFlag{
				Name:    "worker-token",
				Usage:   "shared secret workers present when they dial /worker; without it the worker endpoint refuses every connection and nothing can execute",
				Sources: cli.EnvVars("WORKER_TOKEN"),
			},
			&cli.DurationFlag{
				Name:  "audit-retention",
				Value: 7 * 24 * time.Hour,
				// Rotation is by age rather than by row count, so the promise an
				// operator makes is one they can state: we keep a week. Zero keeps
				// everything, which is a decision rather than a default — an
				// unbounded table is fine right up until it is not.
				Usage:   "how long the action journal is kept before rows are pruned; 0 keeps everything",
				Sources: cli.EnvVars("AUDIT_RETENTION"),
			},
			&cli.BoolFlag{
				Name:    "auth-stub",
				Usage:   "use a fixed, always-admin test identity instead of real Keycloak/OIDC auth — local development only, never set this in a real deployment",
				Sources: cli.EnvVars("AUTH_STUB"),
			},
			&cli.StringFlag{
				Name:    "oidc-issuer",
				Usage:   "Keycloak realm issuer URL, e.g. https://keycloak.internal/realms/executor",
				Sources: cli.EnvVars("OIDC_ISSUER"),
			},
			&cli.StringFlag{
				Name:    "oidc-client-id",
				Usage:   "client ID of the dedicated OIDC client configured in Keycloak for this app",
				Sources: cli.EnvVars("OIDC_CLIENT_ID"),
			},
			&cli.StringFlag{
				Name:    "oidc-client-secret",
				Usage:   "client secret of the dedicated OIDC client configured in Keycloak for this app",
				Sources: cli.EnvVars("OIDC_CLIENT_SECRET"),
			},
			&cli.StringFlag{
				Name:    "public-url",
				Usage:   "this server's externally-reachable base URL (used to build the OIDC redirect URI <public-url>/auth/callback — must match what's configured on the Keycloak client)",
				Sources: cli.EnvVars("PUBLIC_URL"),
			},
			&cli.StringFlag{
				Name:    "admin-group",
				Value:   "admins",
				Usage:   "Keycloak group (from the ID token's groups claim) whose members may block sandboxes and manage agent tokens; everyone else gets read-only access",
				Sources: cli.EnvVars("ADMIN_GROUP"),
			},
			&cli.StringFlag{
				Name:    "session-encryption-key",
				Usage:   "base64-encoded 32-byte key used to encrypt session refresh tokens at rest (generate one with: openssl rand -base64 32)",
				Sources: cli.EnvVars("SESSION_ENCRYPTION_KEY"),
			},
			&cli.IntFlag{
				Name:    "stream-buffer-bytes",
				Value:   stream.DefaultStreamBufferBytes,
				Usage:   "how much recent terminal output is retained per sandbox for replay to a connecting watcher",
				Sources: cli.EnvVars("STREAM_BUFFER_BYTES"),
			},
			&cli.BoolFlag{
				Name:    "devel",
				Usage:   "serve static assets from disk instead of the embedded snapshot",
				Sources: cli.EnvVars("DEVEL"),
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// This process no longer executes anything. The bus retains terminal
			// output and fans it out to browsers; the hub routes work to the
			// workers that do the executing.
			bus := stream.NewBroadcaster(cmd.Int("stream-buffer-bytes"))
			hub := workerhub.New(bus)

			if cmd.String("worker-token") == "" {
				slog.Warn("no --worker-token configured: workers cannot connect, so every tool call will report that no worker is available")
			}

			dsn := cmd.String("db-dsn")

			db, err := storage.Open(dsn)
			if err != nil {
				slog.Error("storage.Open failed at boot; serving /livez and /readyz (not ready) while retrying in the background instead of exiting", "error", err)
				return serveDegradedUntilReady(ctx, cmd, bus, hub, dsn, err)
			}
			return serveReady(ctx, cmd, bus, hub, db)
		},
	}
}

// buildAppHandler builds every route this app serves once db is available —
// MCP, the REST API and the static frontend — plus the ReadyChecker that
// reflects db actually being usable. It's shared between the immediate-ready
// startup path (serveReady) and the recovers-after-retry path
// (serveDegradedUntilReady), so both build identical routes.
func buildAppHandler(ctx context.Context, cmd *cli.Command, bus *stream.Broadcaster, hub *workerhub.Hub, db *gorm.DB) (http.Handler, health.ReadyChecker, error) {
	var authProvider humanauth.Provider
	var oidcHandlers humanauth.OIDCHandlers
	useOIDC := !cmd.Bool("auth-stub")

	if useOIDC {
		if cmd.String("oidc-issuer") == "" || cmd.String("oidc-client-id") == "" || cmd.String("oidc-client-secret") == "" || cmd.String("public-url") == "" || cmd.String("session-encryption-key") == "" {
			return nil, health.ReadyChecker{}, errors.New("OIDC auth requires --oidc-issuer, --oidc-client-id, --oidc-client-secret, --public-url, and --session-encryption-key (or pass --auth-stub for local development)")
		}
		encryptionKey, err := base64.StdEncoding.DecodeString(cmd.String("session-encryption-key"))
		if err != nil {
			return nil, health.ReadyChecker{}, fmt.Errorf("--session-encryption-key must be valid base64: %w", err)
		}
		if len(encryptionKey) != 32 {
			return nil, health.ReadyChecker{}, fmt.Errorf("--session-encryption-key must decode to exactly 32 bytes, got %d (generate one with: openssl rand -base64 32)", len(encryptionKey))
		}
		provider, handlers, err := humanauth.NewOIDCHandlers(ctx, db, humanauth.OIDCConfig{
			Issuer:        cmd.String("oidc-issuer"),
			ClientID:      cmd.String("oidc-client-id"),
			ClientSecret:  cmd.String("oidc-client-secret"),
			PublicURL:     cmd.String("public-url"),
			AdminGroup:    cmd.String("admin-group"),
			EncryptionKey: encryptionKey,
		})
		if err != nil {
			return nil, health.ReadyChecker{}, fmt.Errorf("configure OIDC: %w", err)
		}
		authProvider = provider
		oidcHandlers = handlers
	} else {
		// ⚠️ StubProvider authenticates every request as a fixed always-admin
		// identity with no credential check at all. Only reachable via
		// --auth-stub, which must never be set outside local development: this
		// UI streams the live terminal of every sandbox and can kill the
		// processes running in them.
		authProvider = humanauth.StubProvider{}
		slog.Warn("human auth running in STUB mode — do not deploy to production")
	}

	frontendFS, err := frontend.FS(cmd.Bool("devel"))
	if err != nil {
		return nil, health.ReadyChecker{}, fmt.Errorf("load frontend assets: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, health.ReadyChecker{}, fmt.Errorf("get pooled db handle: %w", err)
	}
	readyChecker := health.ReadyChecker{
		Ping:            sqlDB.PingContext,
		MigrationsReady: true,
		OIDCReady:       true,
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.Handler(db, version, []mcpserver.ToolRegistrar{
		sandboxtools.NewRegistrar(hub, db),
	}))
	// Workers dial in here. Mounted on the same listener as everything else, so a
	// deployment can put it behind the same ingress or, better, keep it on an
	// internal Service the agents' ingress never reaches.
	mux.Handle(workerproto.Path, hub.Handler(cmd.String("worker-token")))
	mux.Handle("/api/", http.StripPrefix("/api", restapi.NewRouter(restapi.Options{
		DB:           db,
		Bus:          bus,
		Hub:          hub,
		AuthProvider: authProvider,
	})))
	if useOIDC {
		mux.HandleFunc("/auth/login", oidcHandlers.Login)
		mux.HandleFunc("/auth/callback", oidcHandlers.Callback)
		mux.HandleFunc("/auth/logout", oidcHandlers.Logout)
	}
	mux.Handle("/", http.FileServer(frontendFS))

	return mux, readyChecker, nil
}

// newServer builds the *http.Server this app always serves with, regardless of
// which startup path constructed handler.
func newServer(addr string, handler http.Handler) *http.Server {
	// No WriteTimeout: the terminal stream is a long-lived connection, and any
	// finite write deadline set here would sever it mid-session. Per-write
	// deadlines belong in the stream handler, where they apply to a single
	// frame rather than the whole connection.
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// serveReady is the happy-path startup: db is already open, so every route
// builds immediately and the server serves fully-ready from the first accepted
// connection.
func serveReady(ctx context.Context, cmd *cli.Command, bus *stream.Broadcaster, hub *workerhub.Hub, db *gorm.DB) error {
	appHandler, readyChecker, err := buildAppHandler(ctx, cmd, bus, hub, db)
	if err != nil {
		return err
	}

	go pruneAuditPeriodically(ctx, db, cmd.Duration("audit-retention"))

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", health.Livez)
	mux.HandleFunc("/readyz", readyChecker.Readyz)
	mux.Handle("/", appHandler)

	server := newServer(cmd.String("listen-addr"), mux)
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("starting server", "addr", server.Addr, "version", version)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		return shutdown(server)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// serveDegradedUntilReady is reached only when storage.Open fails at boot.
// Kubernetes needs a live pod to probe — a crash loop only delays recovery and
// never fixes an outage the app itself can't fix — so this starts listening
// immediately with /livez healthy and /readyz not-ready, and retries
// storage.Open with capped exponential backoff in the background. Once it
// succeeds the real routes are swapped in on the same listener, with no
// restart, and control folds into the same serve/shutdown loop serveReady uses.
func serveDegradedUntilReady(ctx context.Context, cmd *cli.Command, bus *stream.Broadcaster, hub *workerhub.Hub, dsn string, firstErr error) error {
	var mu sync.RWMutex
	checker := health.ReadyChecker{
		Ping:            func(context.Context) error { return firstErr },
		MigrationsReady: false,
		OIDCReady:       false,
	}
	var appHandler atomic.Pointer[http.Handler]

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", health.Livez)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		c := checker
		mu.RUnlock()
		c.Readyz(w, r)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := appHandler.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting up: database not yet available", http.StatusServiceUnavailable)
	}))

	server := newServer(cmd.String("listen-addr"), mux)
	serveErr := make(chan error, 1)
	go func() {
		slog.Info("starting server (degraded: database not yet available)", "addr", server.Addr, "version", version)
		serveErr <- server.ListenAndServe()
	}()

	dbReady := make(chan *gorm.DB, 1)
	go retryOpenUntilReady(ctx, dsn, &mu, &checker, dbReady)

	for {
		select {
		case <-ctx.Done():
			return shutdown(server)
		case err := <-serveErr:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case db := <-dbReady:
			handler, readyChecker, err := buildAppHandler(ctx, cmd, bus, hub, db)
			if err != nil {
				if shutdownErr := shutdown(server); shutdownErr != nil {
					slog.Error("shutdown after failed route build", "error", shutdownErr)
				}
				return err
			}
			h := handler
			appHandler.Store(&h)
			mu.Lock()
			checker = readyChecker
			mu.Unlock()
			slog.Info("database became available; now serving normally")
		}
	}
}

// retryOpenUntilReady retries storage.Open(dsn) with capped exponential backoff
// until it succeeds or ctx is done, updating checker's Ping (via mu) with the
// latest failure after every attempt so /readyz always names the current reason
// rather than the boot-time one. It sends the opened *gorm.DB on ready and
// returns; it never retries again after a success.
func retryOpenUntilReady(ctx context.Context, dsn string, mu *sync.RWMutex, checker *health.ReadyChecker, ready chan<- *gorm.DB) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		db, err := storage.Open(dsn)
		if err == nil {
			ready <- db
			return
		}

		slog.Error("storage.Open retry failed", "error", err)
		mu.Lock()
		checker.Ping = func(context.Context) error { return err }
		mu.Unlock()

		backoff = min(backoff*2, maxBackoff)
	}
}

func shutdown(server *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

func main() {
	// SIGTERM is how a container runtime asks for a graceful stop; without
	// this, in-flight commands are cut mid-response on every rollout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand().Run(ctx, os.Args); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// auditPruneInterval is how often the journal is trimmed. Hourly rather than at
// startup only, because a server that stays up for a month would otherwise honour
// its retention promise once.
const auditPruneInterval = time.Hour

// pruneAuditPeriodically deletes journal rows past their retention until ctx ends.
//
// Failures are logged and the loop continues: an unprunable journal is a disk
// problem to notice, not a reason to stop serving. It runs on every replica,
// which is harmless — deleting rows already deleted is a no-op.
func pruneAuditPeriodically(ctx context.Context, db *gorm.DB, retention time.Duration) {
	if db == nil || retention <= 0 {
		return
	}

	prune := func() {
		deleted, err := storage.PruneAudit(db, retention)
		if err != nil {
			slog.Error("could not prune the action journal", "error", err)
			return
		}
		if deleted > 0 {
			slog.Info("pruned the action journal", "rows", deleted, "retention", retention)
		}
	}

	prune()
	ticker := time.NewTicker(auditPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
