# Architecture decisions

## Domain and money

The domain packages do not depend on HTTP, SQS, Fx, or PostgreSQL. `Money` stores signed `int64` minor units and validates uppercase currency codes against ISO 4217 using `golang.org/x/text/currency`. External amounts use a fixed two-decimal string representation. The initial database mapping is `BIGINT` minor units plus a three-character currency code.

## Persistence design

The initial schema uses PostgreSQL constraints for wallet uniqueness, non-negative balances, operation idempotency keys, provider transaction IDs, ledger arithmetic, and opening uniqueness. A wallet update, transaction result, ledger entry, inbox completion, and outbox events must be written in one SQL transaction where applicable.

The PostgreSQL adapter uses `database/sql` and explicit SQL with the pgx v5 standard-library driver. Its Fx module validates the connection settings, pings PostgreSQL during startup, and closes the pool during shutdown. Money maps to `BIGINT` minor units and a three-character currency code.

Writers should serialize by wallet row using `SELECT ... FOR UPDATE`. This keeps independent wallets parallel and prevents two database processes from spending the same balance. The database constraints remain the final guard if application code or another writer violates the expected sequence.

Application persistence ports expose one `UnitOfWork` per SQL transaction. The wallet lock, idempotency lookup, wager state change, wallet update, ledger append, and outbox writes must use repositories from that same unit. `WalletRepository.Save` receives the version read before mutation so the SQL update can also use a version predicate.

Ledger rows reject updates and deletes through a trigger. The application database role must not own the schema or have privileges to disable triggers, truncate the ledger, or alter tables.

## Idempotency and messaging

External transaction IDs and idempotency keys are unique per provider. The schema stores SHA-256 hashes as lowercase hexadecimal text. HTTP and SQS use the shared wager payload hash over JSON fields in alphabetical key order (`externalTransactionId`, `gameId`, `kind`, `money`, `playerId`, `providerId`, `referenceExternalTransactionId`, `roundId`, `walletId`); it excludes the idempotency key and transport metadata. Money is serialized using its decimal-string JSON representation.

Inbox rows use `(consumer_name, message_id)` as their key and store a SHA-256 hash of the raw message body. The SQS consumer registers, handles the wager, and marks the inbox complete inside one PostgreSQL transaction. Nested application use cases reuse the transaction context, so inbox completion, financial state, ledger, and outbox commit or roll back together. Outbox rows preserve event identity and payload while delivery attempts, leases, and publication timestamps remain mutable for retries and multiple publishers.

## Authentication and wallet HTTP access

Keycloak issues OAuth 2.0 client-credentials access tokens to service accounts. The API verifies RS256 signatures from the configured JWKS URL and requires the configured issuer, audience, expiration, subject, and authorized party. The verifier reads realm roles from `realm_access.roles` and provider identity from `provider_id`. Configure an audience mapper so the access token contains this service's audience. `POST /wallets` requires `wallet:write`; provider wager submission requires `wager:write` and takes its provider identity exclusively from the verified token. The local Compose realm grants both roles and uses a fixed `local-provider` claim for exercising the flow. Production clients must receive only the roles appropriate to their identity. Use HTTPS for Keycloak and JWKS outside the local Docker network.

The API reads `DATABASE_URL`, `KEYCLOAK_ISSUER_URL`, `KEYCLOAK_JWKS_URL`, `OIDC_AUDIENCE`, `SQS_ENABLED`, `SQS_QUEUE_URL`, `SQS_INPUT_QUEUE_URL`, `AWS_REGION`, and optional `SQS_ENDPOINT_URL` from the environment. `HTTP_ADDR` defaults to `:8080`. Public `GET /health/live` reports whether the HTTP process responds. Public `GET /health/ready` probes PostgreSQL and the configured SQS queue with a two-second timeout and returns `503` when either is unavailable. `POST /wallets` accepts a UUID `playerId` and a `Money` object, and returns `201`; malformed input returns `400`, missing or invalid authentication returns `401`, missing `wallet:write` returns `403`, duplicate player/currency wallet returns `409`, and transient database errors return `503`. Wallet reads, reconciliation, and transaction lookup by internal ID use `wallet:write`; provider external-ID lookup requires `wager:write` and matches the provider identity from the token to the route. Ledger pagination uses keyset order `(created_at DESC, id DESC)` and an opaque base64url JSON cursor. `POST /wallets/{walletId}/reconciliation` reads wallet balance and signed ledger aggregate in one SQL statement, so both values share a PostgreSQL statement snapshot. The endpoint is read-only; differences are returned, logged, and counted by the `wallet_reconciliation_divergences_total` counter exposed at `GET /metrics`. `POST /wagering/transactions` accepts `BET`, `WIN`, `LOSS`, `REFUND`, and `ROLLBACK`; omission of `kind` is retained as compatibility behavior for `BET`. It requires `Idempotency-Key`, derives `providerId` from the token, returns `201` on processing, `202` when waiting for a reference, `422` for a persisted business rejection, `409` for idempotency conflicts, and `503` for transient database errors. `WIN` may reference a compatible processed `BET`; `REFUND` must reference a processed `BET`; `ROLLBACK` may reference a processed `BET`, `WIN`, or `REFUND`. Missing or pending references are durably recorded as `PENDING_REFERENCE` and emit a pending-reference outbox event; the retry worker processes up to 12 attempts with exponential backoff from one second, capped at one hour. `LOSS` requires a zero amount and emits no balance-change event or ledger row. The result balance is persisted for replay.

