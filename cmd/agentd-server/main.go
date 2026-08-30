// Command agentd-server is the agentd control-plane API.
//
// Usage:
//
//	agentd-server serve     Run the HTTP API (default).
//	agentd-server migrate   Apply database migrations and exit.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/api"
	"github.com/warren-wupeng/agentd/internal/config"
	"github.com/warren-wupeng/agentd/internal/hub"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/tools"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agentd-server:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel})))

	switch cmd {
	case "migrate":
		if err := store.Migrate(cfg.DatabaseURL, "up"); err != nil {
			return err
		}
		slog.Info("migrations applied")
		return nil
	case "serve":
		return serve(cfg)
	default:
		return agentderr.InvalidInput("unknown command "+cmd, "valid commands: serve, migrate")
	}
}

func serve(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	var opts []api.Option
	if cfg.ModelBaseURL != "" {
		var sb sandbox.Provider
		switch cfg.SandboxProv {
		case "docker":
			sb, err = sandbox.NewDocker(cfg.SandboxBase, "")
		default:
			sb, err = sandbox.NewExec(cfg.SandboxBase)
		}
		if err != nil {
			return err
		}
		listener, err := store.NewEventListener(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("event listener: %w", err)
		}
		defer func() { _ = listener.Close() }()
		deltas := hub.New()
		deps := &loop.Deps{
			Store:        st,
			Model:        model.NewOpenAI(cfg.ModelBaseURL, cfg.ModelAPIKey),
			Sandbox:      sb,
			Policy:       policy.NewStatic(),
			Registry:     tools.NewRegistry(),
			MaxSteps:     cfg.LoopMaxSteps,
			ModelRetries: cfg.LoopRetries,
			Log:          slog.Default(),
			Deltas:       deltas,
		}
		runner := loop.NewRunner(ctx, deps)
		opts = append(opts, api.WithRunner(runner), api.WithStream(deltas, listener))
		// ctx cancellation (SIGINT/SIGTERM) stops in-flight actors at
		// their next checkpoint; Wait drains them before exit.
		defer runner.Wait()

		slog.Info("native loop enabled",
			"sandbox", cfg.SandboxProv, "model_base_url", cfg.ModelBaseURL)
	} else {
		slog.Warn("MODEL_BASE_URL not set — CRUD-only process; native sessions will park with retries_exhausted")
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewHandler(st, opts...),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
