package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/warren-wupeng/agentd/internal/mcp"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/vault"
)

var ctx = context.Background()

func TestMain(m *testing.M) { testutil.Main(m) }

func newStack(t *testing.T) (*vault.Vault, *mcp.MCP) {
	t.Helper()
	testutil.NewStore(t) // migrate + truncate
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	v, err := vault.New(ctx, testutil.DatabaseURL(t), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	m, err := mcp.New(ctx, testutil.DatabaseURL(t), v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return v, m
}

// The M6 criterion in its smallest form: the upstream PROVABLY receives
// the vault credential; the caller PROVABLY never held it.
func TestCallInjectsVaultCredential(t *testing.T) {
	v, m := newStack(t)

	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = fmt.Fprint(w, `{"ok":true,"data":[1,2,3]}`)
	}))
	defer upstream.Close()

	if err := v.PutSecret(ctx, "hub", "tok_live_SECRETVALUE"); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(ctx, mcp.Server{Name: "hub", BaseURL: upstream.URL, SecretName: "hub"}); err != nil {
		t.Fatal(err)
	}

	status, resp, err := m.Call(ctx, "hub", http.MethodPost, "/v1/search", json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if status != 200 || !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("status=%d resp=%s", status, resp)
	}
	if got := fmt.Sprint(gotAuth.Load()); got != "Bearer tok_live_SECRETVALUE" {
		t.Fatalf("upstream auth = %q, want the injected vault credential", got)
	}
}

func TestCallUnregisteredAndMissingSecretAreRemediated(t *testing.T) {
	_, m := newStack(t)

	_, _, err := m.Call(ctx, "nope", "POST", "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unregistered server: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	if err := m.Register(ctx, mcp.Server{Name: "s", BaseURL: upstream.URL, SecretName: "ghost"}); err != nil {
		t.Fatal(err)
	}
	_, _, err = m.Call(ctx, "s", "POST", "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("missing secret should name it: %v", err)
	}
}

func TestRegisterValidates(t *testing.T) {
	_, m := newStack(t)
	if err := m.Register(ctx, mcp.Server{Name: "x", BaseURL: "ftp://nope", SecretName: "s"}); err == nil {
		t.Fatal("non-http(s) base_url must be rejected")
	}
	if err := m.Register(ctx, mcp.Server{Name: "", BaseURL: "https://ok.example", SecretName: "s"}); err == nil {
		t.Fatal("empty name must be rejected")
	}
	list, err := m.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("list after rejects: %v %+v", err, list)
	}
}
