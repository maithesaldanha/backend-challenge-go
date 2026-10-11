ALTER TABLE outbox_events ADD COLUMN sequence_id BIGINT;

WITH ordered_events AS (
    SELECT event_id, row_number() OVER (ORDER BY created_at, event_id) AS sequence_id
    FROM outbox_events
)
UPDATE outbox_events AS target
SET sequence_id = ordered_events.sequence_id
FROM ordered_events
WHERE target.event_id = ordered_events.event_id;

CREATE SEQUENCE outbox_events_sequence_id_seq;
SELECT setval('outbox_events_sequence_id_seq', GREATEST(COALESCE((SELECT MAX(sequence_id) FROM outbox_events), 0) + 1, 1), false);

ALTER TABLE outbox_events
    ALTER COLUMN sequence_id SET DEFAULT nextval('outbox_events_sequence_id_seq'),
    ALTER COLUMN sequence_id SET NOT NULL,
    ADD CONSTRAINT outbox_events_sequence_id_key UNIQUE (sequence_id);

ALTER SEQUENCE outbox_events_sequence_id_seq OWNED BY outbox_events.sequence_id;

DROP INDEX outbox_events_pending_idx;
CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, sequence_id)
    WHERE published_at IS NULL;
