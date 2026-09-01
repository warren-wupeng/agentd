-- Sessions are born ready, not "scheduled": a fresh session has no work
-- queued, so its initial state is idle. rescheduling is reserved for the
-- transient moment a kick is in flight (see loop.Runner). Also fold any
-- rows still sitting on the old creation default into idle — after a
-- restart a rescheduling kick is gone, so idle is the truthful reading of
-- a log that holds only session.created.
ALTER TABLE sessions ALTER COLUMN state SET DEFAULT 'idle';
UPDATE sessions SET state = 'idle' WHERE state = 'rescheduling';
