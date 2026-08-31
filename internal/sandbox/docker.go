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

// DefaultImage is the per-exec container image: small, ubiquitous, and
// enough for read → edit → exec sessions. Agent configs will be able to
// override this once the config surface grows (post-M2).
const DefaultImage = "alpine:3.20"

// Docker runs commands in throwaway containers with the session workdir
// bind-mounted — namespace isolation per ADR-001's dev tier. Each Exec
// is its own `docker run --rm`: process state does not survive between
// commands, but the filesystem (the workdir) does. That is the documented
// contract; tools that need state chain it in one command.
type Docker struct {
	base  string
	image string
	pol   Policy

	mu      sync.Mutex
	handles map[uuid.UUID]*dockerHandle
}

func NewDocker(base, image string) (*Docker, error) {
	if image == "" {
		image = DefaultImage
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox base %s: %w", base, err)
	}
	return &Docker{base: base, image: image, pol: DefaultPolicy(), handles: map[uuid.UUID]*dockerHandle{}}, nil
}

func (d *Docker) Handle(sessionID uuid.UUID) (Handle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h, ok := d.handles[sessionID]; ok {
		return h, nil
	}
	workdir := filepath.Join(d.base, sessionID.String(), "workspace")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, fmt.Errorf("create workdir for session %s: %w", sessionID, err)
	}
	d.handles[sessionID] = &dockerHandle{
		sessionID: sessionID, workdir: workdir, image: d.image,
		network: d.pol.Egress == EgressAllow,
	}
	return d.handles[sessionID], nil
}

type dockerHandle struct {
	sessionID uuid.UUID
	workdir   string
	image     string
	network   bool // false → --network none (egress denied)
}

func (d *Docker) SetPolicy(p Policy) { d.pol = p }
func (d *Docker) Policy() Policy     { return d.pol }

func (h *dockerHandle) SessionID() uuid.UUID { return h.sessionID }

// CanEnforceEgress: TRUE — --network none is kernel-level enforcement.
func (h *dockerHandle) CanEnforceEgress() bool { return true }
func (h *dockerHandle) Workdir() string        { return h.workdir }

func (h *dockerHandle) ResolvePath(modelPath string) (string, error) {
	return resolveUnder(h.workdir, modelPath)
}

func (h *dockerHandle) Exec(ctx context.Context, command string, timeout time.Duration) (ExecResult, error) {
	if command == "" {
		return ExecResult{}, fmt.Errorf("empty command")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The workdir is bind-mounted at the same guest path so tool outputs
	// (absolute paths) read identically inside and outside the container.
	// Network policy is enforced by the kernel: --network none unless
	// egress was explicitly allowed (ADR-001's floor).
	start := time.Now()
	args := []string{"run", "--rm"}
	if !h.network {
		args = append(args, "--network", "none")
	}
	args = append(args,
		"-v", h.workdir+":"+h.workdir,
		"-w", h.workdir,
		h.image, "sh", "-c", command)
	cmd := exec.CommandContext(cctx, "docker", args...)
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
			// docker daemon missing / image pull failure — infrastructure.
			return ExecResult{}, fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(stderr.String()))
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

// ReadFile/WriteFile: the workdir is bind-mounted at the same path, so
// host-side fs on the anchored path is correct for docker.
func (h *dockerHandle) ReadFile(_ context.Context, path string) ([]byte, error) {
	full, err := h.ResolvePath(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (h *dockerHandle) WriteFile(_ context.Context, path string, content []byte) error {
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
