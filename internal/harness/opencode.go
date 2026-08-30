// OpenCode adapter — the first external harness (ADR-004). Drives an
// external OpenCode server over its HTTP API + SSE event bus; no forks,
// no patches, we are just one more client. In M4 the server's location
// is configuration (OPENCODE_URL), matching how container workers will
// run it; the control plane does not manage the process.
//
// WIRE CONTRACT: the shapes below encode our reading of OpenCode's
// server API. They are pinned by the fake server in opencode_test.go —
// until the adapter is validated against a live opencode instance it is
// experimental (conformance green is necessary, not sufficient).
package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/warren-wupeng/agentd/internal/model"
	"github.com/warren-wupeng/agentd/internal/policy"
	"github.com/warren-wupeng/agentd/internal/store"
)

type OpenCode struct {
	baseURL string
	client  *http.Client
	policy  policy.Engine
	st      *store.Store

	mu        sync.Mutex
	interrupt map[uuid.UUID]context.CancelFunc
}

func NewOpenCode(baseURL string, st *store.Store, pol policy.Engine) *OpenCode {
	return &OpenCode{
		baseURL:   strings.TrimRight(baseURL, "/"),
		client:    &http.Client{Timeout: 5 * time.Minute},
		policy:    pol,
		st:        st,
		interrupt: map[uuid.UUID]context.CancelFunc{},
	}
}

func (o *OpenCode) Name() string { return "opencode" }

func (o *OpenCode) Capabilities() CapabilitySet {
	return CapabilitySet{Hooks: true, Streaming: true, PermissionDelegate: true}
}

// --- wire types (the contract; see package comment) ---

type ocSessionResp struct {
	ID string `json:"id"`
}

type ocMessageReq struct {
	Message ocMessage `json:"message"`
}

type ocMessage struct {
	Role  string   `json:"role"`
	Parts []ocPart `json:"parts"`
}

