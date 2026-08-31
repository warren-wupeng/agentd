package main

// The workflow CLI (M8): run a definition file against live agents,
// watch node states until the run parks; status prints an existing run.

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/config"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/tools"
	"github.com/warren-wupeng/agentd/internal/workflow"
)

func runWorkflowCmd(args []string) error {
	fs := flag.NewFlagSet("workflow", flag.ContinueOnError)
	file := fs.String("file", "", "path to a workflow definition JSON")
	agentID := fs.String("agent", "", "agent id (UUID) substituted for AGENT_ID in the template")
	spec := fs.String("spec", "", "the spec injected as {{spec}} (software-dev template)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || *agentID == "" {
		return agentderr.InvalidInput(
			"workflow run requires -file and -agent",
			"example: agentd-server workflow -file templates/software-dev.json -agent <uuid> -spec 'build a fib function'")
	}
	if _, err := uuid.Parse(*agentID); err != nil {
		return agentderr.InvalidInput("agent must be a UUID", "copy it from GET /v1/agents")
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		return agentderr.Wrap(agentderr.CodeInvalidInput, err,
			"cannot read workflow file "+*file, "check the path")
	}
	// template substitution: AGENT_ID placeholder + user variables
	body := strings.ReplaceAll(string(raw), "AGENT_ID", *agentID)
	if *spec != "" {
		body = strings.ReplaceAll(body, "{{spec}}", *spec)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	sb, err := sandboxForEval(cfg)
	if err != nil {
		return err
	}
	ex, err := workflow.NewExecutor(ctx, cfg.DatabaseURL, st, sb, defaultLogger(), harness.NewNative(&loop.Deps{
		Store:        st,
		Model:        model.NewOpenAI(cfg.ModelBaseURL, cfg.ModelAPIKey),
		Sandbox:      sb,
		Policy:       policy.NewStatic(),
		Registry:     tools.NewRegistry(),
		MaxSteps:     cfg.LoopMaxSteps,
		ModelRetries: cfg.LoopRetries,
	}))
	if err != nil {
		return err
	}
	defer ex.Close()

	run, err := ex.Start(ctx, []byte(body))
	if err != nil {
		return err
	}
	fmt.Printf("started workflow %q run %s\n", run.Name, run.ID)

	deadline := time.Now().Add(*timeout)
	for {
		cur, err := ex.Get(ctx, uuid.MustParse(run.ID))
		if err != nil {
			return err
		}
		printNodeStates(cur)
		if cur.Status != "running" {
			fmt.Printf("\nworkflow %s\n", cur.Status)
			return nil
		}
		if time.Now().After(deadline) {
			return agentderr.New(agentderr.CodeConflict, "workflow timed out",
				"the run continues server-side; poll GET /v1/workflows/"+run.ID)
		}
		time.Sleep(2 * time.Second)
		fmt.Println("---")
	}
}

func runWorkflowStatus(args []string) error {
	if len(args) < 1 {
		return agentderr.InvalidInput("workflow status requires a run id", "copy it from the run output")
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return agentderr.InvalidInput("run id must be a UUID", "copy it from the run output")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	sb, err := sandboxForEval(cfg)
	if err != nil {
		return err
	}
	ex, err := workflow.NewExecutor(ctx, cfg.DatabaseURL, st, sb, defaultLogger(), harness.NewNative(&loop.Deps{
		Store: st, Model: model.NewOpenAI(cfg.ModelBaseURL, cfg.ModelAPIKey), Sandbox: sb,
		Policy: policy.NewStatic(), Registry: tools.NewRegistry(),
	}))
	if err != nil {
		return err
	}
	defer ex.Close()
	run, err := ex.Get(ctx, id)
	if err != nil {
		return err
	}
	printNodeStates(run)
	fmt.Printf("\nworkflow %s\n", run.Status)
	return nil
}

func printNodeStates(run *workflow.Run) {
	for _, st := range run.NodeStates {
		line := fmt.Sprintf("  %-10s %-10s", st.ID, st.Status)
		if st.SessionID != "" {
			line += " session " + st.SessionID[:8]
		}
		if st.Error != "" {
			line += " err: " + truncateStr(st.Error, 80)
		}
		fmt.Println(line)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func defaultLogger() *slog.Logger { return slog.Default() }
