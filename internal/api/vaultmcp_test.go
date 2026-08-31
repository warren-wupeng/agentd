package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/warren-wupeng/agentd/internal/api"
	"github.com/warren-wupeng/agentd/internal/mcp"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/vault"
)

func newVaultMCPServer(t *testing.T) *server {
	t.Helper()
	st := testutil.NewStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	ctx := context.Background()
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
	return newStreamServer(t, api.NewHandler(st, api.WithVaultMCP(v, m)))
}

func TestVaultEndpointsAreWriteOnly(t *testing.T) {
	s := newVaultMCPServer(t)
	code, resp := s.do("PUT", "/v1/vault/secrets", map[string]any{
		"name": "github", "value": "ghp_topsecret",
	})
	if code != http.StatusCreated {
		t.Fatalf("put: %d %v", code, resp)
	}
	blob := fmt.Sprint(resp)
	if strings.Contains(blob, "ghp_topsecret") {
		t.Fatal("PUT echoed the value back")
	}

	code, resp = s.do("GET", "/v1/vault/secrets", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if strings.Contains(fmt.Sprint(resp), "ghp_topsecret") {
		t.Fatal("LIST leaked a value")
	}
	if !strings.Contains(fmt.Sprint(resp), "github") {
		t.Fatal("list missing the name")
	}

	code, _ = s.do("DELETE", "/v1/vault/secrets/github", nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	code, resp = s.do("DELETE", "/v1/vault/secrets/github", nil)
	if code != http.StatusNotFound || resp["error"].(map[string]any)["remediation"] == nil {
		t.Fatalf("double delete: %d %v", code, resp)
	}
}

func TestMCPServerRegistryEndpoints(t *testing.T) {
	s := newVaultMCPServer(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	code, resp := s.do("POST", "/v1/mcp/servers", map[string]any{
		"name": "hub", "base_url": upstream.URL, "secret_name": "hub-key",
	})
	if code != http.StatusCreated {
		t.Fatalf("register: %d %v", code, resp)
	}
	code, resp = s.do("GET", "/v1/mcp/servers", nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(resp), "hub") {
		t.Fatalf("list: %d %v", code, resp)
	}
	// invalid base_url rejected
	code, _ = s.do("POST", "/v1/mcp/servers", map[string]any{
		"name": "bad", "base_url": "file:///etc", "secret_name": "x",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("bad base_url: %d", code)
	}
}

func TestMCPSessionProxyTokenEnforcement(t *testing.T) {
	s := newVaultMCPServer(t)
	// reach the vault through a second handler? Simpler: the session
	// token is derived; we test enforcement with garbage and a valid
	// token minted from a directly-constructed vault with the SAME key.
	ctx := context.Background()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3) // must mirror newVaultMCPServer's key
	}
	v2, err := vault.New(ctx, testutil.DatabaseURL(t), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v2.Close() }()

	agentID := s.mustCreateAgent("proxy-agent", "m")
	code, resp := s.do("POST", "/v1/sessions", map[string]any{"agent_id": agentID})
	if code != http.StatusCreated {
		t.Fatal(resp)
	}
	sessID := resp["session"].(map[string]any)["id"].(string)

	// 1. no token → 409
	code, _ = s.do("POST", "/v1/sessions/"+sessID+"/mcp/hub",
		map[string]any{"path": "/x"})
	if code != http.StatusConflict {
		t.Fatalf("no token: %d", code)
	}

	// 2. wrong token → 409
	code, _ = s.doWithHeader("POST", "/v1/sessions/"+sessID+"/mcp/hub",
		map[string]any{"path": "/x"}, "X-Session-Token", strings.Repeat("0", 64))
	if code != http.StatusConflict {
		t.Fatalf("wrong token: %d", code)
	}

	// 3. right token + registered upstream → proxied with credential
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok_PROXYSECRET" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"proxied":true}`)
	}))
	defer upstream.Close()
	// seed: store the secret and register the server via the API
	if code, resp := s.do("PUT", "/v1/vault/secrets", map[string]any{"name": "hub-key", "value": "tok_PROXYSECRET"}); code != http.StatusCreated {
		t.Fatalf("seed secret: %d %v", code, resp)
	}
	if code, resp := s.do("POST", "/v1/mcp/servers", map[string]any{"name": "hub", "base_url": upstream.URL, "secret_name": "hub-key"}); code != http.StatusCreated {
		t.Fatalf("seed server: %d %v", code, resp)
	}
	tok := v2.SessionToken(sessID)
	code, resp = s.doWithHeader("POST", "/v1/sessions/"+sessID+"/mcp/hub",
		map[string]any{"path": "/anything", "method": "POST", "body": map[string]any{}},
		"X-Session-Token", tok)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(resp), "proxied") {
		t.Fatalf("proxy with valid token: %d %v", code, resp)
	}

	// 4. terminated session → token dead
	code, _ = s.do("POST", "/v1/sessions/"+sessID+"/transitions", map[string]any{"to": "terminated"})
	if code != http.StatusOK {
		t.Fatal("terminate failed")
	}
	code, _ = s.doWithHeader("POST", "/v1/sessions/"+sessID+"/mcp/hub",
		map[string]any{"path": "/x"}, "X-Session-Token", tok)
	if code != http.StatusConflict {
		t.Fatalf("terminated session proxy: %d", code)
	}
}
