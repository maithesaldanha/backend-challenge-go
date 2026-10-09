CREATE TABLE wallets (
    id UUID PRIMARY KEY,
    player_id UUID NOT NULL,
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor BIGINT NOT NULL CHECK (balance_minor >= 0),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    CONSTRAINT wallets_player_currency_unique UNIQUE (player_id, currency),
    CONSTRAINT wallets_id_currency_unique UNIQUE (id, currency)
);

CREATE TABLE wager_transactions (
    id UUID PRIMARY KEY,
    source TEXT NOT NULL CHECK (source IN ('INTERNAL', 'PROVIDER')),
    provider_id TEXT,
    external_transaction_id TEXT,
    idempotency_key TEXT,
    payload_hash TEXT,
    wallet_id UUID NOT NULL,
    player_id UUID NOT NULL,
    round_id TEXT,
    game_id TEXT,
    kind TEXT NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    reference_external_transaction_id TEXT,
    reference_transaction_id UUID,
    reference_kind TEXT CHECK (reference_kind IN ('BET', 'WIN', 'REFUND')),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    failure_code TEXT,
    result_balance_minor BIGINT CHECK (result_balance_minor >= 0),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    CONSTRAINT wager_transactions_wallet_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency) ON DELETE RESTRICT,
    CONSTRAINT wager_transactions_external_or_internal_ck CHECK (
        (
            source = 'PROVIDER'
            AND kind IN ('BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')
            AND provider_id IS NOT NULL AND btrim(provider_id) <> ''
            AND external_transaction_id IS NOT NULL AND btrim(external_transaction_id) <> ''
            AND idempotency_key IS NOT NULL AND btrim(idempotency_key) <> ''
            AND payload_hash IS NOT NULL
            AND length(payload_hash) = 64
            AND payload_hash ~ '^[0-9a-f]{64}$'
            AND round_id IS NOT NULL AND btrim(round_id) <> ''
            AND game_id IS NOT NULL AND btrim(game_id) <> ''
        )
        OR
        (
            source = 'INTERNAL'
            AND kind = 'OPENING'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND payload_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL
            AND reference_kind IS NULL
            AND status = 'PROCESSED'
            AND amount_minor > 0
            AND result_balance_minor = amount_minor
            AND failure_code IS NULL
        )
    ),
    CONSTRAINT wager_transactions_amount_ck CHECK (
        (kind = 'LOSS' AND amount_minor = 0)
        OR (kind = 'OPENING' AND amount_minor > 0)
        OR (kind IN ('BET', 'WIN', 'REFUND', 'ROLLBACK') AND amount_minor > 0)
    ),
    CONSTRAINT wager_transactions_state_ck CHECK (
        (status IN ('PENDING', 'PENDING_REFERENCE') AND failure_code IS NULL AND result_balance_minor IS NULL)
        OR (status = 'PROCESSED' AND failure_code IS NULL AND result_balance_minor IS NOT NULL)
        OR (
            status IN ('REJECTED', 'FAILED')
            AND failure_code IS NOT NULL
            AND failure_code ~ '^[A-Z][A-Z0-9_]*$'
            AND result_balance_minor IS NULL
        )
    ),
    CONSTRAINT wager_transactions_reference_ck CHECK (
        (reference_transaction_id IS NULL AND reference_kind IS NULL)
        OR (reference_transaction_id IS NOT NULL AND reference_kind IS NOT NULL AND reference_external_transaction_id IS NOT NULL)
    ),
    CONSTRAINT wager_transactions_reference_external_ck CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL AND btrim(reference_external_transaction_id) <> '')
        OR (kind = 'WIN' AND (reference_external_transaction_id IS NULL OR btrim(reference_external_transaction_id) <> ''))
        OR (kind IN ('OPENING', 'BET', 'LOSS') AND reference_external_transaction_id IS NULL)
    ),
    CONSTRAINT wager_transactions_pending_reference_ck CHECK (
        status <> 'PENDING_REFERENCE'
        OR (
            (kind IN ('REFUND', 'ROLLBACK') OR kind = 'WIN' AND reference_external_transaction_id IS NOT NULL)
            AND reference_transaction_id IS NULL
        )
    ),
    CONSTRAINT wager_transactions_processed_reversal_ck CHECK (
        status <> 'PROCESSED'
        OR (kind NOT IN ('REFUND', 'ROLLBACK') AND NOT (kind = 'WIN' AND reference_external_transaction_id IS NOT NULL))
        OR reference_transaction_id IS NOT NULL
    ),
    CONSTRAINT wager_transactions_reference_kind_ck CHECK (
        reference_transaction_id IS NULL
        OR (kind = 'REFUND' AND reference_kind = 'BET')
        OR (kind = 'ROLLBACK' AND reference_kind IN ('BET', 'WIN', 'REFUND'))
        OR (kind = 'WIN' AND reference_kind = 'BET')
    ),
    CONSTRAINT wager_transactions_reference_fk
        FOREIGN KEY (
            reference_transaction_id,
            provider_id,
            reference_external_transaction_id,
            player_id,
            wallet_id,
            round_id,
            currency,
            reference_kind
        )
        REFERENCES wager_transactions (
            id,
            provider_id,
            external_transaction_id,
            player_id,
            wallet_id,
            round_id,
            currency,
            kind
        ) ON DELETE RESTRICT,
    CONSTRAINT wager_transactions_reference_target_unique UNIQUE (
        id,
        provider_id,
        external_transaction_id,
        player_id,
        wallet_id,
        round_id,
        currency,
        kind
    ),
    CONSTRAINT wager_transactions_id_wallet_currency_unique UNIQUE (id, wallet_id, currency)
);

