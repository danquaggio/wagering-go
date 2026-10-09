//go:build integration || dbtest

package integration

import (
	"context"
	"encoding/json"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"example.com/wagering/internal/postgres"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// databaseConfig applies the isolated database name to the parsed configuration.
// pgx ConnString() returns the original DSN, even after ConnConfig.Database changes.
// Keep the override in the environment so independent test processes inherit it.
func databaseConfig(t *testing.T) *pgxpool.Config {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is required (real migrated PostgreSQL)")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if name := os.Getenv("TEST_DATABASE_NAME"); name != "" {
		cfg.ConnConfig.Database = name
	}
	return cfg
}

func TestDatabaseConfigIsolation(t *testing.T) {
	for name, dsn := range map[string]string{
		"url":      "postgres://wager_app:test_password@localhost:5432/wagering?sslmode=disable",
		"keywords": "host=localhost port=5432 user=wager_app password=test_password dbname=wagering sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_DATABASE_URL", dsn)
			t.Setenv("TEST_DATABASE_NAME", "")
			if got := databaseConfig(t).ConnConfig.Database; got != "wagering" {
				t.Fatalf("default database: got %q, want wagering", got)
			}
			t.Setenv("TEST_DATABASE_NAME", "wager_isolated_regression")
			cfg := databaseConfig(t)
			if cfg.ConnConfig.Database != "wager_isolated_regression" {
				t.Fatalf("isolated database: got %q", cfg.ConnConfig.Database)
			}
			if cfg.ConnConfig.Host != "localhost" || cfg.ConnConfig.User != "wager_app" || cfg.ConnConfig.Password != "test_password" {
				t.Fatal("database isolation changed other connection settings")
			}
		})
	}
}

