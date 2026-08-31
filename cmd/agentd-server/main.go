// Command agentd-server is the agentd control-plane API.
//
// Usage:
//
//	agentd-server serve     Run the HTTP API (default).
//	agentd-server migrate   Apply database migrations and exit.
//
//	agentd-server eval     Score agent versions on a dataset (M7).
//	agentd-server eval-export  Mine a case stub from a session trace.
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
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/hub"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/mcp"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/tools"
	"github.com/warren-wupeng/agentd/internal/vault"
	"github.com/warren-wupeng/agentd/internal/workflow"
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
	case "eval":
		return runEval(args[1:])
	case "eval-export":
		return runEvalExport(args[1:])
	case "workflow":
		return runWorkflowCmd(args[1:])
	case "workflow-status":
		return runWorkflowStatus(args[1:])
	default:
		return agentderr.InvalidInput("unknown command "+cmd, "valid commands: serve, migrate, eval, eval-export, workflow, workflow-status")
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
		case "e2b":
			var e2b *sandbox.E2B
			e2b, err = sandbox.NewE2B("", cfg.E2BAPIKey, cfg.E2BTemplate)
			sb = e2b
			slog.Warn("e2b sandbox provider is experimental — wire contract not yet live-validated")
		default:
			sb, err = sandbox.NewExec(cfg.SandboxBase)
		}
		if err != nil {
			return err
		}
		sb.SetPolicy(sandbox.Policy{Egress: sandbox.Egress(cfg.SandboxEgress)})
		listener, err := store.NewEventListener(ctx, cfg.DatabaseURL)
		if err != nil {
			return fmt.Errorf("event listener: %w", err)
		}
		defer func() { _ = listener.Close() }()

		// The credential plane (M6): optional until VAULT_MASTER_KEY is
		// set — the API degrades to no vault endpoints, nothing half-works.
		registryOpts := []tools.RegistryOption{}
		if cfg.VaultMasterKey != "" {
			mkey, err := vault.NewMasterKey(cfg.VaultMasterKey)
			if err != nil {
				return err
			}
			vlt, err := vault.New(ctx, cfg.DatabaseURL, mkey)
			if err != nil {
				return fmt.Errorf("vault: %w", err)
			}
			defer vlt.Close()
			mcpSvc, err := mcp.New(ctx, cfg.DatabaseURL, vlt)
			if err != nil {
				return fmt.Errorf("mcp: %w", err)
			}
			defer mcpSvc.Close()
			opts = append(opts, api.WithVaultMCP(vlt, mcpSvc))
			registryOpts = append(registryOpts, tools.WithMCP(mcpSvc))
			slog.Info("credential plane enabled", "vault", "aes-256-gcm", "mcp_proxy", "on")
		} else {
			slog.Warn("VAULT_MASTER_KEY not set — vault + MCP credential proxy disabled")
		}
		deltas := hub.New()
		deps := &loop.Deps{
			Store:        st,
			Model:        model.NewOpenAI(cfg.ModelBaseURL, cfg.ModelAPIKey),
			Sandbox:      sb,
			Policy:       policy.NewStatic(),
			Registry:     tools.NewRegistry(registryOpts...),
			MaxSteps:     cfg.LoopMaxSteps,
			ModelRetries: cfg.LoopRetries,
			Log:          slog.Default(),
			Deltas:       deltas,
		}

		// The harness seam (ADR-004): native is the reference runtime;
		// OpenCode registers when its server URL is configured. The
		// dispatcher routes each session by its harness column.
		pol := policy.NewStatic()
		hs := []harness.Harness{harness.NewNative(deps)}
		if cfg.OpenCodeURL != "" {
			hs = append(hs, harness.NewOpenCode(cfg.OpenCodeURL, st, pol))
			slog.Info("opencode harness registered", "url", cfg.OpenCodeURL, "status", "experimental")
		}
		dispatcher := harness.NewDispatcher(ctx, st, slog.Default(), hs...)
		opts = append(opts,
			api.WithRunner(dispatcher),
			api.WithStream(deltas, listener),
			api.WithHarnesses(dispatcher.Names()))

		// The workflow executor (M8): same harnesses, same sandboxes.
		wfExec, err := workflow.NewExecutor(ctx, cfg.DatabaseURL, st, sb, slog.Default(), hs...)
		if err != nil {
			return fmt.Errorf("workflow executor: %w", err)
		}
		defer wfExec.Close()
		opts = append(opts, api.WithWorkflow(wfExec))
		// ctx cancellation (SIGINT/SIGTERM) stops in-flight actors at
		// their next checkpoint; Wait drains them before exit.
		defer dispatcher.Wait()

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
