package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Exec runs commands as local child processes in a per-session workdir.
// ZERO isolation — it exists so dev boxes and CI without a Docker daemon
// still run the full loop. Never point it at hostile workloads; that is
// what the docker provider and ADR-001's e2b provider are for.
type Exec struct {
	base string
	pol  Policy

	mu      sync.Mutex
	handles map[uuid.UUID]*execHandle
}

// NewExec creates the provider; workdirs land under base/<session>/workspace.
func NewExec(base string) (*Exec, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox base %s: %w", base, err)
	}
	return &Exec{base: base, pol: DefaultPolicy(), handles: map[uuid.UUID]*execHandle{}}, nil
}

func (e *Exec) Handle(sessionID uuid.UUID) (Handle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.handles[sessionID]; ok {
		return h, nil
	}
	workdir := filepath.Join(e.base, sessionID.String(), "workspace")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, fmt.Errorf("create workdir for session %s: %w", sessionID, err)
	}
	h := &execHandle{sessionID: sessionID, workdir: workdir}
	e.handles[sessionID] = h
	return h, nil
}

type execHandle struct {
	sessionID uuid.UUID
	workdir   string
}

func (e *Exec) SetPolicy(p Policy) { e.pol = p }
func (e *Exec) Policy() Policy     { return e.pol }

func (h *execHandle) SessionID() uuid.UUID { return h.sessionID }

// CanEnforceEgress is FALSE and always will be: no root, no namespaces.
// The escape suite asserts the consequences as expected behavior —
// the dev tier's honesty is part of the contract.
func (h *execHandle) CanEnforceEgress() bool { return false }
func (h *execHandle) Workdir() string        { return h.workdir }

func (h *execHandle) ResolvePath(modelPath string) (string, error) {
	return resolveUnder(h.workdir, modelPath)
}

func (h *execHandle) Exec(ctx context.Context, command string, timeout time.Duration) (ExecResult, error) {
	if command == "" {
		return ExecResult{}, fmt.Errorf("empty command")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = h.workdir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		so, trunc := truncateBytes(stdout.Bytes())
		se, _ := truncateBytes(stderr.Bytes())
		return ExecResult{
			Command: command, ExitCode: -1, Stdout: so, Stderr: se,
			Truncated: trunc, Duration: time.Since(start),
		}, fmt.Errorf("command timed out after %s", timeout)
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ee, ok := err.(*exec.ExitError); ok {
			exitErr = ee
		}
		if exitErr == nil {
			// spawn failure — infrastructure, not data
			return ExecResult{}, fmt.Errorf("spawn command: %w", err)
		}
		exitCode = exitErr.ExitCode()
	}
	so, trunc1 := truncateBytes(stdout.Bytes())
	se, trunc2 := truncateBytes(stderr.Bytes())
	return ExecResult{
		Command: command, ExitCode: exitCode, Stdout: so, Stderr: se,
		Truncated: trunc1 || trunc2, Duration: time.Since(start),
	}, nil
}

// resolveUnder anchors p under root, rejecting absolute paths and escapes.
func resolveUnder(root, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("absolute paths are not allowed (%q); use a path relative to the workspace", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the workspace", p)
	}
	return filepath.Join(root, clean), nil
}

// ReadFile/WriteFile: host-side fs on the anchored path (dev tier).
func (h *execHandle) ReadFile(_ context.Context, path string) ([]byte, error) {
	full, err := h.ResolvePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (h *execHandle) WriteFile(_ context.Context, path string, content []byte) error {
	full, err := h.ResolvePath(path)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(full); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(full, content, 0o644)
}
