-- DESTRUCTIVE: stop applications and back up first. Use only in disposable databases.
BEGIN;
DROP TABLE IF EXISTS outbox,inbox,wallet_ledger,wager_transactions,wallets,schema_migrations CASCADE;
DROP FUNCTION IF EXISTS immutable_record(),guard_wallet(),guard_transaction(),apply_ledger(),require_ledger(),guard_outbox() CASCADE;
COMMIT;
