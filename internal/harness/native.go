package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/loop"
	"github.com/warren-wupeng/agentd/internal/store"
)

// Native is the reference implementation behind the Harness seam: the
// in-process loop from M2/M3. It exists to (a) validate the interface
// from the inside — if the native loop can't sit behind it, the seam is
// wrong — (b) cover local models, (c) demo policy-in-loop. It is a
// component, not the product (ADR-004).
type Native struct {
	deps *loop.Deps

	mu        sync.Mutex
	interrupt map[uuid.UUID]context.CancelFunc
}

func NewNative(deps *loop.Deps) *Native {
	return &Native{deps: deps, interrupt: map[uuid.UUID]context.CancelFunc{}}
}

func (n *Native) Name() string { return "native" }

func (n *Native) Capabilities() CapabilitySet {
	return CapabilitySet{Hooks: true, Streaming: true}
	// PermissionDelegate is false: native has no permission protocol to
	// delegate — its policy engine IS the in-loop gate.
}

func (n *Native) Launch(ctx context.Context, spec WorkerSpec) (Handle, error) {
	sess, err := n.deps.Store.GetSession(ctx, spec.SessionID)
	if err != nil {
		return Handle{}, err
	}
	if sess.AgentID != spec.AgentID || sess.AgentVersion != spec.AgentVersion {
		return Handle{}, fmt.Errorf("session %s is pinned to agent %s v%d, spec says %s v%d",
			spec.SessionID, sess.AgentID, sess.AgentVersion, spec.AgentID, spec.AgentVersion)
	}
	return Handle{Spec: spec}, nil // in-process: nothing to launch
}

// Run drives loop.Step until the turn parks — the transition discipline
// that used to live in loop.Runner, now behind the seam.
func (n *Native) Run(ctx context.Context, h Handle) error {
	sess, err := n.deps.Store.GetSession(ctx, h.Spec.SessionID)
	if err != nil {
		return err
	}
	switch sess.State {
	case store.StateRescheduling, store.StateIdle:
		if _, _, err := n.deps.Store.TransitionSession(ctx, h.Spec.SessionID, store.StateRunning, nil); err != nil {
			return err
		}
	case store.StateRunning:
		// crash recovery: run without re-transitioning
	case store.StateTerminated:
		return nil
	}

	ictx, cancel := context.WithCancel(ctx)
	n.mu.Lock()
	n.interrupt[h.Spec.SessionID] = cancel
	n.mu.Unlock()
	defer func() {
		cancel()
		n.mu.Lock()
		delete(n.interrupt, h.Spec.SessionID)
		n.mu.Unlock()
	}()

	for {
		outcome, err := loop.Step(ictx, n.deps, h.Spec.SessionID)
		if err != nil {
			return err // dispatcher logs; session stays visibly running
		}
		if outcome == loop.OutcomeContinue {
			continue
		}
		n.ensureIdle(ctx, h.Spec.SessionID)
		return nil
	}
}

func (n *Native) ensureIdle(ctx context.Context, sessionID uuid.UUID) {
	sess, err := n.deps.Store.GetSession(ctx, sessionID)
	if err != nil || sess.State != store.StateRunning {
		return
	}
	_, _, _ = n.deps.Store.TransitionSession(ctx, sessionID, store.StateIdle, nil)
}

// Checkpoint: for native, the log IS the resume — the token pins the
// last seq at mint time purely as an audit marker.
func (n *Native) Checkpoint(ctx context.Context, h Handle) (CheckpointToken, error) {
	events, err := n.deps.Store.ListEvents(ctx, h.Spec.SessionID, 0, 1_000_000)
	if err != nil {
		return CheckpointToken{}, err
	}
	last := int64(0)
	if len(events) > 0 {
		last = events[len(events)-1].Seq
	}
	data, _ := json.Marshal(map[string]int64{"last_seq": last})
	return CheckpointToken{Harness: n.Name(), Data: data}, nil
}

func (n *Native) Resume(ctx context.Context, spec WorkerSpec, tok CheckpointToken) (Handle, error) {
	if tok.Harness != n.Name() {
		return Handle{}, fmt.Errorf("checkpoint minted by %q, not %q", tok.Harness, n.Name())
	}
	// Replay from the log; the token's last_seq is informational.
	return n.Launch(ctx, spec)
}

func (n *Native) Interrupt(sessionID uuid.UUID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if cancel, ok := n.interrupt[sessionID]; ok {
		cancel()
	}
}
