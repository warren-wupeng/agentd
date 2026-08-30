package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/sandbox"
)

// newHandle builds a real exec handle on a temp dir — the file tools are
// what's under test here, not the sandbox.
func newHandle(t *testing.T) sandbox.Handle {
	t.Helper()
	p, err := sandbox.NewExec(filepath.Join(t.TempDir(), "sb"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Handle(uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func exec(t *testing.T, tool Tool, h sandbox.Handle, input string) string {
	t.Helper()
	out, err := tool.Execute(context.Background(), h, json.RawMessage(input))
	if err != nil {
		t.Fatalf("execute %s: %v", tool.Name(), err)
	}
	return out
}

func TestWriteReadEditFlow(t *testing.T) {
	h := newHandle(t)
	w, r, e := NewWriteFile(), NewReadFile(), NewEditFile()

	if out := exec(t, w, h, `{"path":"src/deep/note.txt","content":"alpha beta gamma"}`); !strings.Contains(out, "16 bytes") {
		t.Fatalf("write output: %q", out)
	}
	if out := exec(t, r, h, `{"path":"src/deep/note.txt"}`); out != "alpha beta gamma" {
		t.Fatalf("read output: %q", out)
	}
	if out := exec(t, e, h, `{"path":"src/deep/note.txt","old_string":"beta","new_string":"BETA"}`); !strings.Contains(out, "replaced 1 occurrence") {
		t.Fatalf("edit output: %q", out)
	}
	if out := exec(t, r, h, `{"path":"src/deep/note.txt"}`); out != "alpha BETA gamma" {
		t.Fatalf("post-edit read: %q", out)
	}
}

func TestEditFileAmbiguityAndMisses(t *testing.T) {
	h := newHandle(t)
	e := NewEditFile()
	exec(t, NewWriteFile(), h, `{"path":"a.txt","content":"x y x y x"}`)

	if out := exec(t, e, h, `{"path":"a.txt","old_string":"x","new_string":"z"}`); !strings.Contains(out, "matches 3 times") {
		t.Fatalf("ambiguous edit should fail with count: %q", out)
	}
	if out := exec(t, e, h, `{"path":"a.txt","old_string":"nope","new_string":"z"}`); !strings.Contains(out, "not found") {
		t.Fatalf("miss should fail clearly: %q", out)
	}
	if out := exec(t, e, h, `{"path":"a.txt","old_string":"x","new_string":"z","replace_all":true}`); !strings.Contains(out, "replaced 3 occurrence") {
		t.Fatalf("replace_all output: %q", out)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	h := newHandle(t)
	r, w := NewReadFile(), NewWriteFile()

	for _, bad := range []string{`{"path":"../escape.txt"}`, `{"path":"/etc/passwd"}`, `{"path":"a/../../escape"}`} {
		if out := exec(t, w, h, bad); !strings.Contains(out, "error:") {
			t.Fatalf("write with %s should be rejected, got %q", bad, out)
		}
		if out := exec(t, r, h, bad); !strings.Contains(out, "error:") {
			t.Fatalf("read with %s should be rejected, got %q", bad, out)
		}
	}
	// the guard must not have created anything outside the workdir
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.Workdir()), "escape.txt")); err == nil {
		t.Fatal("escape.txt exists outside the workdir")
	}
}

func TestReadFileMissingIsData(t *testing.T) {
	h := newHandle(t)
	out := exec(t, NewReadFile(), h, `{"path":"nope.txt"}`)
	if !strings.Contains(out, "file not found") {
		t.Fatalf("missing file output: %q", out)
	}
}

func TestBashExitCodeIsData(t *testing.T) {
	h := newHandle(t)
	out := exec(t, NewBash(), h, `{"command":"echo out; echo err 1>&2; exit 3"}`)
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") || !strings.Contains(out, "[exit code: 3]") {
		t.Fatalf("bash output should carry stdout, stderr, exit code: %q", out)
	}
}

func TestBashInvalidInputIsData(t *testing.T) {
	h := newHandle(t)
	out := exec(t, NewBash(), h, `{"timeout_seconds":9999,"command":"true"}`)
	if !strings.Contains(out, "error: timeout_seconds") {
		t.Fatalf("invalid timeout should come back as data: %q", out)
	}
}

func TestRegistryEnableUnknownToolFails(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Enable([]string{"bash", "chainsaw"}); err == nil {
		t.Fatal("unknown tool must be rejected, not silently dropped")
	}
	got, err := r.Enable([]string{"bash", "read_file"})
	if err != nil || len(got) != 2 {
		t.Fatalf("Enable = %v, %v", got, err)
	}
	if all, _ := r.Enable(nil); len(all) != 4 {
		t.Fatalf("Enable(nil) = %d tools, want all 4", len(all))
	}
}
