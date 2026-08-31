package harness_test

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

	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/mcp"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/tools"
	"github.com/warren-wupeng/agentd/internal/vault"
)

// THE M6 done-when, mechanically: an agent calls an MCP server that
// REQUIRES a credential; the server provably receives it; the sandbox
// provably never contained it — not in any event, not on disk, not in
// any tool output.
func TestDoneWhen_MCPCallWithZeroSecretsInSandbox(t *testing.T) {
	const realSecret = "sk_live_MCP_SUPER_SECRET_9f2a"

	ctx := context.Background()
	st := testutil.NewStore(t) // migrate + truncate (single store for all)

	// an MCP upstream that refuses unauthenticated calls
	var sawAuth string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		sawAuth = auth
		mu.Unlock()
		if auth != "Bearer "+realSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"results":[{"repo":"agentd","stars":42}]}`)
	}))
	defer upstream.Close()

	// the credential plane
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	vlt, err := vault.New(ctx, testutil.DatabaseURL(t), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = vlt.Close() }()
	if err := vlt.PutSecret(ctx, "github", realSecret); err != nil {
		t.Fatal(err)
	}
	mcpSvc, err := mcp.New(ctx, testutil.DatabaseURL(t), vlt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mcpSvc.Close() }()
	if err := mcpSvc.Register(ctx, mcp.Server{
		Name: "github", BaseURL: upstream.URL, SecretName: "github",
	}); err != nil {
		t.Fatal(err)
	}

	// the agent's model wants to search github — via the mcp tool
	sbBase := t.TempDir()
	sb, err := sandbox.NewExec(filepath.Join(sbBase, "sb"))
	if err != nil {
		t.Fatal(err)
	}
	fm := &scriptedModel{script: []*model.CompletionResponse{
		{Blocks: []model.Block{{
			Type: model.BlockToolUse, ID: "tu1", Name: "mcp",
			Input: json.RawMessage(`{"server":"github","path":"/v1/search/repos","method":"POST","body":{"q":"agentd"}}`),
		}}, FinishReason: model.FinishToolCalls},
		{Blocks: []model.Block{model.TextBlock("found agentd with 42 stars")}, FinishReason: model.FinishStop},
	}}
	ndeps := &loop.Deps{
		Store: st, Model: fm, Sandbox: sb,
		Policy:       policy.NewStatic(),
		Registry:     tools.NewRegistry(tools.WithMCP(mcpSvc)),
		ModelRetries: 2, RetryBackoff: time.Millisecond,
	}

	a, _, err := st.CreateAgent(ctx, "mcp-agent", "", json.RawMessage(`{"model":"fake","tools":["mcp","bash"]}`))
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[{"type":"text","text":"search github for agentd"}]}`)); err != nil {
		t.Fatal(err)
	}

	native := harness.NewNative(ndeps)
	handle, err := native.Launch(ctx, harness.WorkerSpec{
		SessionID: sess.ID, AgentID: a.ID, AgentVersion: 1, Config: json.RawMessage(`{"model":"fake"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := native.Run(ctx, handle); err != nil {
		t.Fatal(err)
	}

	// 1. the upstream PROVABLY received the real credential
	mu.Lock()
	auth := sawAuth
	mu.Unlock()
	if auth != "Bearer "+realSecret {
		t.Fatalf("upstream never got the credential: %q", auth)
	}

	// 2. the turn completed with the MCP data as the tool result
	events, err := st.ListEvents(ctx, sess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var sawResult bool
	for _, ev := range events {
		if ev.Type == store.EventToolCompleted &&
			// the output string is embedded (escaped) in the event payload
			strings.Contains(string(ev.Payload), `\"stars\":42`) {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("mcp tool result with upstream data missing from the log")
	}
	s2, _ := st.GetSession(ctx, sess.ID)
	if s2.StopReason == nil || *s2.StopReason != store.StopEndTurn {
		t.Fatalf("session parked at %v, want end_turn", s2.StopReason)
	}

	// 3. THE CRITERION: the real secret appears NOWHERE — not in any
	// event payload, not anywhere under the sandbox workdir.
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), realSecret) {
			t.Fatalf("SECRET LEAKED into event log: seq=%d type=%s", ev.Seq, ev.Type)
		}
	}
	var leaked []string
	_ = filepath.Walk(sbBase, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(data), realSecret) {
			leaked = append(leaked, p)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Fatalf("SECRET LEAKED into sandbox files: %v", leaked)
	}
}