type ocPart struct {
	Type   string          `json:"type"` // "text" | "tool"
	Text   string          `json:"text,omitempty"`
	Tool   string          `json:"tool,omitempty"` // tool name for type=="tool"
	State  string          `json:"state,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output string          `json:"output,omitempty"`
}

type ocEvent struct {
	Type string          `json:"type"` // message.part.updated | permission.ask | session.idle
	Data json.RawMessage `json:"data"`
}

type ocEventData struct {
	SessionID string          `json:"sessionID"`
	MessageID string          `json:"messageID"`
	Role      string          `json:"role"`
	Part      ocPart          `json:"part"`
	ID        string          `json:"id"` // permission.ask id
	Provider  string          `json:"providerID"`
	Input     json.RawMessage `json:"input"` // permission input
}

// Launch attaches the OpenCode session for this agentd session,
// creating it if the log has no harness.launched mapping yet. The
// mapping is durable as an event — replay recovers it after restarts.
func (o *OpenCode) Launch(ctx context.Context, spec WorkerSpec) (Handle, error) {
	state, err := o.existingState(ctx, spec.SessionID)
	if err != nil {
		return Handle{}, err
	}
	if state != nil {
		return Handle{Spec: spec, HarnessState: state}, nil
	}

	var resp ocSessionResp
	if err := o.do(ctx, http.MethodPost, "/session", map[string]any{"title": "agentd " + spec.SessionID.String()}, &resp); err != nil {
		return Handle{}, fmt.Errorf("create opencode session: %w", err)
	}
	state, err = json.Marshal(map[string]any{"opencode_session_id": resp.ID})
	if err != nil {
		return Handle{}, err
	}
	if _, err := o.st.AppendEvent(ctx, spec.SessionID, store.EventHarnessLaunched, store.ActorSystem,
		json.RawMessage(state)); err != nil {
		return Handle{}, err
	}
	return Handle{Spec: spec, HarnessState: state}, nil
}

func (o *OpenCode) existingState(ctx context.Context, sessionID uuid.UUID) (json.RawMessage, error) {
	events, err := o.st.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		if ev.Type == store.EventHarnessLaunched {
			return ev.Payload, nil
		}
	}
	return nil, nil
}

// Run drives one OpenCode turn: unprocessed user messages go in as the
// prompt, the SSE bus is normalized into agentd events, permission asks
// are delegated to the agentd policy engine, session.idle parks the turn.
func (o *OpenCode) Run(ctx context.Context, h Handle) (err error) {
	var state struct {
		OpencodeSessionID string `json:"opencode_session_id"`
	}
	if err := json.Unmarshal(h.HarnessState, &state); err != nil {
		return fmt.Errorf("bad harness state: %w", err)
	}
	ocSess := state.OpencodeSessionID

	sess, err := o.st.GetSession(ctx, h.Spec.SessionID)
	if err != nil {
		return err
	}
	if sess.State == store.StateTerminated {
		return nil
	}
	if err := o.promote(ctx, h.Spec.SessionID); err != nil {
		return err
	}

	ictx, cancel := context.WithCancel(ctx)
	o.mu.Lock()
	o.interrupt[h.Spec.SessionID] = cancel
	o.mu.Unlock()
	defer func() {
		cancel()
		o.mu.Lock()
		delete(o.interrupt, h.Spec.SessionID)
		o.mu.Unlock()
	}()

	defer func() {
		if perr := o.park(ctx, h.Spec.SessionID, err); perr != nil && err == nil {
			err = perr
		}
	}()

	// Drain unprocessed user input into one prompt (rule-2 shape).
	events, err := o.st.ListEvents(ictx, h.Spec.SessionID, 0, 1_000_000)
	if err != nil {
		return err
	}
	var prompt strings.Builder
	for _, ev := range events {
		if ev.Type == store.EventMessageUser && ev.ProcessedAt == nil {
			var pl struct {
				Content []model.Block `json:"content"`
			}
			_ = json.Unmarshal(ev.Payload, &pl)
			for _, b := range pl.Content {
				if b.Type == model.BlockText {
					if prompt.Len() > 0 {
						prompt.WriteString("\n")
					}
					prompt.WriteString(b.Text)
				}
			}
			if _, err := o.st.ClaimEvent(ictx, h.Spec.SessionID, ev.ID); err != nil {
				return err
			}
		}
	}
	if prompt.Len() == 0 {
		return nil // nothing to do; park as-is
	}

	msgReq := ocMessageReq{Message: ocMessage{Role: "user", Parts: []ocPart{{Type: "text", Text: prompt.String()}}}}
	var msgResp ocSessionResp
	if err := o.do(ictx, http.MethodPost, "/session/"+ocSess+"/message", msgReq, &msgResp); err != nil {
		return fmt.Errorf("send prompt: %w", err)
	}

	return o.consume(ictx, h.Spec.SessionID, ocSess)
}

// consume reads the SSE bus until session.idle, normalizing into events.
func (o *OpenCode) consume(ctx context.Context, sessionID uuid.UUID, ocSess string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/event?sessionID="+ocSess, nil)
	if err != nil {
		return err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe to opencode events: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode event stream returned %s", resp.Status)
	}

	// The permission.ask id and the tool part have no join key we can
	// rely on — permissions arrive immediately before their tool part,
	// so the LAST verdict applies to the NEXT tool.requested.
	var lastVerdict *policy.Verdict
	denied := false
	toolUseID := 0

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev ocEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("decode opencode event: %w", err)
		}
		var d ocEventData
		_ = json.Unmarshal(ev.Data, &d)
		if d.SessionID != "" && d.SessionID != ocSess {
			continue
		}

		switch ev.Type {
		case "permission.ask":
			// ADR-004's strongest governance surface: the harness
			// delegates its permission decision to agentd's engine.
			input := d.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			verdict := o.policy.Check(d.Provider, input)
			lastVerdict = &verdict
			status := "once" // allow
			if verdict.Decision == policy.Deny || verdict.Decision == policy.Ask {
				// Deny rejects outright; an ask from a delegated permission
				// parks the turn (requires_action) — same contract as the
				// native loop's ask, decided by OUR engine.
				status = "reject"
				denied = true
			}
			if err := o.do(ctx, http.MethodPost, "/permission/"+d.ID, map[string]string{"status": status}, nil); err != nil {
				return fmt.Errorf("answer permission %s: %w", d.ID, err)
			}

		case "message.part.updated":
			part := d.Part
			switch part.Type {
			case "tool":
				if part.State == "running" {
					toolUseID++
					id := fmt.Sprintf("oc-%s-%d", strings.TrimPrefix(ocSess, "ses_"), toolUseID)
					verdict := o.policy.Check(part.Tool, part.Input)
					if lastVerdict != nil {
						verdict = *lastVerdict
						lastVerdict = nil
					}
					// Normalize to the native shape: the assistant's
					// intent (tool_use block) precedes the request.
					if err := o.append(sessionID, store.EventMessageAssistant, store.ActorAgent, map[string]any{
						"content": []model.Block{{
							Type: model.BlockToolUse, ID: id, Name: part.Tool, Input: part.Input,
						}},
						"harness": o.Name(),
					}); err != nil {
						return err
					}
					if err := o.append(sessionID, store.EventToolRequested, store.ActorSystem, map[string]any{
						"tool_use_id": id, "name": part.Tool, "input": part.Input, "verdict": verdict,
					}); err != nil {
						return err
					}
				}
				if part.State == "completed" {
					// pair with the most recent requested id by tool name
					id, err := o.lastRequestedID(ctx, sessionID, part.Tool)
					if err != nil {
						return err
					}
					if err := o.append(sessionID, store.EventToolCompleted, store.ActorSystem, map[string]any{
						"tool_use_id": id, "output": part.Output,
					}); err != nil {
						return err
					}
				}
			case "text":
				if d.Role == "assistant" && part.State == "completed" && part.Text != "" {
					if err := o.append(sessionID, store.EventMessageAssistant, store.ActorAgent, map[string]any{
						"content": []model.Block{model.TextBlock(part.Text)},
						"harness": o.Name(),
					}); err != nil {
						return err
					}
				}
			}

		case "session.idle":
			reason := store.StopEndTurn
			if denied {
				reason = store.StopRequiresAction
			}
			return o.append(sessionID, store.EventTurnCompleted, store.ActorSystem, map[string]any{
				"stop_reason": reason,
			})
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return sc.Err()
}

// lastRequestedID finds the newest tool.requested for a tool name that
// has no result yet — OpenCode tool parts carry no ids we can join on.
func (o *OpenCode) lastRequestedID(ctx context.Context, sessionID uuid.UUID, toolName string) (string, error) {
	events, err := o.st.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err != nil {
		return "", err
	}
	var requested []string
	hasResult := map[string]bool{}
	for _, ev := range events {
		var pl struct {
			ToolUseID string `json:"tool_use_id"`
			Name      string `json:"name"`
		}
		_ = json.Unmarshal(ev.Payload, &pl)
		switch ev.Type {
		case store.EventToolRequested:
			if pl.Name == toolName {
				requested = append(requested, pl.ToolUseID)
			}
		case store.EventToolCompleted, store.EventToolFailed:
			hasResult[pl.ToolUseID] = true
		}
	}
	for i := len(requested) - 1; i >= 0; i-- {
		if !hasResult[requested[i]] {
			return requested[i], nil
		}
	}
	return "", fmt.Errorf("no unanswered tool.requested for %q", toolName)
}

func (o *OpenCode) append(sessionID uuid.UUID, eventType string, actor store.Actor, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = o.st.AppendEvent(context.Background(), sessionID, eventType, actor, b)
	return err
}

func (o *OpenCode) promote(ctx context.Context, sessionID uuid.UUID) error {
	sess, err := o.st.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	switch sess.State {
	case store.StateRescheduling, store.StateIdle:
		_, _, err = o.st.TransitionSession(ctx, sessionID, store.StateRunning, nil)
		return err
	default:
		return nil
	}
}

// park ends the turn bookkeeping: idle transition with the stop_reason
// already recorded by turn.completed (or without one if nothing ran).
func (o *OpenCode) park(ctx context.Context, sessionID uuid.UUID, runErr error) error {
	if runErr != nil {
		return nil // leave visibly running; operator re-kicks
	}
	sess, err := o.st.GetSession(ctx, sessionID)
	if err != nil || sess.State != store.StateRunning {
		return err
	}
	var reason *store.StopReason
	events, err := o.st.ListEvents(ctx, sessionID, 0, 1_000_000)
	if err == nil {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].Type == store.EventTurnCompleted {
				var pl struct {
					StopReason string `json:"stop_reason"`
				}
				_ = json.Unmarshal(events[i].Payload, &pl)
				if pl.StopReason != "" {
					sr := store.StopReason(pl.StopReason)
					reason = &sr
				}
				break
			}
		}
	}
	_, _, err = o.st.TransitionSession(ctx, sessionID, store.StateIdle, reason)
	return err
}

func (o *OpenCode) Checkpoint(ctx context.Context, h Handle) (CheckpointToken, error) {
	var state struct {
		OpencodeSessionID string `json:"opencode_session_id"`
	}
	if err := json.Unmarshal(h.HarnessState, &state); err != nil {
		return CheckpointToken{}, err
	}
	events, err := o.st.ListEvents(ctx, h.Spec.SessionID, 0, 1_000_000)
	if err != nil {
		return CheckpointToken{}, err
	}
	last := int64(0)
	if len(events) > 0 {
		last = events[len(events)-1].Seq
	}
	// NOTE: the OpenCode Context Epoch (compaction rotates the provider
	// cache baseline) belongs here once readable — pins must not pretend
	// the baseline survived a compaction boundary.
	return tokenJSON(o.Name(), map[string]any{
		"opencode_session_id": state.OpencodeSessionID,
		"last_seq":            last,
	})
}

func (o *OpenCode) Resume(ctx context.Context, spec WorkerSpec, tok CheckpointToken) (Handle, error) {
	if tok.Harness != o.Name() {
		return Handle{}, fmt.Errorf("checkpoint minted by %q, not %q", tok.Harness, o.Name())
	}
	return o.Launch(ctx, spec) // OpenCode's own session persistence resumes
}

func (o *OpenCode) Interrupt(sessionID uuid.UUID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cancel, ok := o.interrupt[sessionID]; ok {
		cancel()
	}
}

func (o *OpenCode) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("opencode %s %s: %s: %.200s", method, path, resp.Status, buf.String())
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
