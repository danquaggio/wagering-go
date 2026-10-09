BEGIN;
CREATE TABLE wallets (
 id uuid PRIMARY KEY, player_id uuid NOT NULL, currency text NOT NULL CHECK(currency IN ('BRL','USD','EUR')),
 balance bigint NOT NULL DEFAULT 0 CHECK(balance>=0), version bigint NOT NULL DEFAULT 1 CHECK(version>=1),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(player_id,currency), CHECK(updated_at>=created_at)
);
CREATE TABLE wager_transactions (
 id uuid PRIMARY KEY, wallet_id uuid NOT NULL REFERENCES wallets(id), player_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
 amount bigint NOT NULL, currency text NOT NULL CHECK(currency IN ('BRL','USD','EUR')),
 provider_id text, external_id text, idempotency_key text, payload_hash text, round_id text, game_id text,
 reference_external_id text, reference_id uuid REFERENCES wager_transactions(id),
 status text NOT NULL CHECK(status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
 failure_code text, result_balance bigint CHECK(result_balance>=0), result_currency text,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0), next_attempt timestamptz NOT NULL DEFAULT now(),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(provider_id,external_id), UNIQUE(provider_id,idempotency_key),
 CHECK((kind='LOSS' AND amount=0) OR (kind<>'LOSS' AND amount>0)),
 CHECK((status IN ('REJECTED','FAILED'))=(failure_code IS NOT NULL)),
 CHECK(status<>'PROCESSED' OR result_balance IS NOT NULL),
 CHECK((result_balance IS NULL)=(result_currency IS NULL)),
 CHECK(result_currency IS NULL OR result_currency IN ('BRL','USD','EUR')),
 CHECK(status<>'PENDING_REFERENCE' OR reference_external_id IS NOT NULL),
 CHECK(kind NOT IN ('REFUND','ROLLBACK') OR reference_external_id IS NOT NULL),
 CHECK(kind NOT IN ('BET','LOSS') OR reference_external_id IS NULL),
 CHECK(reference_external_id IS NULL OR reference_external_id<>external_id),
 CHECK(updated_at>=created_at),
 CHECK(provider_id IS NULL OR provider_id ~ '^[a-zA-Z0-9_.:-]{1,128}$'),
 CHECK(external_id IS NULL OR external_id ~ '^[a-zA-Z0-9_.:-]{1,128}$'),
 CHECK(idempotency_key IS NULL OR idempotency_key ~ '^[a-zA-Z0-9_.:-]{1,128}$'),
 CHECK(round_id IS NULL OR round_id ~ '^[a-zA-Z0-9_.:-]{1,128}$'),
 CHECK(game_id IS NULL OR game_id ~ '^[a-zA-Z0-9_.:-]{1,128}$'),
 CHECK(reference_external_id IS NULL OR reference_external_id ~ '^[a-zA-Z0-9_.:-]{1,128}$'),
 CHECK(
  (kind='OPENING' AND id=wallet_id AND status='PROCESSED'
   AND provider_id IS NULL AND external_id IS NULL AND idempotency_key IS NULL AND payload_hash IS NULL
   AND round_id IS NULL AND game_id IS NULL AND reference_external_id IS NULL AND reference_id IS NULL)
  OR
  (kind<>'OPENING' AND provider_id IS NOT NULL AND external_id IS NOT NULL AND idempotency_key IS NOT NULL
   AND payload_hash IS NOT NULL AND payload_hash ~ '^[a-f0-9]{64}$' AND round_id IS NOT NULL AND game_id IS NOT NULL)
 )
);
CREATE UNIQUE INDEX one_opening ON wager_transactions(wallet_id) WHERE kind='OPENING';
-- Deliberately stronger than one reversal of EACH kind: either kind consumes the original.
CREATE UNIQUE INDEX one_successful_reversal ON wager_transactions(reference_id)
 WHERE status='PROCESSED' AND kind IN ('REFUND','ROLLBACK');
CREATE INDEX pending_work ON wager_transactions(next_attempt) WHERE status IN ('PENDING','PENDING_REFERENCE');

CREATE TABLE wallet_ledger (
 seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
 id uuid PRIMARY KEY, wallet_id uuid NOT NULL REFERENCES wallets(id),
 transaction_id uuid NOT NULL REFERENCES wager_transactions(id),
 direction text NOT NULL CHECK(direction IN ('DEBIT','CREDIT')),
 amount bigint NOT NULL CHECK(amount>0), currency text NOT NULL CHECK(currency IN ('BRL','USD','EUR')),
 balance_before bigint NOT NULL CHECK(balance_before>=0),
 balance_after bigint NOT NULL CHECK(balance_after>=0), created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(wallet_id,transaction_id),
 CHECK(balance_after::numeric=balance_before::numeric+CASE direction WHEN 'CREDIT' THEN amount::numeric ELSE -amount::numeric END)
);
CREATE INDEX ledger_cursor ON wallet_ledger(wallet_id,seq);

