package main

// The eval CLI (M7): score agent versions on a dataset, print the
// version-compare diff; mine a case stub from an existing session's
// trace (trace → dataset). Eval runs the same machinery users do —
// real harness, real model, real sandboxes.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/config"
	"github.com/warren-wupeng/agentd/internal/eval"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/tools"
)

func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	datasetPath := fs.String("dataset", "", "path to a dataset JSON file")
	agentIDRaw := fs.String("agent", "", "agent id (UUID)")
	versionsRaw := fs.String("versions", "", "comma-separated agent versions to compare, e.g. 1,2")
	outPath := fs.String("out", "", "optional path for the JSON reports")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *datasetPath == "" || *agentIDRaw == "" || *versionsRaw == "" {
		return agentderr.InvalidInput(
			"eval requires -dataset, -agent, and -versions",
			"example: agentd-server eval -dataset evals/answer.json -agent 490f926a-... -versions 1,2")
	}
	agentID, err := uuid.Parse(*agentIDRaw)
	if err != nil {
		return agentderr.InvalidInput("agent must be a UUID", "copy it from GET /v1/agents")
	}
	var versions []int
	for _, raw := range strings.Split(*versionsRaw, ",") {
		var v int
		if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &v); err != nil || v < 1 {
			return agentderr.InvalidInput(
				"versions must be comma-separated positive integers, got "+*versionsRaw,
				"example: 1,2")
		}
		versions = append(versions, v)
	}
	if len(versions) < 1 || len(versions) > 2 {
		return agentderr.InvalidInput(
			"eval compares one or two versions",
			"one version prints its report; two prints the diff — that's the point")
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

	raw, err := os.ReadFile(*datasetPath)
	if err != nil {
		return agentderr.Wrap(agentderr.CodeInvalidInput, err,
			"cannot read dataset file "+*datasetPath, "check the path")
	}
	d, err := eval.ParseDataset(raw)
	if err != nil {
		return agentderr.Wrap(agentderr.CodeInvalidInput, err,
			"dataset file is not valid", "see docs/exec-plans/completed/M7-eval-harness.md for the shape")
	}

	sb, err := sandboxForEval(cfg)
	if err != nil {
		return err
	}
	runner := &eval.Runner{
		Store: st,
		Harness: harness.NewNative(&loop.Deps{
			Store:        st,
			Model:        model.NewOpenAI(cfg.ModelBaseURL, cfg.ModelAPIKey),
			Sandbox:      sb,
			Policy:       policy.NewStatic(),
			Registry:     tools.NewRegistry(),
			MaxSteps:     cfg.LoopMaxSteps,
			ModelRetries: cfg.LoopRetries,
		}),
		Scorer: eval.NewScorer(sb),
	}

	var reports []*eval.VersionReport
	for _, v := range versions {
		report, err := runner.RunDataset(ctx, *d, agentID, v)
		if err != nil {
			return err
		}
		reports = append(reports, report)
	}

	if len(reports) == 1 {
		b, _ := json.MarshalIndent(reports[0], "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Print(eval.Compare(reports[0], reports[1]))
	}
	if *outPath != "" {
		b, _ := json.MarshalIndent(reports, "", "  ")
		if err := os.WriteFile(*outPath, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "reports written to %s\n", *outPath)
	}
	return nil
}

func runEvalExport(args []string) error {
	fs := flag.NewFlagSet("eval-export", flag.ContinueOnError)
	sessionRaw := fs.String("session", "", "session id (UUID) whose trace to mine")
	caseID := fs.String("id", "", "optional case id for the mined case")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sessionRaw == "" {
		return agentderr.InvalidInput("eval-export requires -session",
			"example: agentd-server eval-export -session 8ce58df2-... -id deploy-case")
	}
	sessionID, err := uuid.Parse(*sessionRaw)
	if err != nil {
		return agentderr.InvalidInput("session must be a UUID", "copy it from GET /v1/sessions")
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

	c, err := eval.ExportCase(ctx, st, sessionID, *caseID)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintln(os.Stderr, "rubric is empty — author it before running eval")
	return nil
}

func sandboxForEval(cfg *config.Config) (sandbox.Provider, error) {
	switch cfg.SandboxProv {
	case "docker":
		return sandbox.NewDocker(cfg.SandboxBase, "")
	case "e2b":
		return sandbox.NewE2B("", cfg.E2BAPIKey, cfg.E2BTemplate)
	default:
		return sandbox.NewExec(cfg.SandboxBase)
	}
}
