package store

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/warren-wupeng/agentd/internal/agentderr"
)

// EventListener tails agentd_events notifications on ONE dedicated
// connection (ADR-003): LISTEN/NOTIFY is the low-latency wake path, never
// the source of truth — a notification missed during a listener blip is
// recovered by the client's replay-and-dedupe reconnect contract, and the
// SSE handler re-queries on every wake regardless.
type EventListener struct {
	databaseURL string

	mu   sync.Mutex
	conn *pgx.Conn
	subs map[string]map[chan struct{}]struct{}
	dead bool // set when the pump exits for good
}

// NewEventListener connects and starts listening. Cancel ctx to stop;
// Close releases the connection.
func NewEventListener(ctx context.Context, databaseURL string) (*EventListener, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, agentderr.Internal(err)
	}
	if _, err := conn.Exec(ctx, `LISTEN agentd_events`); err != nil {
		_ = conn.Close(ctx)
		return nil, agentderr.Internal(err)
	}
	l := &EventListener{databaseURL: databaseURL, conn: conn,
		subs: map[string]map[chan struct{}]struct{}{}}
	go l.pump(ctx)
	return l, nil
}

// Subscribe returns a wake channel for one session. Wakes coalesce: a
// pending wake is never queued twice, and a wake that finds no new rows
// is a harmless no-op read.
func (l *EventListener) Subscribe(sessionID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs[sessionID] == nil {
		l.subs[sessionID] = map[chan struct{}]struct{}{}
	}
	l.subs[sessionID][ch] = struct{}{}
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if set := l.subs[sessionID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(l.subs, sessionID)
			}
		}
	}
}

// Close releases the listener connection and stops the pump.
func (l *EventListener) Close() error {
	l.mu.Lock()
	l.dead = true
	l.mu.Unlock()
	return l.conn.Close(context.Background())
}

func (l *EventListener) pump(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		notification, err := l.conn.WaitForNotification(ctx)
		if err == nil {
			l.wake(notification.Payload)
			continue
		}
		if ctx.Err() != nil {
			return // shutdown
		}
		// Connection dropped. Notifications lost in between are
		// tolerable by design (see type comment); reconnect with backoff
		// until the context dies.
		if !l.reconnect(ctx, 250*time.Millisecond, 5*time.Second) {
			return
		}
	}
}

func (l *EventListener) wake(sessionID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.subs[sessionID] {
		select {
		case ch <- struct{}{}:
		default: // a wake is already pending
		}
	}
}

// reconnect replaces the dead connection, retrying with capped backoff
// until success or ctx shutdown. Returns false when it gave up.
func (l *EventListener) reconnect(ctx context.Context, initial, max time.Duration) bool {
	wait := initial
	for {
		l.mu.Lock()
		dead := l.dead
		l.mu.Unlock()
		if dead || ctx.Err() != nil {
			return false
		}
		conn, err := pgx.Connect(ctx, l.databaseURL)
		if err == nil {
			if _, err := conn.Exec(ctx, `LISTEN agentd_events`); err == nil {
				l.mu.Lock()
				l.conn = conn
				l.mu.Unlock()
				return true
			}
			_ = conn.Close(ctx)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		if wait *= 2; wait > max {
			wait = max
		}
	}
}
