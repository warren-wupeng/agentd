package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
)

// maxFileBytes caps what read_file returns and edit_file rewrites — a
// binary or a generated bundle must not flood the context.
const maxFileBytes = 256 << 10

// ReadFile returns a file's contents from the session workspace.
type ReadFile struct{}

func NewReadFile() *ReadFile { return &ReadFile{} }

func (t *ReadFile) Name() string { return "read_file" }
func (t *ReadFile) PolicyDefault() policy.Verdict {
	return policy.Verdict{Decision: policy.Allow}
}

func (t *ReadFile) Description() string {
	return "Read a file from the session workspace. Paths are relative to the workspace root; paths outside it are rejected."
}

func (t *ReadFile) Schema() json.RawMessage {
	return objSchema(map[string]any{
		"path": prop("File path relative to the workspace root", "string"),
	}, "path")
}

func (t *ReadFile) Execute(_ context.Context, h sandbox.Handle, input json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("parse input: %w", err)
	}
	full, err := h.ResolvePath(in.Path)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	data, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return fmt.Sprintf("error: file not found: %s (list the workspace with bash: ls -R)", in.Path), nil
	}
	if err != nil {
		return "", err
	}
	if len(data) > maxFileBytes {
		return string(data[:maxFileBytes]) + "\n...[truncated]", nil
	}
	return string(data), nil
}

// WriteFile creates or overwrites a file in the session workspace.
type WriteFile struct{}

func NewWriteFile() *WriteFile { return &WriteFile{} }

func (t *WriteFile) Name() string { return "write_file" }
func (t *WriteFile) PolicyDefault() policy.Verdict {
	return policy.Verdict{Decision: policy.Allow}
}

func (t *WriteFile) Description() string {
	return "Create or overwrite a file in the session workspace. Parent directories are created. " +
		"Provide the complete new content — this is not an append."
}

func (t *WriteFile) Schema() json.RawMessage {
	return objSchema(map[string]any{
		"path":    prop("File path relative to the workspace root", "string"),
		"content": prop("The complete file content to write", "string"),
	}, "path", "content")
}

func (t *WriteFile) Execute(_ context.Context, h sandbox.Handle, input json.RawMessage) (string, error) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("parse input: %w", err)
	}
	full, err := h.ResolvePath(in.Path)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	if dir := filepath.Dir(full); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(full, []byte(in.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), nil
}

// EditFile replaces exact-string occurrences — unique-match or explicit
// replace_all, the two shapes that can't silently edit the wrong spot.
type EditFile struct{}

func NewEditFile() *EditFile { return &EditFile{} }

func (t *EditFile) Name() string { return "edit_file" }
func (t *EditFile) PolicyDefault() policy.Verdict {
	return policy.Verdict{Decision: policy.Allow}
}

func (t *EditFile) Description() string {
	return "Edit a file by replacing an exact string. old_string must match exactly once unless replace_all is true. " +
		"Read the file first; if old_string matches zero or multiple times the edit fails with a count."
}

func (t *EditFile) Schema() json.RawMessage {
	return objSchema(map[string]any{
		"path":        prop("File path relative to the workspace root", "string"),
		"old_string":  prop("The exact text to replace (including whitespace)", "string"),
		"new_string":  prop("The replacement text", "string"),
		"replace_all": propBool("Replace every occurrence instead of requiring a unique match"),
	}, "path", "old_string", "new_string")
}

func (t *EditFile) Execute(_ context.Context, h sandbox.Handle, input json.RawMessage) (string, error) {
	var in struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("parse input: %w", err)
	}
	full, err := h.ResolvePath(in.Path)
	if err != nil {
		return "error: " + err.Error(), nil
	}
	data, err := os.ReadFile(full)
	if os.IsNotExist(err) {
		return fmt.Sprintf("error: file not found: %s — create it with write_file first", in.Path), nil
	}
	if err != nil {
		return "", err
	}
	if len(data) > maxFileBytes {
		return "error: file exceeds the edit size cap; rewrite it with write_file instead", nil
	}

	n := bytes.Count(data, []byte(in.OldString))
	if n == 0 {
		return fmt.Sprintf("error: old_string not found in %s — read the file and copy the exact text", in.Path), nil
	}
	if n > 1 && !in.ReplaceAll {
		return fmt.Sprintf(
			"error: old_string matches %d times in %s — include more surrounding lines to make it unique, or set replace_all",
			n, in.Path), nil
	}
	updated := strings.Replace(string(data), in.OldString, in.NewString, 1)
	count := 1
	if in.ReplaceAll {
		updated = strings.Replace(string(data), in.OldString, in.NewString, -1)
		count = n
	}
	if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", count, in.Path), nil
}
