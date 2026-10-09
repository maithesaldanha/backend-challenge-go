CREATE INDEX wallet_ledger_entries_wallet_cursor_idx
    ON wallet_ledger_entries (wallet_id, created_at DESC, id DESC);
