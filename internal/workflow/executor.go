package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warren-wupeng/agentd/internal/agentderr"
	"github.com/warren-wupeng/agentd/internal/harness"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/sandbox"
	"github.com/warren-wupeng/agentd/internal/store"
)

// Executor runs workflow definitions: topological scheduling with
// parallel fan-out, per-node retry, output propagation. Every node is
// an ordinary session through the harness seam — workflows never know
// what a harness is.
type Executor struct {
	store  *store.Store
	byName map[string]harness.Harness
	sb     sandbox.Provider
	log    *slog.Logger
	pool   *pgxpool.Pool

	mu      sync.Mutex
	running map[string]bool
}

func NewExecutor(ctx context.Context, databaseURL string, st *store.Store, sb sandbox.Provider, log *slog.Logger, hs ...harness.Harness) (*Executor, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	e := &Executor{
		store: st, sb: sb, log: log, pool: pool,
		byName:  map[string]harness.Harness{},
		running: map[string]bool{},
	}
	for _, h := range hs {
		e.byName[h.Name()] = h
	}
	return e, nil
}

func (e *Executor) Close() error { e.pool.Close(); return nil }

// Start validates, persists, and asynchronously runs the definition.
// The returned run has id + initial node states.
func (e *Executor) Start(ctx context.Context, raw []byte) (*Run, error) {
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, err
	}
	for _, n := range def.Nodes {
		if _, ok := e.byName[n.Harness]; !ok {
			return nil, agentderr.InvalidInput(
				"node "+n.ID+" names harness "+n.Harness+" which is not registered",
				"registered harnesses are listed by GET /v1/sessions creation errors")
		}
		if _, err := uuid.Parse(n.Agent); err != nil {
			return nil, agentderr.InvalidInput(
				"node "+n.ID+" agent must be a UUID",
				"copy the agent id from GET /v1/agents")
		}
	}

	run := &Run{
		ID: uuid.NewString(), Name: def.Name, Status: "running",
		Definition: def,
	}
	for _, n := range def.Nodes {
		run.NodeStates = append(run.NodeStates, NodeState{ID: n.ID, Status: "pending"})
	}
	if err := e.persist(ctx, run); err != nil {
		return nil, err
	}

	e.mu.Lock()
	if e.running[run.ID] {
		e.mu.Unlock()
		return run, nil // already going (idempotent start)
	}
	e.running[run.ID] = true
	e.mu.Unlock()

	go e.execute(run)
	return run, nil
}

// Get loads one run.
func (e *Executor) Get(ctx context.Context, id uuid.UUID) (*Run, error) {
	var name, status string
	var defRaw, statesRaw []byte
	err := e.pool.QueryRow(ctx,
		`SELECT name, status, definition, node_states FROM workflow_runs WHERE id = $1`, id).
		Scan(&name, &status, &defRaw, &statesRaw)
	if err != nil {
		return nil, agentderr.NotFound("workflow run "+id.String()+" not found", "list runs via the console or create one via POST /v1/workflows")
	}
	run := &Run{ID: id.String(), Name: name, Status: status}
	_ = json.Unmarshal(defRaw, &run.Definition)
	_ = json.Unmarshal(statesRaw, &run.NodeStates)
	return run, nil
}

func (e *Executor) persist(ctx context.Context, run *Run) error {
	defRaw, err := json.Marshal(run.Definition)
	if err != nil {
		return agentderr.Internal(err)
	}
	statesRaw, err := json.Marshal(run.NodeStates)
	if err != nil {
		return agentderr.Internal(err)
	}
	_, err = e.pool.Exec(ctx, `
		INSERT INTO workflow_runs (id, name, status, definition, node_states)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
		  status = EXCLUDED.status, node_states = EXCLUDED.node_states, updated_at = now()`,
		run.ID, run.Name, run.Status, defRaw, statesRaw)
	if err != nil {
		return agentderr.Internal(err)
	}
	return nil
}