## Reversals

`WIN` may optionally reference a processed `BET`; when it does, the provider, player, wallet, currency, and round must match. The payout amount may differ from the bet.

`REFUND` can reference only a processed `BET`. `ROLLBACK` can reference a processed `BET`, `WIN`, or `REFUND`. The amount, provider, player, wallet, currency, and round must match the referenced transaction.

A processed bet may have one successful direct reversal: either `REFUND` or `ROLLBACK`, but not both. A `ROLLBACK` of a processed `REFUND` is allowed because it removes the refund credit; it does not permit a second refund of the original bet. The database uses partial unique indexes as a concurrent final guard.

## SQS consumer

The inbound worker reads one FIFO message at a time with long polling and a 60-second visibility timeout. Each message is processed within a 45-second deadline. The application hashes the raw body and registers `(consumer_name, message_id)` in `consumer_inbox`; it invokes the existing wager use case within the same transaction context, then marks the inbox complete. The message is deleted only after the SQL commit. Failed messages receive exponential visibility delays from one to sixteen seconds and the queue redrive policy moves them to the FIFO DLQ after five receives. HTTP and SQS use the same canonical wager payload hash and durable idempotency key.

## Current implementation and verification boundary

The implemented application use cases include wallet opening and reads, ledger pagination, reconciliation, transaction lookup, `BET`, `LOSS`, `WIN`, `REFUND`, `ROLLBACK`, pending-reference retries, SQS consumption, and outbox dispatch. The PostgreSQL adapter uses one `sql.Tx` per unit of work. Wallet opening with a positive initial balance writes the wallet, internal `OPENING` transaction, opening ledger row, and two outbox events atomically. A zero-balance wallet has no opening transaction or ledger row.

Ledger reads validate rehydrated entries and use the `(wallet_id, created_at DESC, id DESC)` index for keyset pagination. Reconciliation compares the stored balance with the ledger aggregate in one SQL statement. The outbox dispatcher claims work with `FOR UPDATE SKIP LOCKED`, uses leases, and retries failed publishes. The inbound SQS worker commits its inbox record, wager result, ledger, and outbox events together, and deletes the message only after commit.

Automated coverage currently includes unit tests for money, wallet and wager domain behavior; application tests for bets, losses, reversals and pending-reference retry; SQS message decoding and payload hashing; and PostgreSQL integration tests for inbox/idempotency/atomicity and concurrent wager processing across three independent processes. PostgreSQL integration tests are gated by `TEST_DATABASE_URL` and create temporary schemas in that database.

The repository does not yet have end-to-end tests combining Keycloak, HTTP and PostgreSQL; tests against a running SQS broker; controlled crash/restart tests; tests for competing outbox publishers; or the full set of cross-transport duplicate, DLQ, retry-expiry and multi-instance recovery scenarios from the challenge. The LocalStack Compose profile requires a valid `LOCALSTACK_AUTH_TOKEN`; without it the SQS service cannot be used. These are known verification gaps, not guarantees established by the current test suite.

On the user's Windows environment, the latest `go test ./...` run passed every package except `internal/application/sqsconsumer`, whose generated test executable was blocked by Application Control. In an earlier attempt, the same policy also blocked `internal/domain/wager`; its tests passed in the latest run. The PostgreSQL integration tests, application wager tests, and the money and wallet domain tests passed. `go test -race` could not start because this Go installation has `CGO_ENABLED=0`; race testing requires CGO and a configured C compiler. Re-run the complete suite and race checks in an environment with those prerequisites before treating them as green.
