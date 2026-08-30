package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/store"
)

// Dispatcher implements the API's Runner contract: Kick schedules one
// goroutine per active session, routed by the session's harness column.
// It owns concurrency so harnesses don't have to (ADR-004: Run is
// synchronous; the event log is the state).
type Dispatcher struct {
	ctx    context.Context
	store  *store.Store
	byName map[string]Harness
	log    *slog.Logger

	mu     sync.Mutex
	active map[uuid.UUID]bool
	wg     sync.WaitGroup
}

func NewDispatcher(ctx context.Context, st *store.Store, log *slog.Logger, harnesses ...Harness) *Dispatcher {
	d := &Dispatcher{ctx: ctx, store: st, log: log, byName: map[string]Harness{}, active: map[uuid.UUID]bool{}}
	for _, h := range harnesses {
		d.byName[h.Name()] = h
	}
	return d
}

// Names returns the registered harness names (for API validation).
func (d *Dispatcher) Names() []string {
	out := make([]string, 0, len(d.byName))
	for n := range d.byName {
		out = append(out, n)
	}
	return out
}

// Kick schedules the session's actor. Idempotent and non-blocking: an
// active session is left alone (its current Run sees new events), a
// parked one gets a fresh goroutine.
func (d *Dispatcher) Kick(sessionID uuid.UUID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active[sessionID] {
		return
	}
	d.active[sessionID] = true
	d.wg.Add(1)
	go d.run(sessionID)
}

// Wait blocks until all actors are parked (shutdown drain).
func (d *Dispatcher) Wait() { d.wg.Wait() }

func (d *Dispatcher) run(sessionID uuid.UUID) {
	defer func() {
		d.mu.Lock()
		delete(d.active, sessionID)
		d.mu.Unlock()
		d.wg.Done()
	}()

	sess, err := d.store.GetSession(d.ctx, sessionID)
	if err != nil {
		d.log.Error("dispatcher: session load failed", "session", sessionID, "err", err)
		return
	}
	if sess.State == store.StateTerminated {
		return
	}
	h, ok := d.byName[sess.Harness]
	if !ok {
		d.log.Error("dispatcher: no harness registered", "session", sessionID, "harness", sess.Harness,
			"registered", d.Names())
		return
	}

	spec, err := d.workerSpec(d.ctx, sess)
	if err != nil {
		d.log.Error("dispatcher: spec build failed", "session", sessionID, "err", err)
		return
	}
	handle, err := h.Launch(d.ctx, spec)
	if err != nil {
		d.log.Error("dispatcher: launch failed", "session", sessionID, "harness", sess.Harness, "err", err)
		return
	}
	if err := h.Run(d.ctx, handle); err != nil {
		// Infrastructure failure: log and leave the session visibly
		// running — an operator sees it, a re-Kick retries it.
		d.log.Error("dispatcher: run failed", "session", sessionID, "harness", sess.Harness, "err", err)
	}
}

func (d *Dispatcher) workerSpec(ctx context.Context, sess *store.Session) (WorkerSpec, error) {
	v, err := d.store.GetAgentVersion(ctx, sess.AgentID, sess.AgentVersion)
	if err != nil {
		return WorkerSpec{}, err
	}
	return WorkerSpec{
		SessionID:    sess.ID,
		AgentID:      sess.AgentID,
		AgentVersion: sess.AgentVersion,
		Config:       v.Config,
	}, nil
}

// tokenJSON is a small helper for adapters minting checkpoint tokens.
func tokenJSON(harnessName string, v any) (CheckpointToken, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return CheckpointToken{}, fmt.Errorf("marshal checkpoint: %w", err)
	}
	return CheckpointToken{Harness: harnessName, Data: data}, nil
}
