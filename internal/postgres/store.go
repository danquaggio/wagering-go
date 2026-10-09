package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

type Store struct{ Pool *pgxpool.Pool }

func New(p *pgxpool.Pool) *Store { return &Store{p} }

type unit struct{ tx pgx.Tx }

func (s *Store) Within(ctx context.Context, fn func(application.Unit) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout='3s'; SET LOCAL statement_timeout='8s'"); err != nil {
		return err
	}
	if err = fn(&unit{tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func convert(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}
	var e *pgconn.PgError
	if errors.As(err, &e) && e.Code == "23505" {
		return application.ErrConflict
	}
	return err
}

type scanner interface{ Scan(...any) error }

func scanWallet(row scanner) (domain.Wallet, error) {
	var v domain.WalletView
	var amount int64
	var currency string
	if err := row.Scan(&v.ID, &v.PlayerID, &amount, &currency, &v.Version, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return domain.Wallet{}, convert(err)
	}
	m, err := domain.Minor(amount, currency)
	if err != nil {
		return domain.Wallet{}, err
	}
	v.Balance = m
	return domain.RestoreWallet(v)
}

const walletColumns = "id::text,player_id::text,balance,currency,version,created_at,updated_at"

func (u *unit) Wallet(ctx context.Context, id string) (domain.Wallet, error) {
	return scanWallet(u.tx.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1 FOR UPDATE", id))
}
func (s *Store) Wallet(ctx context.Context, id string) (domain.Wallet, error) {
	return scanWallet(s.Pool.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1", id))
}
func (u *unit) CreateWallet(ctx context.Context, w domain.Wallet) error {
	v := w.View()
	_, err := u.tx.Exec(ctx, "INSERT INTO wallets(id,player_id,currency,created_at,updated_at) VALUES($1,$2,$3,$4,$4)", v.ID, v.PlayerID, v.Balance.Currency(), v.CreatedAt)
	return convert(err)
}

const txColumns = `id::text,wallet_id::text,player_id::text,kind,amount,currency,COALESCE(provider_id,''),COALESCE(external_id,''),COALESCE(idempotency_key,''),COALESCE(payload_hash,''),COALESCE(round_id,''),COALESCE(game_id,''),COALESCE(reference_external_id,''),COALESCE(reference_id::text,''),status,COALESCE(failure_code,''),result_balance,result_currency,created_at,updated_at`

func scanTransaction(row scanner) (domain.WagerTransaction, error) {
	var v domain.TransactionView
	var amount int64
	var currency string
	var balance *int64
	var balanceCurrency *string
	r := &v.Request
	err := row.Scan(&v.ID, &r.WalletID, &r.PlayerID, &r.Kind, &amount, &currency, &r.ProviderID, &r.ExternalID, &v.Key, &v.Hash, &r.RoundID, &r.GameID, &r.Reference, &v.ReferenceID, &v.State, &v.Failure, &balance, &balanceCurrency, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return domain.WagerTransaction{}, convert(err)
	}
	r.Money, err = domain.Minor(amount, currency)
	if err != nil {
		return domain.WagerTransaction{}, err
	}
	if balance != nil && balanceCurrency != nil {
		m, err := domain.Minor(*balance, *balanceCurrency)
		if err != nil {
			return domain.WagerTransaction{}, err
		}
		v.Balance = &m
	}
	return domain.RestoreTransaction(v)
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func balance(v domain.TransactionView) (any, any) {
	if v.Balance == nil {
		return nil, nil
	}
	return v.Balance.Minor(), v.Balance.Currency()
}
func (u *unit) InsertTransaction(ctx context.Context, t domain.WagerTransaction) (bool, error) {
	v := t.View()
	r := v.Request
	b, c := balance(v)
	tag, err := u.tx.Exec(ctx, `INSERT INTO wager_transactions(id,wallet_id,player_id,kind,amount,currency,provider_id,external_id,idempotency_key,payload_hash,round_id,game_id,reference_external_id,status,result_balance,result_currency,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$17) ON CONFLICT DO NOTHING`,
		v.ID, r.WalletID, r.PlayerID, r.Kind, r.Money.Minor(), r.Money.Currency(), nullable(r.ProviderID), nullable(r.ExternalID), nullable(v.Key), nullable(v.Hash), nullable(r.RoundID), nullable(r.GameID), nullable(r.Reference), v.State, b, c, v.CreatedAt)
	return tag.RowsAffected() == 1, err
}
func (u *unit) ByKeys(ctx context.Context, provider, key, external string) ([]domain.WagerTransaction, error) {
	rows, err := u.tx.Query(ctx, "SELECT "+txColumns+" FROM wager_transactions WHERE provider_id=$1 AND (idempotency_key=$2 OR external_id=$3)", provider, key, external)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WagerTransaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (u *unit) Transaction(ctx context.Context, id string, lock bool) (domain.WagerTransaction, error) {
	q := "SELECT " + txColumns + " FROM wager_transactions WHERE id=$1"
	if lock {
		q += " FOR UPDATE"
	}
	return scanTransaction(u.tx.QueryRow(ctx, q, id))
}
func (s *Store) Transaction(ctx context.Context, id, provider string) (domain.WagerTransaction, error) {
	return scanTransaction(s.Pool.QueryRow(ctx, "SELECT "+txColumns+" FROM wager_transactions WHERE id=$1 AND provider_id=$2", id, provider))
}
func (s *Store) External(ctx context.Context, provider, external string) (domain.WagerTransaction, error) {
	return scanTransaction(s.Pool.QueryRow(ctx, "SELECT "+txColumns+" FROM wager_transactions WHERE provider_id=$1 AND external_id=$2", provider, external))
}
func (u *unit) Reference(ctx context.Context, provider, external string) (domain.WagerTransaction, error) {
	return scanTransaction(u.tx.QueryRow(ctx, "SELECT "+txColumns+" FROM wager_transactions WHERE provider_id=$1 AND external_id=$2", provider, external))
}
func (u *unit) WasReversed(ctx context.Context, id string) (bool, error) {
	var yes bool
	err := u.tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM wager_transactions WHERE reference_id=$1 AND kind IN ('REFUND','ROLLBACK') AND status='PROCESSED')", id).Scan(&yes)
	return yes, err
}
func (u *unit) SaveTransaction(ctx context.Context, t domain.WagerTransaction, attempts int, next time.Time) error {
	v := t.View()
	b, c := balance(v)
	_, err := u.tx.Exec(ctx, `UPDATE wager_transactions SET status=$2,failure_code=$3,result_balance=$4,result_currency=$5,reference_id=$6,attempts=$7,next_attempt=$8,updated_at=$9 WHERE id=$1`, v.ID, v.State, nullable(v.Failure), b, c, nullable(v.ReferenceID), attempts, next, v.UpdatedAt)
	return err
}
func (u *unit) Ledger(ctx context.Context, l domain.WalletLedgerEntry) error {
	v := l.View()
	_, err := u.tx.Exec(ctx, `INSERT INTO wallet_ledger(id,wallet_id,transaction_id,direction,amount,currency,balance_before,balance_after,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, v.ID, v.WalletID, v.TransactionID, v.Direction, v.Money.Minor(), v.Money.Currency(), v.Before.Minor(), v.After.Minor(), v.CreatedAt)
	return err
}
func (u *unit) Event(ctx context.Context, e domain.Event) error {
	_, err := u.tx.Exec(ctx, "INSERT INTO outbox(event_id,aggregate_id,event_type,payload,occurred_at) VALUES($1,$2,$3,$4,$5)", e.ID(), e.Aggregate(), e.Type(), e.Bytes(), e.At())
	return err
}
func (u *unit) Inbox(ctx context.Context, message, hash string) (*application.Result, error) {
	tag, err := u.tx.Exec(ctx, "INSERT INTO inbox(consumer_name,message_id,payload_hash) VALUES('wager-v1',$1,$2) ON CONFLICT DO NOTHING", message, hash)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var old string
	var raw []byte
	if err = u.tx.QueryRow(ctx, "SELECT payload_hash,result FROM inbox WHERE consumer_name='wager-v1' AND message_id=$1", message).Scan(&old, &raw); err != nil {
		return nil, err
	}
	if old != hash {
		return nil, application.ErrConflict
	}
	if len(raw) == 0 {
		return nil, errors.New("incomplete inbox invariant")
	}
	var out application.Result
	// Result balances are always nonnegative and valid external Money.
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func (u *unit) CompleteInbox(ctx context.Context, id string, r application.Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = u.tx.Exec(ctx, "UPDATE inbox SET completed_at=now(),result=$2 WHERE consumer_name='wager-v1' AND message_id=$1", id, b)
	return err
}
func (s *Store) Due(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, "SELECT id::text FROM wager_transactions WHERE status IN ('PENDING','PENDING_REFERENCE') AND next_attempt<=now() ORDER BY next_attempt LIMIT 16")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (u *unit) Attempts(ctx context.Context, id string) (int, time.Time, error) {
	var n int
	var due time.Time
	err := u.tx.QueryRow(ctx, "SELECT attempts,next_attempt FROM wager_transactions WHERE id=$1", id).Scan(&n, &due)
	return n, due, err
}

type Outbox struct {
	ID, Aggregate, Token string
	Payload              []byte
	Attempts             int
}

func (s *Store) Claim(ctx context.Context) (*Outbox, error) {
	var o Outbox
	o.Token = domain.ID()
	err := s.Pool.QueryRow(ctx, `UPDATE outbox SET lease_token=$1,lease_until=now()+interval '30 seconds',attempts=attempts+1
 WHERE event_id=(SELECT event_id FROM outbox WHERE published_at IS NULL AND next_attempt<=now()
 AND (lease_until IS NULL OR lease_until<now()) ORDER BY seq FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING event_id::text,aggregate_id::text,payload,attempts`, o.Token).Scan(&o.ID, &o.Aggregate, &o.Payload, &o.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}
func (s *Store) Published(ctx context.Context, o *Outbox) error {
	_, err := s.Pool.Exec(ctx, "UPDATE outbox SET published_at=now(),lease_token=NULL,lease_until=NULL WHERE event_id=$1 AND lease_token=$2 AND published_at IS NULL", o.ID, o.Token)
	return err
}
func (s *Store) Retry(ctx context.Context, o *Outbox) error {
	_, err := s.Pool.Exec(ctx, "UPDATE outbox SET next_attempt=$3,lease_token=NULL,lease_until=NULL WHERE event_id=$1 AND lease_token=$2 AND published_at IS NULL", o.ID, o.Token, time.Now().Add(application.Backoff(o.Attempts)))
	return err
}
func (s *Store) OutboxAge(ctx context.Context) (int64, error) {
	var seconds int64
	err := s.Pool.QueryRow(ctx, "SELECT COALESCE(EXTRACT(EPOCH FROM now()-min(occurred_at))::bigint,0) FROM outbox WHERE published_at IS NULL").Scan(&seconds)
	return seconds, err
}
