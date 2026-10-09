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

External transaction IDs and idempotency keys are unique per provider. The schema stores SHA-256 hashes as lowercase hexadecimal text; canonical payload construction and normalization must be shared by HTTP and SQS adapters.

Inbox rows use `(consumer_name, message_id)` as their key. Inbox completion and the financial result commit together. Outbox rows preserve event identity and payload while delivery attempts, leases, and publication timestamps remain mutable for retries and multiple publishers.

## Reversals

`WIN` may optionally reference a processed `BET`; when it does, the provider, player, wallet, currency, and round must match. The payout amount may differ from the bet.

`REFUND` can reference only a processed `BET`. `ROLLBACK` can reference a processed `BET`, `WIN`, or `REFUND`. The amount, provider, player, wallet, currency, and round must match the referenced transaction.

A processed bet may have one successful direct reversal: either `REFUND` or `ROLLBACK`, but not both. A `ROLLBACK` of a processed `REFUND` is allowed because it removes the refund credit; it does not permit a second refund of the original bet. The database uses partial unique indexes as a concurrent final guard.

## Current implementation boundary

The `ProcessBet` application use case handles synchronous bets through the persistence ports. It checks idempotent replays, locks the wallet row through the repository, and commits the wager, balance, ledger, and resulting outbox events together. The SQL adapter implements these ports with one `sql.Tx` per unit of work. The Fx module can provide the database pool and transactor; application entrypoint, configuration loading, other wager kinds, retry workers, event publishing, authentication, HTTP, and SQS remain to be added. The migration has not yet been applied against PostgreSQL in this workspace.