// execute is the scheduler loop: launch every ready node in parallel,
// wait for the wave, persist, repeat. Fan-out is real goroutines.
func (e *Executor) execute(run *Run) {
	ctx := context.Background()
	defer func() {
		e.mu.Lock()
		delete(e.running, run.ID)
		e.mu.Unlock()
	}()

	def := run.Definition
	outputs := map[string]NodeOutput{}

	for {
		nodeOf := map[string]*NodeDef{}
		for i := range def.Nodes {
			nodeOf[def.Nodes[i].ID] = &def.Nodes[i]
		}
		stateOf := map[string]*NodeState{}
		for i := range run.NodeStates {
			stateOf[run.NodeStates[i].ID] = &run.NodeStates[i]
		}

		// ready = pending with all deps completed
		var ready []*NodeDef
		failed := false
		for i := range def.Nodes {
			n := &def.Nodes[i]
			st := stateOf[n.ID]
			if st.Status != "pending" {
				if st.Status == "failed" {
					failed = true
				}
				continue
			}
			depsMet := true
			for _, dep := range n.DependsOn {
				if stateOf[dep].Status != "completed" {
					depsMet = false
					break
				}
			}
			if depsMet {
				ready = append(ready, n)
			}
		}

		// nothing pending and nothing runnable → done
		if len(ready) == 0 {
			run.Status = "completed"
			if failed {
				run.Status = "failed"
			}
			_ = e.persist(ctx, run)
			e.log.Info("workflow finished", "run", run.ID, "name", run.Name, "status", run.Status)
			return
		}

		// run the wave in parallel
		type result struct {
			node *NodeDef
			out  NodeOutput
			err  error
		}
		results := make(chan result, len(ready))
		for _, n := range ready {
			stateOf[n.ID].Status = "running"
			stateOf[n.ID].StartedAt = time.Now().UTC().Format(time.RFC3339)
			go func(n *NodeDef) {
				out, err := e.runNode(ctx, run, n, outputs)
				results <- result{node: n, out: out, err: err}
			}(n)
		}
		_ = e.persist(ctx, run)

		for range ready {
			r := <-results
			st := stateOf[r.node.ID]
			st.Attempts++
			if r.err != nil {
				if st.Attempts <= r.node.MaxRetries {
					st.Status = "pending" // retry in the next wave
					st.Error = r.err.Error()
					e.log.Warn("workflow node failed, will retry",
						"run", run.ID, "node", r.node.ID, "attempt", st.Attempts, "err", r.err)
					continue
				}
				st.Status = "failed"
				st.Error = r.err.Error()
				st.EndedAt = time.Now().UTC().Format(time.RFC3339)
				e.log.Error("workflow node failed permanently",
					"run", run.ID, "node", r.node.ID, "err", r.err)
				continue
			}
			st.Status = "completed"
			st.Output = r.out.Text
			st.EndedAt = time.Now().UTC().Format(time.RFC3339)
			outputs[r.node.ID] = r.out
		}
		_ = e.persist(ctx, run)
	}
}

// runNode executes one node: fresh pinned session, rendered prompt,
// harness Run to park, output + output_files collection.
func (e *Executor) runNode(ctx context.Context, run *Run, n *NodeDef, outputs map[string]NodeOutput) (NodeOutput, error) {
	agentID, _ := uuid.Parse(n.Agent)

	sess, _, err := e.store.CreateSession(ctx, agentID, 0, n.Harness)
	if err != nil {
		return NodeOutput{}, fmt.Errorf("node %s: create session: %w", n.ID, err)
	}
	prompt := RenderPrompt(n.Prompt, outputs)
	input, _ := json.Marshal(map[string]any{
		"content": []model.Block{model.TextBlock(prompt)},
	})
	if _, err := e.store.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser, input); err != nil {
		return NodeOutput{}, fmt.Errorf("node %s: post prompt: %w", n.ID, err)
	}

	// update the persisted session id while the node runs
	for i := range run.NodeStates {
		if run.NodeStates[i].ID == n.ID {
			run.NodeStates[i].SessionID = sess.ID.String()
		}
	}

	h := e.byName[n.Harness]
	cfg := e.pinnedConfig(ctx, agentID, sess.AgentVersion)
	handle, err := h.Launch(ctx, harness.WorkerSpec{
		SessionID: sess.ID, AgentID: agentID, AgentVersion: sess.AgentVersion, Config: cfg,
	})
	if err != nil {
		return NodeOutput{}, fmt.Errorf("node %s: launch: %w", n.ID, err)
	}
	if err := h.Run(ctx, handle); err != nil {
		return NodeOutput{}, fmt.Errorf("node %s: run: %w", n.ID, err)
	}

	return e.collectOutput(ctx, sess.ID, n)
}

func (e *Executor) pinnedConfig(ctx context.Context, agentID uuid.UUID, version int) json.RawMessage {
	v, err := e.store.GetAgentVersion(ctx, agentID, version)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return v.Config
}

// collectOutput projects the node session into its NodeOutput: final
// text plus any declared output_files read from the session sandbox.
func (e *Executor) collectOutput(ctx context.Context, sessionID uuid.UUID, n *NodeDef) (NodeOutput, error) {
	events, err := e.store.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err != nil {
		return NodeOutput{}, err
	}
	out := NodeOutput{}
	failed := ""
	for _, ev := range events {
		switch ev.Type {
		case store.EventMessageAssistant:
			var pl struct {
				Content []model.Block `json:"content"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			for _, b := range pl.Content {
				if b.Type == model.BlockText && b.Text != "" {
					out.Text = b.Text
				}
			}
		case store.EventTurnCompleted:
			var pl struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			if pl.StopReason == "retries_exhausted" {
				failed = "session parked with retries_exhausted"
			}
		}
	}
	if failed != "" {
		return NodeOutput{}, fmt.Errorf("node %s: %s", n.ID, failed)
	}

	if len(n.OutputFiles) > 0 && e.sb != nil {
		h, err := e.sb.Handle(sessionID)
		if err == nil {
			out.Files = map[string]string{}
			for _, path := range n.OutputFiles {
				data, err := h.ReadFile(ctx, path)
				if err == nil {
					out.Files[path] = string(data)
				}
			}
		}
	}
	return out, nil
}
