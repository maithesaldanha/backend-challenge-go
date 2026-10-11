DROP INDEX outbox_events_pending_idx;

ALTER TABLE outbox_events DROP COLUMN sequence_id;
DROP SEQUENCE IF EXISTS outbox_events_sequence_id_seq;

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL;