CREATE UNIQUE INDEX wager_transactions_provider_external_id_unique
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE source = 'PROVIDER';

CREATE UNIQUE INDEX wager_transactions_provider_idempotency_key_unique
    ON wager_transactions (provider_id, idempotency_key)
    WHERE source = 'PROVIDER';

CREATE UNIQUE INDEX wager_transactions_one_opening_per_wallet
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

CREATE UNIQUE INDEX wager_transactions_one_reversal_per_type
    ON wager_transactions (reference_transaction_id, kind)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

CREATE UNIQUE INDEX wager_transactions_one_direct_bet_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND reference_kind = 'BET' AND kind IN ('REFUND', 'ROLLBACK');

CREATE INDEX wager_transactions_pending_work_idx
    ON wager_transactions (next_attempt_at, created_at)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');

CREATE INDEX wager_transactions_external_reference_idx
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE reference_external_transaction_id IS NOT NULL;

CREATE FUNCTION protect_wager_transaction_identity() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wager transactions cannot be deleted';
    END IF;
    IF ROW(
        OLD.id,
        OLD.source,
        OLD.provider_id,
        OLD.external_transaction_id,
        OLD.idempotency_key,
        OLD.payload_hash,
        OLD.wallet_id,
        OLD.player_id,
        OLD.round_id,
        OLD.game_id,
        OLD.kind,
        OLD.amount_minor,
        OLD.currency,
        OLD.reference_external_transaction_id,
        OLD.created_at
    ) IS DISTINCT FROM ROW(
        NEW.id,
        NEW.source,
        NEW.provider_id,
        NEW.external_transaction_id,
        NEW.idempotency_key,
        NEW.payload_hash,
        NEW.wallet_id,
        NEW.player_id,
        NEW.round_id,
        NEW.game_id,
        NEW.kind,
        NEW.amount_minor,
        NEW.currency,
        NEW.reference_external_transaction_id,
        NEW.created_at
    ) THEN
        RAISE EXCEPTION 'wager transaction identity and request are immutable';
    END IF;
    IF OLD.reference_transaction_id IS NOT NULL AND
        (NEW.reference_transaction_id IS DISTINCT FROM OLD.reference_transaction_id OR NEW.reference_kind IS DISTINCT FROM OLD.reference_kind) THEN
        RAISE EXCEPTION 'resolved transaction reference is immutable';
    END IF;
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'terminal wager transactions are immutable';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'wager transaction timestamp cannot move backwards';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transactions_protect_identity
    BEFORE UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION protect_wager_transaction_identity();

CREATE FUNCTION protect_wallet_identity_and_version() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wallets cannot be deleted';
    END IF;
    IF ROW(OLD.id, OLD.player_id, OLD.currency, OLD.created_at) IS DISTINCT FROM
        ROW(NEW.id, NEW.player_id, NEW.currency, NEW.created_at) THEN
        RAISE EXCEPTION 'wallet identity is immutable';
    END IF;
    IF OLD.balance_minor = NEW.balance_minor AND
        (NEW.version <> OLD.version OR NEW.updated_at IS DISTINCT FROM OLD.updated_at) THEN
        RAISE EXCEPTION 'wallet version and timestamp change only with balance';
    END IF;
    IF OLD.balance_minor <> NEW.balance_minor AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'wallet version must increment once per balance change';
    END IF;
    IF NEW.updated_at < OLD.updated_at THEN
        RAISE EXCEPTION 'wallet timestamp cannot move backwards';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallets_protect_identity_and_version
    BEFORE UPDATE OR DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION protect_wallet_identity_and_version();

