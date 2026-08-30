package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/warren-wupeng/agentd/internal/store"
	"github.com/warren-wupeng/agentd/internal/testutil"
)

var ctx = context.Background()

func TestMain(m *testing.M) { testutil.Main(m) }

func TestMigrateUpDownUp(t *testing.T) {
	url := testutil.DatabaseURL(t)
	if err := store.Migrate(url, "down"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := store.Migrate(url, "up"); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func TestAgentLifecycleAndImmutability(t *testing.T) {
	st := testutil.NewStore(t)

	cfgA := json.RawMessage(`{"model":"claude-sonnet-4-6","system_prompt":"v1 prompt"}`)
	a, v1, err := st.CreateAgent(ctx, "reviewer", "code review bot", cfgA)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("want version 1, got %d", v1.Version)
	}

	// Duplicate name → conflict.
	if _, _, err := st.CreateAgent(ctx, "reviewer", "", cfgA); err == nil {
		t.Fatal("expected conflict on duplicate name")
	}

	// New version; v1 must stay semantically identical (jsonb normalizes
	// bytes, so equality is deep, not bytewise).
	cfgB := json.RawMessage(`{"model":"claude-opus-4-8","system_prompt":"v2 prompt"}`)
	v2, err := st.CreateAgentVersion(ctx, a.ID, cfgB)
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("want version 2, got %d", v2.Version)
	}

	got1, err := st.GetAgentVersion(ctx, a.ID, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	var wantCfg, gotCfg map[string]any
	if err := json.Unmarshal(cfgA, &wantCfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got1.Config, &gotCfg); err != nil {
		t.Fatal(err)
	}
	if wantCfg["model"] != gotCfg["model"] || wantCfg["system_prompt"] != gotCfg["system_prompt"] {
		t.Fatalf("v1 config drifted: want %v, got %v", wantCfg, gotCfg)
	}

	latest, err := st.LatestAgentVersion(ctx, a.ID)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.Version != 2 {
		t.Fatalf("want latest 2, got %d", latest.Version)
	}
}

// The DB trigger, not just the app, enforces version immutability — a raw
// UPDATE must fail even if future code tries one.
func TestAgentVersionImmutableTrigger(t *testing.T) {
	st := testutil.NewStore(t)

	a, _, err := st.CreateAgent(ctx, "immutable", "", json.RawMessage(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, testutil.DatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(ctx,
		`UPDATE agent_versions SET config = '{"model":"hacked"}' WHERE agent_id = $1`, a.ID)
	if err == nil {
		t.Fatal("expected trigger to reject UPDATE on agent_versions")
	}
}

func TestDeleteAgentConflictWithSessions(t *testing.T) {
	st := testutil.NewStore(t)

	a, _, err := st.CreateAgent(ctx, "pinned", "", json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateSession(ctx, a.ID, 0, "native"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, a.ID); err == nil {
		t.Fatal("expected conflict deleting agent with live sessions")
	}

	b, _, err := st.CreateAgent(ctx, "free", "", json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAgent(ctx, b.ID); err != nil {
		t.Fatalf("delete without sessions: %v", err)
	}
	if _, err := st.GetAgent(ctx, b.ID); err == nil {
		t.Fatal("expected 404 after delete")
	}
	if vs, err := st.ListAgentVersions(ctx, b.ID); err == nil {
		t.Fatalf("expected 404 listing versions of deleted agent, got %v", vs)
	}
}

func TestSessionPinsVersion(t *testing.T) {
	st := testutil.NewStore(t)

	a, _, err := st.CreateAgent(ctx, "pinbot", "", json.RawMessage(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	sess, created, err := st.CreateSession(ctx, a.ID, 1, "native")
	if err != nil {
		t.Fatal(err)
	}
	if sess.AgentVersion != 1 || sess.State != store.StateRescheduling {
		t.Fatalf("bad session: %+v", sess)
	}
	if created.Type != store.EventSessionCreated || created.Actor != store.ActorSystem {
		t.Fatalf("bad created event: %+v", created)
	}

	// v2 exists now; the session must stay pinned to v1.
	if _, err := st.CreateAgentVersion(ctx, a.ID, json.RawMessage(`{"model":"m2"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentVersion != 1 {
		t.Fatalf("session drifted to version %d", got.AgentVersion)
	}

	// Malformed harness names are rejected at the store (format check);
	// the KNOWN-harness set is API-layer policy (M4 harness registry).
	if _, _, err := st.CreateSession(ctx, a.ID, 0, "Bad_Harness"); err == nil {
		t.Fatal("expected invalid-input on malformed harness")
	}
	if _, _, err := st.CreateSession(ctx, a.ID, 0, ""); err == nil {
		t.Fatal("expected invalid-input on empty harness")
	}
}

func TestTransitionStateMachine(t *testing.T) {
	st := testutil.NewStore(t)

	a, _, _ := st.CreateAgent(ctx, "fsm", "", json.RawMessage(`{"model":"m"}`))
	sess, _, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}

	// Illegal: rescheduling → idle.
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateIdle, nil); err == nil {
		t.Fatal("expected invalid transition rescheduling→idle")
	}

	// Legal chain: → running → idle(stop_reason) → running → terminated.
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateRunning, nil); err != nil {
		t.Fatalf("→running: %v", err)
	}
	sr := store.StopRequiresAction
	s2, _, err := st.TransitionSession(ctx, sess.ID, store.StateIdle, &sr)
	if err != nil {
		t.Fatalf("→idle: %v", err)
	}
	if s2.StopReason == nil || *s2.StopReason != store.StopRequiresAction {
		t.Fatalf("stop_reason not persisted: %+v", s2)
	}
	// stop_reason with non-idle target is rejected.
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateRunning, &sr); err == nil {
		t.Fatal("expected invalid-input for stop_reason on →running")
	}
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateRunning, nil); err != nil {
		t.Fatalf("idle→running: %v", err)
	}
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateTerminated, nil); err != nil {
		t.Fatalf("→terminated: %v", err)
	}
	// terminated is final.
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateRunning, nil); err == nil {
		t.Fatal("expected terminal-state rejection")
	}

	// G1 structural check: every transition produced a state_changed event,
	// plus exactly one session.created, in order.
	events, err := st.ListEvents(ctx, sess.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var created, transitions int
	var lastFrom, lastTo string
	for _, ev := range events {
		switch ev.Type {
		case store.EventSessionCreated:
			created++
		case store.EventSessionStateChanged:
			transitions++
			var p struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if transitions > 1 && p.From != lastTo {
				t.Fatalf("event chain broken: previous to=%q, next from=%q", lastTo, p.From)
			}
			lastFrom, lastTo = p.From, p.To
		}
	}
	if created != 1 {
		t.Fatalf("want 1 created event, got %d", created)
	}
	if transitions != 4 {
		t.Fatalf("want 4 transition events, got %d", transitions)
	}
	if lastFrom != "running" || lastTo != "terminated" {
		t.Fatalf("chain ended at %q→%q, want running→terminated", lastFrom, lastTo)
	}
}

func TestReplayAndClaim(t *testing.T) {
	st := testutil.NewStore(t)

	a, _, _ := st.CreateAgent(ctx, "replay", "", json.RawMessage(`{"model":"m"}`))
	sess, created, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}

	const n = 50
	for i := 0; i < n; i++ {
		payload, _ := json.Marshal(map[string]any{"i": i})
		if _, err := st.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser, payload); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Replay from the 20th appended event: order strictly by seq.
	all, err := st.ListEvents(ctx, sess.ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n+1 { // +1 for session.created
		t.Fatalf("want %d events, got %d", n+1, len(all))
	}
	pivot := all[20].Seq
	page, err := st.ListEvents(ctx, sess.ID, pivot, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != n+1-21 {
		t.Fatalf("want %d events after pivot, got %d", n+1-21, len(page))
	}
	for i := 1; i < len(page); i++ {
		if page[i].Seq <= page[i-1].Seq {
			t.Fatalf("seq not ascending at %d: %d <= %d", i, page[i].Seq, page[i-1].Seq)
		}
	}

	// Idempotent re-read: same query, same ids.
	again, err := st.ListEvents(ctx, sess.ID, pivot, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := range page {
		if page[i].ID != again[i].ID {
			t.Fatalf("replay not idempotent at %d", i)
		}
	}

	// Claim: first sets processed_at, second returns it unchanged.
	c1, err := st.ClaimEvent(ctx, sess.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c1.ProcessedAt == nil {
		t.Fatal("claim did not set processed_at")
	}
	c2, err := st.ClaimEvent(ctx, sess.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.ProcessedAt.Equal(*c1.ProcessedAt) {
		t.Fatalf("second claim mutated processed_at: %v → %v", c1.ProcessedAt, c2.ProcessedAt)
	}

	// Terminated sessions reject appends.
	if _, _, err := st.TransitionSession(ctx, sess.ID, store.StateTerminated, nil); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"late": true})
	if _, err := st.AppendEvent(ctx, sess.ID, store.EventMessageUser, store.ActorUser, payload); err == nil {
		t.Fatal("expected conflict appending to terminated session")
	}
}

// ADR-003's live tail: an append must wake exactly the subscribers of
// that session, and only after commit.
func TestEventListenerWakesOnAppend(t *testing.T) {
	url := testutil.DatabaseURL(t)
	if err := store.Migrate(url, "up"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := store.NewEventListener(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	st := testutil.NewStore(t)
	a, _, err := st.CreateAgent(ctx, "notify", "", json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	mine, _, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := st.CreateSession(ctx, a.ID, 0, "native")
	if err != nil {
		t.Fatal(err)
	}

	wakeMine, cancelMine := listener.Subscribe(mine.ID.String())
	defer cancelMine()
	wakeOther, cancelOther := listener.Subscribe(other.ID.String())
	defer cancelOther()

	// A wake from a PREVIOUS append (session.created during CreateSession)
	// may still be pending — drain both channels before the real append.
	select {
	case <-wakeMine:
	default:
	}
	select {
	case <-wakeOther:
	default:
	}

	if _, err := st.AppendEvent(ctx, mine.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[]}`)); err != nil {
		t.Fatal(err)
	}

	// Notification delivery is asynchronous — wait for it, briefly.
	select {
	case <-wakeMine:
	case <-time.After(2 * time.Second):
		t.Fatal("no wake for the appended session")
	}
	select {
	case <-wakeOther:
		t.Fatal("wake leaked to an unrelated session")
	case <-time.After(50 * time.Millisecond):
	}

	// Wakes coalesce: after a drain, a second append wakes again.
	if _, err := st.AppendEvent(ctx, mine.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakeMine:
	case <-time.After(2 * time.Second):
		t.Fatal("second append did not wake")
	}

	// After unsubscribe: no wake, and Publish must not block the append.
	cancelMine()
	if _, err := st.AppendEvent(ctx, mine.ID, store.EventMessageUser, store.ActorUser,
		json.RawMessage(`{"content":[]}`)); err != nil {
		t.Fatal(err)
	}
}