CREATE TABLE inbox (
 consumer_name text NOT NULL, message_id text NOT NULL, payload_hash text NOT NULL CHECK(payload_hash ~ '^[a-f0-9]{64}$'),
 received_at timestamptz NOT NULL DEFAULT now(), completed_at timestamptz, result jsonb,
 PRIMARY KEY(consumer_name,message_id), CHECK((completed_at IS NULL)=(result IS NULL))
);
CREATE TABLE outbox (
 seq bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
 event_id uuid PRIMARY KEY, aggregate_id uuid NOT NULL, event_type text NOT NULL,
 payload jsonb NOT NULL, occurred_at timestamptz NOT NULL, attempts integer NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL DEFAULT now(), lease_until timestamptz, lease_token uuid,
 published_at timestamptz,
 CHECK(jsonb_typeof(payload)='object'),
 CHECK(payload ?& ARRAY['eventId','eventType','aggregateId','correlationId','occurredAt','version','data']),
 CHECK(event_type IN ('WagerTransactionProcessed','WagerTransactionRejected','WalletBalanceChanged','WagerTransactionPendingReference')),
 CHECK(payload->>'eventId'=event_id::text),
 CHECK(payload->>'aggregateId'=aggregate_id::text),
 CHECK(payload->>'eventType'=event_type),
 CHECK(attempts>=0), CHECK((lease_until IS NULL)=(lease_token IS NULL))
);
CREATE INDEX outbox_pending ON outbox(next_attempt,seq) WHERE published_at IS NULL;

CREATE FUNCTION immutable_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'append-only record' USING ERRCODE='23514'; END $$;
CREATE TRIGGER immutable_ledger BEFORE UPDATE OR DELETE ON wallet_ledger FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_ledger_truncate BEFORE TRUNCATE ON wallet_ledger FOR EACH STATEMENT EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_transactions_delete BEFORE DELETE ON wager_transactions FOR EACH ROW EXECUTE FUNCTION immutable_record();
CREATE TRIGGER immutable_wallets_delete BEFORE DELETE ON wallets FOR EACH ROW EXECUTE FUNCTION immutable_record();

CREATE FUNCTION guard_wallet() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.balance<>0 OR NEW.version<>1 THEN RAISE EXCEPTION 'create wallet with zero balance and version one'; END IF;
 ELSE
  -- Only nested application of a ledger entry may mutate a wallet.
  IF pg_trigger_depth()<2 THEN RAISE EXCEPTION 'wallet balance changes require ledger'; END IF;
  IF ROW(NEW.id,NEW.player_id,NEW.currency,NEW.created_at) IS DISTINCT FROM ROW(OLD.id,OLD.player_id,OLD.currency,OLD.created_at)
   THEN RAISE EXCEPTION 'immutable wallet identity'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER guard_wallet BEFORE INSERT OR UPDATE ON wallets FOR EACH ROW EXECUTE FUNCTION guard_wallet();

CREATE FUNCTION guard_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE w wallets; r wager_transactions;
BEGIN
 IF TG_OP='UPDATE' THEN
  IF OLD.status IN ('PROCESSED','REJECTED','FAILED') THEN RAISE EXCEPTION 'terminal transaction immutable'; END IF;
  IF ROW(NEW.id,NEW.wallet_id,NEW.player_id,NEW.kind,NEW.amount,NEW.currency,NEW.provider_id,NEW.external_id,NEW.idempotency_key,NEW.payload_hash,NEW.round_id,NEW.game_id,NEW.reference_external_id,NEW.created_at)
    IS DISTINCT FROM ROW(OLD.id,OLD.wallet_id,OLD.player_id,OLD.kind,OLD.amount,OLD.currency,OLD.provider_id,OLD.external_id,OLD.idempotency_key,OLD.payload_hash,OLD.round_id,OLD.game_id,OLD.reference_external_id,OLD.created_at)
   THEN RAISE EXCEPTION 'immutable transaction input'; END IF;
  IF NEW.status NOT IN ('PENDING_REFERENCE','PROCESSED','REJECTED','FAILED') THEN RAISE EXCEPTION 'invalid transition'; END IF;
 END IF;
 IF NEW.status='PROCESSED' THEN
  SELECT * INTO STRICT w FROM wallets WHERE id=NEW.wallet_id FOR UPDATE;
  IF w.player_id<>NEW.player_id OR w.currency<>NEW.currency OR NEW.result_currency<>w.currency THEN
   RAISE EXCEPTION 'wallet mismatch';
  END IF;
  IF NEW.kind IN ('REFUND','ROLLBACK') OR NEW.reference_external_id IS NOT NULL THEN
   SELECT * INTO STRICT r FROM wager_transactions WHERE id=NEW.reference_id;
   IF r.status<>'PROCESSED' OR r.provider_id<>NEW.provider_id OR r.external_id<>NEW.reference_external_id
    OR r.player_id<>NEW.player_id OR r.wallet_id<>NEW.wallet_id OR r.currency<>NEW.currency OR r.round_id<>NEW.round_id
    THEN RAISE EXCEPTION 'reference mismatch'; END IF;
   IF NEW.kind IN ('REFUND','ROLLBACK') AND NEW.amount<>r.amount THEN RAISE EXCEPTION 'partial reversal'; END IF;
   IF (NEW.kind IN ('REFUND','WIN') AND r.kind<>'BET') OR (NEW.kind='ROLLBACK' AND r.kind NOT IN ('BET','WIN','REFUND'))
    THEN RAISE EXCEPTION 'invalid reference kind'; END IF;
  END IF;
  IF NEW.kind='LOSS' AND NEW.result_balance<>w.balance THEN RAISE EXCEPTION 'loss changed balance'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER guard_transaction BEFORE INSERT OR UPDATE ON wager_transactions FOR EACH ROW EXECUTE FUNCTION guard_transaction();

