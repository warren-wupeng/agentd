package loop

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/store"
)

// Runner owns the goroutine-per-active-session actors (design:
// "Concurrency model"). It holds no truth: the log decides what a session
// is doing; a goroutine is just an execution detail that keeps calling
// Step until the turn parks. Idle sessions cost nothing.
type Runner struct {
	ctx  context.Context
	deps *Deps

	mu     sync.Mutex
	active map[uuid.UUID]bool
	wg     sync.WaitGroup
}

func NewRunner(ctx context.Context, deps *Deps) *Runner {
	return &Runner{
		ctx:    ctx,
		deps:   deps,
		active: map[uuid.UUID]bool{},
	}
}

// Kick schedules the actor for a session. Idempotent and non-blocking:
// an already-active session is left alone (its next Step will see the new
// events), a parked one gets a fresh goroutine.
func (r *Runner) Kick(sessionID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[sessionID] {
		return
	}
	r.active[sessionID] = true
	r.wg.Add(1)
	go r.run(sessionID)
}

// Wait blocks until all actors have parked (used at shutdown).
func (r *Runner) Wait() {
	r.wg.Wait()
}

func (r *Runner) run(sessionID uuid.UUID) {
	defer func() {
		r.mu.Lock()
		delete(r.active, sessionID)
		r.mu.Unlock()
		r.wg.Done()
	}()

	log := r.deps.Log
	if log == nil {
		log = slog.Default()
	}

	// Promote to running unless the session is already there (a crashed
	// process leaves sessions stuck in running — Step recovers them).
	sess, err := r.deps.Store.GetSession(r.ctx, sessionID)
	if err != nil {
		log.Error("runner: session load failed", "session", sessionID, "err", err)
		return
	}
	switch sess.State {
	case store.StateRescheduling, store.StateIdle:
		if _, _, err := r.deps.Store.TransitionSession(r.ctx, sessionID, store.StateRunning, nil); err != nil {
			log.Error("runner: transition to running failed", "session", sessionID, "err", err)
			return
		}
	case store.StateRunning:
		// crash recovery path: run without re-transitioning
	case store.StateTerminated:
		return
	}

	for {
		outcome, err := Step(r.ctx, r.deps, sessionID)
		if err != nil {
			// Infrastructure failure inside a step: log it and park the
			// session visibly running — an operator sees it, a re-Kick
			// retries it. Never silently swallow.
			log.Error("runner: step failed", "session", sessionID, "err", err)
			return
		}
		switch outcome {
		case OutcomeContinue:
			continue
		case OutcomeParked, OutcomeNoop:
			r.ensureIdle(sessionID, log)
			return
		}
	}
}

// ensureIdle parks a running session that ended without a stop_reason
// (e.g. kicked with nothing to do). Turns that ended via finishTurn are
// already idle.
func (r *Runner) ensureIdle(sessionID uuid.UUID, log *slog.Logger) {
	sess, err := r.deps.Store.GetSession(r.ctx, sessionID)
	if err != nil {
		log.Error("runner: session reload failed", "session", sessionID, "err", err)
		return
	}
	if sess.State != store.StateRunning {
		return
	}
	if _, _, err := r.deps.Store.TransitionSession(r.ctx, sessionID, store.StateIdle, nil); err != nil {
		log.Error("runner: park failed", "session", sessionID, "err", err)
	}
}