func database(t *testing.T) (*postgres.Store, *application.Service) {
	t.Helper()
	cfg := databaseConfig(t)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var connectedDatabase string
	if err = pool.QueryRow(context.Background(), "SELECT current_database()").Scan(&connectedDatabase); err != nil {
		t.Fatal(err)
	}
	if connectedDatabase != cfg.ConnConfig.Database {
		t.Fatalf("connected to database %q, expected %q", connectedDatabase, cfg.ConnConfig.Database)
	}
	store := postgres.New(pool)
	svc, err := application.New(store, 12, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return store, svc
}
func amount(t *testing.T, s string) domain.Money {
	t.Helper()
	m, err := domain.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func openWallet(t *testing.T, svc *application.Service, s string) domain.WalletView {
	t.Helper()
	w, err := svc.Open(context.Background(), domain.ID(), amount(t, s))
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func input(t *testing.T, w domain.WalletView, k domain.Kind, a string) application.Input {
	t.Helper()
	external := domain.ID()
	return application.Input{Request: domain.Request{ProviderID: "provider-a", ExternalID: external, PlayerID: w.PlayerID, WalletID: w.ID, RoundID: "round", GameID: "game", Kind: k, Money: amount(t, a)}, Key: external, CorrelationID: domain.ID()}
}
func execute(t *testing.T, svc *application.Service, in application.Input) application.Result {
	t.Helper()
	r, err := svc.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func assertWallet(t *testing.T, s *postgres.Store, id string, balance string, entries int64) {
	t.Helper()
	r, err := s.Reconcile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Consistent || r.Stored.String() != balance || r.Count != entries {
		t.Fatalf("reconciliation: %+v", r)
	}
}
func TestFinancialRules(t *testing.T) {
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	bet := input(t, w, domain.Bet, "25.00")
	r := execute(t, svc, bet)
	if r.Status != domain.Processed || r.Balance.String() != "75.00" {
		t.Fatal(r)
	}
	loss := input(t, w, domain.Loss, "0.00")
	execute(t, svc, loss)
	saved, _ := s.Wallet(context.Background(), w.ID)
	if saved.View().Version != 2 {
		t.Fatal("LOSS changed version")
	}
	win := input(t, w, domain.Win, "10.00")
	execute(t, svc, win)
	replay := execute(t, svc, bet)
	if !replay.Replay || replay.Balance.String() != "75.00" {
		t.Fatal("replay must reproduce original balance", replay)
	}
	conflict := bet
	conflict.Request.Money = amount(t, "24.00")
	if _, err := svc.Execute(context.Background(), conflict); err != application.ErrConflict {
		t.Fatal(err)
	}
	conflict = bet
	conflict.Key = domain.ID()
	if _, err := svc.Execute(context.Background(), conflict); err != application.ErrConflict {
		t.Fatal("alternate key accepted", err)
	}
	refund := input(t, w, domain.Refund, "25.00")
	refund.Request.Reference = bet.Request.ExternalID
	if r = execute(t, svc, refund); r.Status != domain.Processed {
		t.Fatal(r)
	}
	rollback := input(t, w, domain.Rollback, "25.00")
	rollback.Request.Reference = bet.Request.ExternalID
	if r = execute(t, svc, rollback); r.FailureCode != "ALREADY_REVERSED" {
		t.Fatal(r)
	}
	rollback = input(t, w, domain.Rollback, "25.00")
	rollback.Request.Reference = refund.Request.ExternalID
	if r = execute(t, svc, rollback); r.Status != domain.Processed {
		t.Fatal(r)
	}
	assertWallet(t, s, w.ID, "85.00", 5)
	w2 := openWallet(t, svc, "0.00")
	assertWallet(t, s, w2.ID, "0.00", 0)
	win = input(t, w2, domain.Win, "10.00")
	execute(t, svc, win)
	execute(t, svc, input(t, w2, domain.Bet, "10.00"))
	rollback = input(t, w2, domain.Rollback, "10.00")
	rollback.Request.Reference = win.Request.ExternalID
	if r = execute(t, svc, rollback); r.FailureCode != "REVERSAL_INSUFFICIENT_FUNDS" {
		t.Fatal(r)
	}
	assertWallet(t, s, w2.ID, "0.00", 2)
}
func TestInboxAtomicityAndConflict(t *testing.T) {
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	in := input(t, w, domain.Bet, "25.00")
	in.MessageID = domain.ID()
	in.MessageHash = fmt.Sprintf("%064d", 1)
	a := execute(t, svc, in)
	b := execute(t, svc, in)
	if a.TransactionID != b.TransactionID || !b.Replay {
		t.Fatal(a, b)
	}
	in.MessageHash = fmt.Sprintf("%064d", 2)
	if _, err := svc.Execute(context.Background(), in); err != application.ErrConflict {
		t.Fatal(err)
	}
	// A transaction error must roll back the newly inserted inbox too.
	in.MessageID = domain.ID()
	in.Request.WalletID = domain.ID()
	if _, err := svc.Execute(context.Background(), in); err == nil {
		t.Fatal("unknown wallet")
	}
	var n int
	if err := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM inbox WHERE message_id=$1", in.MessageID).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	assertWallet(t, s, w.ID, "75.00", 2)
}
func TestPendingReferenceDurability(t *testing.T) {
	isolateDatabase(t)
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	bet := input(t, w, domain.Bet, "25.00")
	refund := input(t, w, domain.Refund, "25.00")
	refund.Request.Reference = bet.Request.ExternalID
	pending := execute(t, svc, refund)
	if pending.Status != domain.PendingReference {
		t.Fatal(pending)
	}
	execute(t, svc, bet)
	// New repository and service simulate a restart; multiprocess tests below have independent memory too.
	_, resumer := database(t)
	time.Sleep(1100 * time.Millisecond)
	if err := resumer.Resume(context.Background(), pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	replay := execute(t, svc, refund)
	if replay.Status != domain.Processed {
		t.Fatal(replay)
	}
	assertWallet(t, s, w.ID, "100.00", 3)
	expiring, err := application.New(s, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	missing := input(t, w, domain.Refund, "1.00")
	missing.Request.Reference = domain.ID()
	pending = execute(t, expiring, missing)
	time.Sleep(1100 * time.Millisecond)
	if err = expiring.Resume(context.Background(), pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	r := execute(t, svc, missing)
	if r.FailureCode != "REFERENCE_NOT_FOUND" {
		t.Fatal(r)
	}
}
func TestDatabaseProtection(t *testing.T) {
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	ctx := context.Background()
	for _, q := range []string{
		"UPDATE wallets SET balance=0 WHERE id=$1",
		"UPDATE wallet_ledger SET amount=1 WHERE wallet_id=$1",
		"DELETE FROM wallet_ledger WHERE wallet_id=$1",
		"UPDATE wager_transactions SET status='PENDING' WHERE wallet_id=$1",
	} {
		if _, err := s.Pool.Exec(ctx, q, w.ID); err == nil {
			t.Fatal("unauthorized mutation", q)
		}
	}
	// Force an error after all financial writes, before commit; nothing may survive.
	in := input(t, w, domain.Bet, "1.00")
	in.Key = "rollback-" + domain.ID()
	err := s.Within(ctx, func(u application.Unit) error {
		got, err := u.Wallet(ctx, w.ID)
		if err != nil {
			return err
		}
		before := got.View().Balance
		if err = got.Debit(in.Request.Money, time.Now()); err != nil {
			return err
		}
		tx, err := domain.NewTransaction(domain.ID(), in.Key, in.Request, time.Now())
		if err != nil {
			return err
		}
		if _, err = u.InsertTransaction(ctx, tx); err != nil {
			return err
		}
		after := got.View().Balance
		if err = tx.Transition(domain.Processed, "", "", &after, time.Now()); err != nil {
			return err
		}
		if err = u.SaveTransaction(ctx, tx, 0, time.Now()); err != nil {
			return err
		}
		l, err := domain.NewLedger(domain.LedgerView{ID: domain.ID(), WalletID: w.ID, TransactionID: tx.View().ID, Direction: domain.Debit, Money: in.Request.Money, Before: before, After: after, CreatedAt: time.Now()})
		if err != nil {
			return err
		}
		if err = u.Ledger(ctx, l); err != nil {
			return err
		}
		return fmt.Errorf("injected before commit")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	assertWallet(t, s, w.ID, "100.00", 1)
	// PROCESSED without a corresponding ledger must fail at COMMIT.
	err = s.Within(ctx, func(u application.Unit) error {
		tx, err := domain.NewTransaction(domain.ID(), in.Key, in.Request, time.Now())
		if err != nil {
			return err
		}
		if _, err = u.InsertTransaction(ctx, tx); err != nil {
			return err
		}
		b := amount(t, "99.00")
		if err = tx.Transition(domain.Processed, "", "", &b, time.Now()); err != nil {
			return err
		}
		return u.SaveTransaction(ctx, tx, 0, time.Now())
	})
	if err == nil {
		t.Fatal("processed without ledger committed")
	}
	assertWallet(t, s, w.ID, "100.00", 1)
}
func TestIndependentWalletProgress(t *testing.T) {
	s, svc := database(t)
	a := openWallet(t, svc, "100.00")
	b := openWallet(t, svc, "100.00")
	ctx := context.Background()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", a.ID); err != nil {
		t.Fatal(err)
	}
	fast, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := svc.Execute(fast, input(t, b, domain.Bet, "1.00"))
	if err != nil || r.Status != domain.Processed {
		t.Fatal("unrelated wallet blocked", err)
	}
}
func TestIndependentProcessHelper(t *testing.T) {
	if os.Getenv("WAGER_PROCESS_HELPER") != "1" {
		return
	}
	store, svc := database(t)
	var in application.Input
	if err := json.Unmarshal([]byte(os.Getenv("WAGER_PROCESS_INPUT")), &in); err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(os.Getenv("WAGER_PROCESS_COUNT"))
	if err != nil {
		t.Fatal(err)
	}
	at, _ := strconv.ParseInt(os.Getenv("WAGER_PROCESS_START"), 10, 64)
	time.Sleep(time.Until(time.UnixMilli(at)))
	if os.Getenv("WAGER_PROCESS_MODE") == "outbox" {
		if _, err := store.Claim(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}
	for i := 0; i < n; i++ {
		if _, err := svc.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
}
func processes(t *testing.T, inputs []application.Input, counts []int) {
	t.Helper()
	at := time.Now().Add(time.Second).UnixMilli()
	var wg sync.WaitGroup
	errs := make(chan error, len(inputs))
	for i, in := range inputs {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestIndependentProcessHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "WAGER_PROCESS_HELPER=1", "WAGER_PROCESS_INPUT="+string(raw), "WAGER_PROCESS_COUNT="+strconv.Itoa(counts[i]), "WAGER_PROCESS_START="+strconv.FormatInt(at, 10))
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("%w: %s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
func TestThreeProcesses50Duplicates(t *testing.T) {
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	in := input(t, w, domain.Bet, "25.00")
	processes(t, []application.Input{in, in, in}, []int{17, 17, 16})
	assertWallet(t, s, w.ID, "75.00", 2)
	var n int
	if err := s.Pool.QueryRow(context.Background(), "SELECT count(*) FROM wager_transactions WHERE wallet_id=$1 AND kind='BET'", w.ID).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
func TestThreeProcessesTwoBets80(t *testing.T) {
	s, svc := database(t)
	w := openWallet(t, svc, "100.00")
	a := input(t, w, domain.Bet, "80.00")
	b := input(t, w, domain.Bet, "80.00")
	// The third process races a replay of A while the first two race distinct operations.
	processes(t, []application.Input{a, b, a}, []int{1, 1, 1})
	aa := execute(t, svc, a)
	bb := execute(t, svc, b)
	statuses := map[domain.State]int{aa.Status: 1}
	statuses[bb.Status]++
	if statuses[domain.Processed] != 1 || statuses[domain.Rejected] != 1 {
		t.Fatal(aa, bb)
	}
	rejected := aa
	if bb.Status == domain.Rejected {
		rejected = bb
	}
	if rejected.FailureCode != "INSUFFICIENT_FUNDS" {
		t.Fatal(rejected)
	}
	assertWallet(t, s, w.ID, "20.00", 2)
}

func TestThreeProcessesIndependentWallets(t *testing.T) {
	store, svc := database(t)
	wallets := []domain.WalletView{openWallet(t, svc, "100.00"), openWallet(t, svc, "100.00"), openWallet(t, svc, "100.00")}
	inputs := make([]application.Input, 3)
	for i, w := range wallets {
		inputs[i] = input(t, w, domain.Bet, "25.00")
	}
	processes(t, inputs, []int{1, 1, 1})
	for _, w := range wallets {
		assertWallet(t, store, w.ID, "75.00", 2)
	}
}
func TestMigrationsAndConcurrentOutbox(t *testing.T) {
	adminDSN := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminDSN == "" {
		t.Fatal("TEST_ADMIN_DATABASE_URL required for isolated migration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "wager_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)") }()
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "001_init.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, string(up)); err != nil {
		t.Fatal("up migration", err)
	}
	t.Setenv("TEST_DATABASE_NAME", name)
	appCfg := databaseConfig(t)
	appPool, err := pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer appPool.Close()
	store := postgres.New(appPool)
	svc, err := application.New(store, 12, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := openWallet(t, svc, "100.00")
	if _, err = owner.Exec(ctx, "UPDATE outbox SET next_attempt='infinity' WHERE event_type='WalletBalanceChanged'"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAGER_PROCESS_MODE", "outbox")
	in := input(t, w, domain.Loss, "0.00")
	processes(t, []application.Input{in, in, in}, []int{1, 1, 1})
	var id string
	var attempts int
	if err = owner.QueryRow(ctx, "SELECT event_id::text,attempts FROM outbox WHERE event_type='WagerTransactionProcessed'").Scan(&id, &attempts); err != nil || attempts != 1 {
		t.Fatalf("expected exactly one outbox claim, got attempts=%d: %v", attempts, err)
	}
	// Advance only the lease clock, modeling abandonment without waiting 30 seconds.
	if _, err = owner.Exec(ctx, "UPDATE outbox SET lease_until=now()-interval '1 second' WHERE event_id=$1", id); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.Claim(ctx)
	if err != nil || reclaimed == nil || reclaimed.ID != id || reclaimed.Attempts != 2 {
		t.Fatal("abandoned lease not reclaimed", reclaimed, err)
	}
	if err = store.Published(ctx, reclaimed); err != nil {
		t.Fatal(err)
	}
	// Verify trigger protection as the schema owner too, without disabling triggers.
	if _, err = owner.Exec(ctx, "UPDATE wallet_ledger SET amount=1"); err == nil {
		t.Fatal("ledger edit passed owner trigger")
	}
	if _, err = owner.Exec(ctx, "DELETE FROM wallet_ledger"); err == nil {
		t.Fatal("ledger delete passed owner trigger")
	}
	appPool.Close()
	down, err := os.ReadFile(filepath.Join("..", "..", "migrations", "001_init.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, string(down)); err != nil {
		t.Fatal("down migration", err)
	}
	if _, err = owner.Exec(ctx, string(up)); err != nil {
		t.Fatal("reapply migration", err)
	}
}

// isolateDatabase gives timing-sensitive worker tests their own migrated database.
func isolateDatabase(t *testing.T) {
	t.Helper()
	adminDSN := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminDSN == "" {
		t.Fatal("TEST_ADMIN_DATABASE_URL required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := "wager_isolated_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); admin.Close() })
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	owner, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "001_init.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, string(up)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_DATABASE_NAME", name)
}
