// E2B provider — ADR-001's production tier (Firecracker microVMs).
// EXPERIMENTAL: the wire shapes below encode E2B's documented API as we
// understand it and are pinned by the fake server in e2b_test.go —
// until validated against live E2B (E2B_API_KEY + a real template),
// treat this file as the single point where live drift gets fixed.
//
// Hand-rolled instead of the e2b-go SDK on purpose: the SDK owns
// transport and auth and cannot be pointed at a fake; a thin client on
// the documented REST surface keeps the wire contract testable.
package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const DefaultE2BTemplate = "base"

// E2B drives sandboxes on an E2B deployment (cloud or self-hosted).
// Sandboxes are created per session and killed on Close; the workdir
// lives INSIDE the microVM (E2B's filesystem), so file tools go over
// the API rather than the host filesystem.
type E2B struct {
	baseURL  string
	apiKey   string
	template string
	pol      Policy

	client *http.Client

	mu      sync.Mutex
	handles map[uuid.UUID]*e2bHandle
}

func NewE2B(baseURL, apiKey, template string) (*E2B, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("E2B_API_KEY is required for the e2b provider")
	}
	if template == "" {
		template = DefaultE2BTemplate
	}
	if baseURL == "" {
		baseURL = "https://api.e2b.dev"
	}
	return &E2B{
		baseURL:  strings.TrimRight(baseURL, "/"),
		apiKey:   apiKey,
		template: template,
		pol:      DefaultPolicy(),
		client:   &http.Client{Timeout: 3 * time.Minute},
		handles:  map[uuid.UUID]*e2bHandle{},
	}, nil
}

func (e *E2B) SetPolicy(p Policy) { e.pol = p }
func (e *E2B) Policy() Policy     { return e.pol }

// Handle creates (or reattaches to) the E2B sandbox for a session.
func (e *E2B) Handle(sessionID uuid.UUID) (Handle, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.handles[sessionID]; ok {
		return h, nil
	}
	var resp struct {
		SandboxID string `json:"sandboxID"`
	}
	// E2B enforces network policy at the TEMPLATE level (each template
	// bakes its egress rules); we pass ours so the deployment can pick
	// the right one and record the intent.
	err := e.do(context.Background(), http.MethodPost, "/sandboxes", map[string]any{
		"templateID": e.template,
		"metadata":   map[string]string{"agentd_session": sessionID.String(), "egress": string(e.pol.Egress)},
		"timeout":    900, // seconds; sessions are re-enterable, not immortal
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("create e2b sandbox: %w", err)
	}
	h := &e2bHandle{provider: e, sessionID: sessionID, sandboxID: resp.SandboxID}
	e.handles[sessionID] = h
	return h, nil
}

// Kill ends the session's sandbox (best effort; E2B timeouts are the
// backstop). Called by shutdown paths — not part of the Provider
// interface yet, deliberately: lifecycle here is process-scoped.
func (e *E2B) Kill(sessionID uuid.UUID) error {
	e.mu.Lock()
	h, ok := e.handles[sessionID]
	delete(e.handles, sessionID)
	e.mu.Unlock()
	if !ok {
		return nil
	}
	return e.do(context.Background(), http.MethodDelete, "/sandboxes/"+h.sandboxID, nil, nil)
}

type e2bHandle struct {
	provider  *E2B
	sessionID uuid.UUID
	sandboxID string
}

func (h *e2bHandle) SessionID() uuid.UUID { return h.sessionID }

// CanEnforceEgress: TRUE — egress rules are part of the microVM
// template definition (Firecracker network config), enforced below the
// guest kernel.
func (h *e2bHandle) CanEnforceEgress() bool { return true }

// Workdir for E2B is the sandbox-internal path; host-side tooling must
// go through ResolvePath + the file API, never os.* on this string.
func (h *e2bHandle) Workdir() string { return "/home/user/agentd" }

func (h *e2bHandle) ResolvePath(modelPath string) (string, error) {
	return resolveUnder(h.Workdir(), modelPath)
}

// Exec runs a command inside the microVM via the commands API.
func (h *e2bHandle) Exec(ctx context.Context, command string, timeout time.Duration) (ExecResult, error) {
	if command == "" {
		return ExecResult{}, fmt.Errorf("empty command")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	var resp struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	start := time.Now()
	err := h.provider.do(ctx, http.MethodPost, "/sandboxes/"+h.sandboxID+"/commands", map[string]any{
		"cmd":     "sh",
		"args":    []string{"-c", command},
		"cwd":     h.Workdir(),
		"timeout": int(timeout.Seconds()),
	}, &resp)
	if err != nil {
		return ExecResult{}, err
	}
	so, t1 := truncateE2B(resp.Stdout)
	se, t2 := truncateE2B(resp.Stderr)
	return ExecResult{
		Command: command, ExitCode: resp.ExitCode, Stdout: so, Stderr: se,
		Truncated: t1 || t2, Duration: time.Since(start),
	}, nil
}

// WriteFile/ReadFile helpers ride the files API (base64 payloads).

func (h *e2bHandle) WriteFile(ctx context.Context, path string, content []byte) error {
	full, err := h.ResolvePath(path)
	if err != nil {
		return err
	}
	return h.provider.do(ctx, http.MethodPost, "/sandboxes/"+h.sandboxID+"/files", map[string]any{
		"path":    full,
		"content": base64.StdEncoding.EncodeToString(content),
	}, nil)
}

func (h *e2bHandle) ReadFile(ctx context.Context, path string) ([]byte, error) {
	full, err := h.ResolvePath(path)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Content string `json:"content"`
	}
	if err := h.provider.do(ctx, http.MethodGet,
		"/sandboxes/"+h.sandboxID+"/files?path="+full, nil, &resp); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(resp.Content)
}

func (e *E2B) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("e2b %s %s: %s: %.200s", method, path, resp.Status, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func truncateE2B(s string) (string, bool) {
	if len(s) <= MaxOutputBytes {
		return s, false
	}
	return s[:MaxOutputBytes] + "\n...[output truncated]", true
}
