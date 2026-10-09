package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func request(t *testing.T, kind Kind, amount string) Request {
	return Request{ProviderID: "provider-a", ExternalID: "tx-1", PlayerID: ID(), WalletID: ID(), RoundID: "round-1", GameID: "game-1", Kind: kind, Money: money(t, amount)}
}
func TestWalletAndLedger(t *testing.T) {
	now := time.Now().UTC()
	w, err := NewWallet(ID(), ID(), money(t, "100.00"), now)
	if err != nil {
		t.Fatal(err)
	}
	old := w.View()
	if err = w.Debit(money(t, "80.00"), now); err != nil {
		t.Fatal(err)
	}
	if w.View().Balance.String() != "20.00" || w.View().Version != 2 {
		t.Fatal(w.View())
	}
	if err = w.Debit(money(t, "80.00"), now); err == nil {
		t.Fatal("overdraft")
	}
	if w.View().Balance.String() != "20.00" || w.View().Version != 2 {
		t.Fatal("failed debit mutated state")
	}
	if err = w.Credit(money(t, "0.00"), now); err == nil {
		t.Fatal("zero")
	}
	usd, _ := Parse("1.00", "USD")
	if err = w.Credit(usd, now); err == nil {
		t.Fatal("currency")
	}
	restored, err := RestoreWallet(w.View())
	if err != nil || restored.View() != w.View() {
		t.Fatal("restore caused movement")
	}
	if _, err = RestoreWallet(WalletView{}); err == nil {
		t.Fatal("uninitialized wallet")
	}
	l := LedgerView{ID: ID(), WalletID: w.View().ID, TransactionID: ID(), Direction: Debit, Money: money(t, "80.00"), Before: old.Balance, After: w.View().Balance, CreatedAt: now}
	if _, err = NewLedger(l); err != nil {
		t.Fatal(err)
	}
	l.After = old.Balance
	if _, err = NewLedger(l); err == nil {
		t.Fatal("bad equation")
	}
}
func TestTransactionStatesAndOpening(t *testing.T) {
	now := time.Now().UTC()
	r := request(t, Refund, "25.00")
	r.Reference = "before"
	tx, err := NewTransaction(ID(), "key", r, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Transition(PendingReference, "", "", nil, now); err != nil {
		t.Fatal(err)
	}
	b := money(t, "100.00")
	if err = tx.Transition(Rejected, "REFERENCE_NOT_FOUND", "", &b, now); err != nil {
		t.Fatal(err)
	}
	if err = tx.Transition(Processed, "", ID(), &b, now); err == nil {
		t.Fatal("terminal state changed")
	}
	restore, err := RestoreTransaction(tx.View())
	if err != nil || restore.View().State != Rejected {
		t.Fatal("restore")
	}
	w, err := NewWallet(ID(), ID(), b, now)
	if err != nil {
		t.Fatal(err)
	}
	opening, err := NewOpening(w, now)
	if err != nil {
		t.Fatal(err)
	}
	v := opening.View()
	if v.ID != w.View().ID || v.Request.ProviderID != "" || v.Key != "" || v.Hash != "" || v.Request.Kind != Opening || v.State != Processed {
		t.Fatal("opening metadata")
	}
	event, err := ProcessedEvent(v, v.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(event.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["eventType"]) != `"WagerTransactionProcessed"` {
		t.Fatal(string(event.Bytes()))
	}
	snapshot := event.Bytes()
	snapshot[0] = 'x'
	if event.Bytes()[0] != '{' {
		t.Fatal("mutable snapshot")
	}
	w, _ = NewWallet(ID(), ID(), money(t, "0.00"), now)
	if _, err = NewOpening(w, now); err == nil {
		t.Fatal("zero opening")
	}
	var uninitialized WagerTransaction
	if uninitialized.Transition(Rejected, "X", "", nil, now) == nil {
		t.Fatal("zero transaction")
	}
}
func TestExternalKindsZeroAndHash(t *testing.T) {
	for _, k := range []Kind{Bet, Win, Refund, Rollback, Loss, Opening} {
		r := request(t, k, "0.00")
		if k == Refund || k == Rollback {
			r.Reference = "original"
		}
		if err := r.Validate(); (err == nil) != (k == Loss) {
			t.Fatalf("%s zero: %v", k, err)
		}
		r.Money = money(t, "1.00")
		if err := r.Validate(); (err == nil) != (k != Loss && k != Opening) {
			t.Fatalf("%s positive: %v", k, err)
		}
	}
	r := request(t, Bet, "25.00")
	a, err := r.Hash()
	if err != nil {
		t.Fatal(err)
	}
	tx1, _ := NewTransaction(ID(), "key-a", r, time.Now())
	tx2, _ := NewTransaction(ID(), "key-b", r, time.Now())
	if tx1.View().Hash != a || tx2.View().Hash != a {
		t.Fatal("transport in hash")
	}
	r.Money = money(t, "25.01")
	b, _ := r.Hash()
	if a == b {
		t.Fatal("payload conflict not detectable")
	}
}
func TestReferenceRules(t *testing.T) {
	now := time.Now().UTC()
	r := request(t, Bet, "25.00")
	original, _ := NewTransaction(ID(), "key", r, now)
	b := money(t, "75.00")
	if err := original.Transition(Processed, "", "", &b, now); err != nil {
		t.Fatal(err)
	}
	reversal := r
	reversal.Kind = Refund
	reversal.ExternalID = "reverse"
	reversal.Reference = r.ExternalID
	dir, err := EvaluateReference(reversal, original.View(), false)
	if err != nil || dir != Credit {
		t.Fatal(dir, err)
	}
	if _, err = EvaluateReference(reversal, original.View(), true); err == nil {
		t.Fatal("duplicate reversal")
	}
	reversal.Money = money(t, "24.00")
	if _, err = EvaluateReference(reversal, original.View(), false); err == nil {
		t.Fatal("partial refund")
	}
	reversal.Money = r.Money
	reversal.RoundID = "another"
	if _, err = EvaluateReference(reversal, original.View(), false); err == nil {
		t.Fatal("round mismatch")
	}
	for _, kind := range []Kind{Bet, Win, Refund} {
		ref := original.View()
		ref.Request.Kind = kind
		reversal.RoundID = r.RoundID
		reversal.Kind = Rollback
		dir, err = EvaluateReference(reversal, ref, false)
		if err != nil {
			t.Fatal(err)
		}
		expected := Debit
		if kind == Bet {
			expected = Credit
		}
		if dir != expected {
			t.Fatal(kind, dir)
		}
	}
}