CREATE FUNCTION apply_ledger() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=public,pg_temp AS $$
DECLARE w wallets; t wager_transactions; r wager_transactions; expected text;
BEGIN
 SELECT * INTO STRICT w FROM wallets WHERE id=NEW.wallet_id FOR UPDATE;
 SELECT * INTO STRICT t FROM wager_transactions WHERE id=NEW.transaction_id;
 IF t.status<>'PROCESSED' OR t.kind='LOSS' OR t.wallet_id<>w.id OR t.player_id<>w.player_id
  OR t.currency<>w.currency OR NEW.currency<>w.currency OR NEW.amount<>t.amount OR NEW.balance_before<>w.balance
  OR t.result_balance<>NEW.balance_after OR t.result_currency<>NEW.currency THEN RAISE EXCEPTION 'ledger/domain mismatch'; END IF;
 expected:=CASE t.kind WHEN 'BET' THEN 'DEBIT' ELSE 'CREDIT' END;
 IF t.kind='ROLLBACK' THEN
  SELECT * INTO STRICT r FROM wager_transactions WHERE id=t.reference_id;
  expected:=CASE r.kind WHEN 'BET' THEN 'CREDIT' ELSE 'DEBIT' END;
 END IF;
 IF NEW.direction<>expected THEN RAISE EXCEPTION 'wrong direction'; END IF;
 IF t.kind='OPENING' AND (w.version<>1 OR w.balance<>0 OR EXISTS(SELECT 1 FROM wallet_ledger WHERE wallet_id=w.id))
  THEN RAISE EXCEPTION 'duplicate opening'; END IF;
 UPDATE wallets SET balance=NEW.balance_after,
  version=CASE WHEN t.kind='OPENING' THEN 1 ELSE version+1 END,
  updated_at=GREATEST(updated_at,NEW.created_at) WHERE id=w.id;
 RETURN NEW;
END $$;
CREATE TRIGGER apply_ledger BEFORE INSERT ON wallet_ledger FOR EACH ROW EXECUTE FUNCTION apply_ledger();

CREATE FUNCTION require_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE t wager_transactions; n integer;
BEGIN
 SELECT * INTO t FROM wager_transactions WHERE id=NEW.id;
 SELECT count(*) INTO n FROM wallet_ledger WHERE wallet_id=t.wallet_id AND transaction_id=t.id;
 IF (t.status='PROCESSED' AND t.kind<>'LOSS' AND n<>1) OR ((t.status<>'PROCESSED' OR t.kind='LOSS') AND n<>0)
  THEN RAISE EXCEPTION 'transaction must have exactly its corresponding ledger'; END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER require_ledger AFTER INSERT OR UPDATE ON wager_transactions DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION require_ledger();

CREATE FUNCTION guard_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(OLD.event_id,OLD.aggregate_id,OLD.event_type,OLD.payload,OLD.occurred_at,OLD.seq)
   IS DISTINCT FROM ROW(NEW.event_id,NEW.aggregate_id,NEW.event_type,NEW.payload,NEW.occurred_at,NEW.seq)
  THEN RAISE EXCEPTION 'immutable outbox snapshot'; END IF;
 IF OLD.published_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'published outbox immutable'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER guard_outbox BEFORE UPDATE ON outbox FOR EACH ROW EXECUTE FUNCTION guard_outbox();
CREATE TRIGGER immutable_outbox_delete BEFORE DELETE ON outbox FOR EACH ROW EXECUTE FUNCTION immutable_record();

CREATE TABLE schema_migrations(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_migrations(version) VALUES(1);
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO wager_app;
GRANT SELECT,INSERT ON wallets,wager_transactions,wallet_ledger,inbox,outbox TO wager_app;
-- SELECT FOR UPDATE requires UPDATE privilege on at least one column.
-- guard_wallet still rejects direct UPDATE, including updated_at.
GRANT UPDATE(updated_at) ON wallets TO wager_app;
GRANT UPDATE(status,failure_code,result_balance,result_currency,reference_id,attempts,next_attempt,updated_at) ON wager_transactions TO wager_app;
GRANT UPDATE(completed_at,result) ON inbox TO wager_app;
GRANT UPDATE(attempts,next_attempt,lease_until,lease_token,published_at) ON outbox TO wager_app;
GRANT SELECT ON schema_migrations TO wager_app;
GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO wager_app;
REVOKE ALL ON FUNCTION apply_ledger() FROM PUBLIC;
COMMIT;