CREATE TABLE wallet_ledger_entries (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL,
    transaction_id UUID NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_before_minor BIGINT NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor BIGINT NOT NULL CHECK (balance_after_minor >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallet_ledger_wallet_fk
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency) ON DELETE RESTRICT,
    CONSTRAINT wallet_ledger_transaction_fk
        FOREIGN KEY (transaction_id, wallet_id, currency)
        REFERENCES wager_transactions (id, wallet_id, currency) ON DELETE RESTRICT,
    CONSTRAINT wallet_ledger_wallet_transaction_unique UNIQUE (wallet_id, transaction_id),
    CONSTRAINT wallet_ledger_balance_math_ck CHECK (
        (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
        OR (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
    )
);

CREATE FUNCTION prevent_wallet_ledger_mutation() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wallet ledger entries cannot be deleted';
    END IF;
    RAISE EXCEPTION 'wallet ledger entries are immutable';
END;
$$;

CREATE TRIGGER wallet_ledger_no_update_or_delete
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION prevent_wallet_ledger_mutation();

CREATE TABLE consumer_inbox (
    consumer_name TEXT NOT NULL CHECK (btrim(consumer_name) <> ''),
    message_id TEXT NOT NULL CHECK (btrim(message_id) <> ''),
    message_hash TEXT NOT NULL CHECK (length(message_hash) = 64 AND message_hash ~ '^[0-9a-f]{64}$'),
    received_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CONSTRAINT consumer_inbox_pk PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT consumer_inbox_completion_ck CHECK (completed_at IS NULL OR completed_at >= received_at)
);

CREATE TABLE outbox_events (
    event_id UUID PRIMARY KEY,
    aggregate_id UUID NOT NULL,
    event_type TEXT NOT NULL CHECK (btrim(event_type) <> ''),
    correlation_id TEXT NOT NULL CHECK (btrim(correlation_id) <> ''),
    causation_id UUID,
    occurred_at TIMESTAMPTZ NOT NULL,
    event_version INTEGER NOT NULL CHECK (event_version > 0),
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_until TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT outbox_published_ck CHECK (published_at IS NULL OR published_at >= occurred_at)
);

CREATE INDEX outbox_events_pending_idx
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL;

CREATE FUNCTION prevent_outbox_payload_mutation() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'outbox events cannot be deleted';
    END IF;
    IF ROW(
        OLD.event_id,
        OLD.aggregate_id,
        OLD.event_type,
        OLD.correlation_id,
        OLD.causation_id,
        OLD.occurred_at,
        OLD.event_version,
        OLD.payload,
        OLD.created_at
    ) IS DISTINCT FROM ROW(
        NEW.event_id,
        NEW.aggregate_id,
        NEW.event_type,
        NEW.correlation_id,
        NEW.causation_id,
        NEW.occurred_at,
        NEW.event_version,
        NEW.payload,
        NEW.created_at
    ) THEN
        RAISE EXCEPTION 'outbox event payload is immutable';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'published outbox events cannot be unpublished';
    END IF;
    IF NEW.attempts < OLD.attempts THEN
        RAISE EXCEPTION 'outbox attempts cannot decrease';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_events_immutable_payload
    BEFORE UPDATE OR DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION prevent_outbox_payload_mutation();

CREATE FUNCTION protect_inbox_identity() RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'inbox messages cannot be deleted';
    END IF;
    IF ROW(OLD.consumer_name, OLD.message_id, OLD.message_hash, OLD.received_at) IS DISTINCT FROM
        ROW(NEW.consumer_name, NEW.message_id, NEW.message_hash, NEW.received_at) THEN
        RAISE EXCEPTION 'inbox message identity is immutable';
    END IF;
    IF OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at THEN
        RAISE EXCEPTION 'completed inbox messages cannot be reopened';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER consumer_inbox_protect_identity
    BEFORE UPDATE OR DELETE ON consumer_inbox
    FOR EACH ROW EXECUTE FUNCTION protect_inbox_identity();
