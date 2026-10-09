# Architecture decisions

## Domain and money

The domain packages do not depend on HTTP, SQS, Fx, or PostgreSQL. `Money` stores signed `int64` minor units and a three-letter uppercase currency code. External amounts use a fixed two-decimal string representation. The initial database mapping is `BIGINT` minor units plus a three-character currency code.

## Persistence design

The initial schema uses PostgreSQL constraints for wallet uniqueness, non-negative balances, operation idempotency keys, provider transaction IDs, ledger arithmetic, and opening uniqueness. A wallet update, transaction result, ledger entry, inbox completion, and outbox events must be written in one SQL transaction where applicable.

The PostgreSQL adapter uses `database/sql` and explicit SQL with the pgx v5 standard-library driver. Its Fx module validates the connection settings, pings PostgreSQL during startup, and closes the pool during shutdown. Money maps to `BIGINT` minor units and a three-character currency code.

Writers should serialize by wallet row using `SELECT ... FOR UPDATE`. This keeps independent wallets parallel and prevents two database processes from spending the same balance. The database constraints remain the final guard if application code or another writer violates the expected sequence.

Application persistence ports expose one `UnitOfWork` per SQL transaction. The wallet lock, idempotency lookup, wager state change, wallet update, ledger append, and outbox writes must use repositories from that same unit. `WalletRepository.Save` receives the version read before mutation so the SQL update can also use a version predicate.

Ledger rows reject updates and deletes through a trigger. The application database role must not own the schema or have privileges to disable triggers, truncate the ledger, or alter tables.

## Idempotency and messaging

External transaction IDs and idempotency keys are unique per provider. The schema stores SHA-256 hashes as lowercase hexadecimal text. The HTTP `BET` adapter hashes JSON fields in alphabetical key order (`externalTransactionId`, `gameId`, `kind`, `money`, `playerId`, `providerId`, `roundId`, `walletId`); it excludes the idempotency key and transport metadata. Money is serialized using its decimal-string JSON representation. An eventual SQS adapter must use this same payload definition.

Inbox rows use `(consumer_name, message_id)` as their key. Inbox completion and the financial result commit together. Outbox rows preserve event identity and payload while delivery attempts, leases, and publication timestamps remain mutable for retries and multiple publishers.

## Authentication and wallet HTTP access

Keycloak issues OAuth 2.0 client-credentials access tokens to service accounts. The API verifies RS256 signatures from the configured JWKS URL and requires the configured issuer, audience, expiration, subject, and authorized party. The verifier reads realm roles from `realm_access.roles` and provider identity from `provider_id`. Configure an audience mapper so the access token contains this service's audience. `POST /wallets` requires `wallet:write`; provider bet submission requires `wager:write` and takes its provider identity exclusively from the verified token. The local Compose realm grants both roles and uses a fixed `local-provider` claim for exercising the flow. Production clients must receive only the roles appropriate to their identity. Use HTTPS for Keycloak and JWKS outside the local Docker network.

The API reads `DATABASE_URL`, `KEYCLOAK_ISSUER_URL`, `KEYCLOAK_JWKS_URL`, and `OIDC_AUDIENCE` from the environment. `HTTP_ADDR` defaults to `:8080`. `POST /wallets` accepts a UUID `playerId` and a `Money` object, and returns `201`; malformed input returns `400`, missing or invalid authentication returns `401`, missing `wallet:write` returns `403`, duplicate player/currency wallet returns `409`, and transient database errors return `503`. `POST /wagering/transactions` accepts `BET` and `LOSS`; omission of `kind` is retained as compatibility behavior for `BET`. It requires `Idempotency-Key`, derives `providerId` from the token, returns `201` on processing, `422` for a persisted business rejection, `409` for idempotency conflicts, and `503` for transient database errors. `LOSS` requires a zero amount and emits no balance-change event or ledger row. The result balance is persisted for replay.

## Reversals

`WIN` may optionally reference a processed `BET`; when it does, the provider, player, wallet, currency, and round must match. The payout amount may differ from the bet.

`REFUND` can reference only a processed `BET`. `ROLLBACK` can reference a processed `BET`, `WIN`, or `REFUND`. The amount, provider, player, wallet, currency, and round must match the referenced transaction.

A processed bet may have one successful direct reversal: either `REFUND` or `ROLLBACK`, but not both. A `ROLLBACK` of a processed `REFUND` is allowed because it removes the refund credit; it does not permit a second refund of the original bet. The database uses partial unique indexes as a concurrent final guard.

## Current implementation boundary

`OpenWallet`, `ProcessBet`, and `ProcessLoss` use the persistence ports. Wallet opening writes only the wallet for a zero balance; a positive opening also writes the `OPENING` transaction, initial credit ledger entry, and two outbox events in the same transaction. `BET` checks idempotent replays, locks the wallet, and atomically commits the wager, balance, ledger, and resulting outbox events. `LOSS` locks and verifies the wallet, writes the zero-value wager and processed event, and leaves wallet version and ledger unchanged. The SQL adapter implements these ports with one `sql.Tx` per unit of work. Fx modules provide the database pool, Keycloak authenticator, and HTTP server lifecycle. The wallet opening route requires `wallet:write`; wager routes require `wager:write`. Remaining work includes `WIN`, `REFUND`, `ROLLBACK`, reads, retry workers, event publication, SQS, health checks, and comprehensive integration tests. The local Compose stack has not yet been exercised against running containers.
