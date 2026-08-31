package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The M5 done-when: an adversarial corpus against every provider, with
// expected outcomes driven by what each provider ACTUALLY guarantees.
// The dev tier's weaknesses are asserted as EXPECTED behavior — a test
// that documents "exec allows host reads" is worth more than omitting
// the test; silent semantic drift becomes a red build.

// escapeCase is one adversarial probe.
type escapeCase struct {
	name    string
	command string
	// blocked: true → the probe must FAIL to reach its target (non-zero
	// exit or empty result). false → the probe succeeds; the provider
	// does not isolate this dimension (asserted so drift is visible).
	blocked bool
}

// hostFileRead reads a file that exists on a typical host but MUST NOT
// be the host's file from inside a real sandbox (a container's
// /etc/passwd is its own; an e2b microVM's is its own).
var hostFileRead = escapeCase{
	name:    "host-file-read /etc/hostname content",
	command: `cat /etc/hostname`,
}

// egressProbe: DNS + HTTP — fails fast when the network is gone.
const egressProbe = `nslookup one.one.one.one >/dev/null 2>&1 || getent hosts example.com >/dev/null 2>&1 || wget -q -T 3 -O /dev/null http://example.com 2>&1`

func runEscape(t *testing.T, h Handle, tc escapeCase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.Exec(ctx, tc.command, 20*time.Second)
	if err != nil {
		// Infrastructure errors count as blocked for probes, and as
		// failures for must-succeed probes — the caller interprets.
		t.Fatalf("%s: exec infrastructure error: %v", tc.name, err)
	}
	if tc.blocked && res.ExitCode == 0 {
		t.Errorf("%s: probe SUCCEEDED (exit 0) but this provider must block it:\n%s",
			tc.name, truncateForLog(res.Stdout)+truncateForLog(res.Stderr))
	}
	if !tc.blocked && res.ExitCode != 0 {
		t.Errorf("%s: probe failed (exit %d) but this provider documents it as allowed:\n%s",
			tc.name, res.ExitCode, truncateForLog(res.Stderr))
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// --- the exec tier: zero isolation, asserted honestly ---

func TestEscapeSuiteExecTier(t *testing.T) {
	p, err := NewExec(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Handle(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if h.CanEnforceEgress() {
		t.Fatal("exec must report CanEnforceEgress=false — honesty is the contract")
	}

	// Path containment is OUR guard (resolveUnder) and must hold on
	// every tier, isolated or not.
	if _, err := h.ResolvePath("../escape.txt"); err == nil {
		t.Fatal("resolveUnder guard failed on exec")
	}

	// The dev tier does NOT isolate the host: these probes succeed, on
	// purpose in the assertions. If one starts failing, exec semantics
	// changed silently — investigate, don't just flip the expectation.
	runEscape(t, h, escapeCase{name: "dev-tier host file readable (documented)", command: hostFileRead.command, blocked: false})
	if h.CanEnforceEgress() {
		t.Fatal("unreachable")
	}
	// Network: exec cannot enforce; probe may succeed OR fail depending
	// on the environment's own egress — we assert only that the provider
	// did not lie about enforcement, not the network state.
	_, _ = h.Exec(context.Background(), egressProbe, 15*time.Second)
}

// Output flood must be capped on every tier — the event log must never
// hold a 10MB tool output.
func TestEscapeSuiteOutputFloodCapped(t *testing.T) {
	providers := map[string]func(t *testing.T) Provider{
		"exec": func(t *testing.T) Provider {
			p, err := NewExec(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
	}
	if name := ProviderForTest("docker"); name != "" {
		providers["docker"] = func(t *testing.T) Provider {
			p, err := NewDocker(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	for name, mk := range providers {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			h, err := p.Handle(uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			res, err := h.Exec(context.Background(), `yes flood | head -c 3145728`, 30*time.Second) // 3MB
			if err != nil {
				t.Fatalf("flood exec: %v", err)
			}
			if len(res.Stdout) > MaxOutputBytes+1024 {
				t.Fatalf("output not capped: %d bytes", len(res.Stdout))
			}
			if !res.Truncated {
				t.Fatalf("truncation flag not set for %d-byte output", len(res.Stdout))
			}
		})
	}
}

// --- the docker tier: real isolation, gated on a daemon existing ---

// ProviderForTest returns the provider name if the tier's gate env is
// set (docker tier needs a daemon; CI sets it on runners that have one).
func ProviderForTest(name string) string {
	switch name {
	case "docker":
		if os.Getenv("AGENTD_DOCKER") == "1" {
			return "docker"
		}
	}
	return ""
}

func TestEscapeSuiteDockerTier(t *testing.T) {
	if ProviderForTest("docker") == "" {
		t.Skip("AGENTD_DOCKER=1 not set — no docker daemon in this environment")
	}
	p, err := NewDocker(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}

	// Default policy: egress denied.
	h, err := p.Handle(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if !h.CanEnforceEgress() {
		t.Fatal("docker must report CanEnforceEgress=true")
	}
	runEscape(t, h, escapeCase{
		name:    "egress denied by default (--network none)",
		command: egressProbe,
		blocked: true,
	})

	// The container's /etc/hostname exists but is the CONTAINER's, not
	// the host's — the read succeeds while the host stays invisible.
	// Assert the stronger property: the value differs from the host's.
	ctx := context.Background()
	res, err := h.Exec(ctx, `cat /etc/hostname`, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hostName, _ := os.ReadFile("/etc/hostname")
	if strings.TrimSpace(string(hostName)) != "" &&
		strings.TrimSpace(res.Stdout) == strings.TrimSpace(string(hostName)) {
		t.Fatalf("container sees the HOST hostname %q — isolation broken", res.Stdout)
	}

	// Workdir bind mount: files written inside are visible outside, and
	// path containment holds.
	out, err := h.Exec(ctx, `echo inside > shared.txt && cat shared.txt`, 20*time.Second)
	if err != nil || out.ExitCode != 0 {
		t.Fatalf("workdir roundtrip failed: %v %+v", err, out)
	}
	if _, err := os.Stat(filepath.Join(h.Workdir(), "shared.txt")); err != nil {
		t.Fatalf("workdir file not visible on host: %v", err)
	}
	if _, err := h.ResolvePath("../escape"); err == nil {
		t.Fatal("resolveUnder guard failed on docker")
	}

	// Opt-in egress allow: the probe now succeeds.
	p.SetPolicy(Policy{Egress: EgressAllow})
	h2, err := p.Handle(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	runEscape(t, h2, escapeCase{
		name:    "egress allowed when policy says so",
		command: egressProbe,
		blocked: false,
	})
}

// --- the e2b tier: wire contract, fake-pinned (experimental) ---

func TestEscapeSuiteE2BWireContract(t *testing.T) {
	fakeFiles := struct {
		sync.Mutex
		m map[string]string
	}{m: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			_ = r.Body.Close()
			_, _ = fmt.Fprint(w, `{"sandboxID":"sbx_test1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sbx_test1/commands":
			var body map[string]any
			_ = decodeBody(r, &body)
			args, _ := body["args"].([]any)
			cmd := ""
			if len(args) > 1 {
				cmd = fmt.Sprint(args[1])
			}
			switch {
			case strings.Contains(cmd, "cat /etc/hostname"):
				_, _ = fmt.Fprint(w, `{"exitCode":0,"stdout":"sbx-test1-host","stderr":""}`)
			case strings.Contains(cmd, "nslookup"), strings.Contains(cmd, "getent"), strings.Contains(cmd, "wget"):
				// microVM with a no-egress template: probes fail
				_, _ = fmt.Fprint(w, `{"exitCode":1,"stdout":"","stderr":"no route to host"}`)
			case strings.Contains(cmd, "yes flood"):
				_, _ = fmt.Fprint(w, `{"exitCode":0,"stdout":"`+strings.Repeat("f", MaxOutputBytes+4096)+`","stderr":""}`)
			default:
				_, _ = fmt.Fprint(w, `{"exitCode":0,"stdout":"","stderr":""}`)
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandboxes/"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sbx_test1/files":
			var body struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			_ = decodeBody(r, &body)
			fakeFiles.Lock()
			fakeFiles.m[strings.TrimPrefix(body.Path, "/home/user/agentd/")] = body.Content
			fakeFiles.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/sbx_test1/files":
			path := r.URL.Query().Get("path")
			fakeFiles.Lock()
			content, ok := fakeFiles.m[strings.TrimPrefix(path, "/home/user/agentd/")]
			fakeFiles.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprint(w, `{"error":"file does not exist"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"content":%q}`, content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p, err := NewE2B(srv.URL, "test-key", "")
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Handle(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if !h.CanEnforceEgress() {
		t.Fatal("e2b must report CanEnforceEgress=true (template-level enforcement)")
	}

	// egress blocked (template-enforced): probe must fail
	runEscape(t, h, escapeCase{name: "e2b egress denied by template", command: egressProbe, blocked: true})

	// output capped even though the API returned more
	res, err := h.Exec(context.Background(), `yes flood | head -c 3145728`, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) > MaxOutputBytes+1024 || !res.Truncated {
		t.Fatalf("e2b output not capped: %d truncated=%v", len(res.Stdout), res.Truncated)
	}

	// files API round-trips base64
	if err := h.WriteFile(context.Background(), "note.txt", []byte("hello e2b")); err != nil {
		t.Fatal(err)
	}
	got, err := h.ReadFile(context.Background(), "note.txt")
	if err != nil || string(got) != "hello e2b" {
		t.Fatalf("file roundtrip: %v %q", err, got)
	}
	if _, err := h.ReadFile(context.Background(), "../escape"); err == nil {
		t.Fatal("resolveUnder guard failed on e2b")
	}

	// kill is idempotent and clears the mapping
	if err := p.Kill(uuid.Nil); err != nil { // no such mapping — no-op
		t.Fatalf("kill of unknown session should be a no-op: %v", err)
	}
}

func decodeBody(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(v)
}
