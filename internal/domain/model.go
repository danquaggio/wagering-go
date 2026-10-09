package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"time"
)

func ID() string {
	b := make([]byte, 16)
	// crypto/rand.Read in Go 1.26 guarantees a full read or fatal runtime failure.
	_, _ = rand.Read(b)
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var token = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,128}$`)

func ValidID(s string) bool    { return uuid.MatchString(s) }
func ValidToken(s string) bool { return token.MatchString(s) }

type WalletView struct {
	ID        string    `json:"id"`
	PlayerID  string    `json:"playerId"`
	Balance   Money     `json:"balance"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}
type Wallet struct{ v WalletView }

func NewWallet(id, player string, balance Money, now time.Time) (Wallet, error) {
	return RestoreWallet(WalletView{id, player, balance, 1, now.UTC(), now.UTC()})
}
func RestoreWallet(v WalletView) (Wallet, error) {
	if !ValidID(v.ID) || !ValidID(v.PlayerID) || !v.Balance.Valid() || v.Balance.Minor() < 0 || v.Version < 1 || v.CreatedAt.IsZero() || v.UpdatedAt.Before(v.CreatedAt) {
		return Wallet{}, Rule("INVALID_WALLET")
	}
	return Wallet{v}, nil
}
func (w Wallet) View() WalletView                     { return w.v }
func (w *Wallet) Credit(m Money, now time.Time) error { return w.move(m, false, now) }
func (w *Wallet) Debit(m Money, now time.Time) error  { return w.move(m, true, now) }
func (w *Wallet) move(m Money, debit bool, now time.Time) error {
	if _, err := RestoreWallet(w.v); err != nil {
		return err
	}
	if !m.Valid() || m.Minor() <= 0 {
		return Rule("INVALID_AMOUNT")
	}
	if now.Before(w.v.UpdatedAt) {
		now = w.v.UpdatedAt
	}
	if w.v.Version == math.MaxInt64 {
		return Rule("VERSION_OVERFLOW")
	}
	var next Money
	var err error
	if debit {
		next, err = w.v.Balance.Sub(m)
	} else {
		next, err = w.v.Balance.Add(m)
	}
	if err != nil {
		return err
	}
	if next.Minor() < 0 {
		return Rule("INSUFFICIENT_FUNDS")
	}
	w.v.Balance = next
	w.v.Version++
	w.v.UpdatedAt = now.UTC()
	return nil
}

type Kind string

const (
	Opening  Kind = "OPENING"
	Bet      Kind = "BET"
	Win      Kind = "WIN"
	Loss     Kind = "LOSS"
	Refund   Kind = "REFUND"
	Rollback Kind = "ROLLBACK"
)

type State string

const (
	Pending          State = "PENDING"
	PendingReference State = "PENDING_REFERENCE"
	Processed        State = "PROCESSED"
	Rejected         State = "REJECTED"
	Failed           State = "FAILED"
)

type Request struct {
	ProviderID string `json:"providerId"`
	ExternalID string `json:"externalTransactionId"`
	PlayerID   string `json:"playerId"`
	WalletID   string `json:"walletId"`
	RoundID    string `json:"roundId"`
	GameID     string `json:"gameId"`
	Kind       Kind   `json:"kind"`
	Money      Money  `json:"money"`
	Reference  string `json:"referenceExternalTransactionId,omitempty"`
}

func (r Request) Validate() error {
	if !ValidToken(r.ProviderID) || !ValidToken(r.ExternalID) || !ValidToken(r.RoundID) || !ValidToken(r.GameID) || !ValidID(r.PlayerID) || !ValidID(r.WalletID) {
		return Rule("INVALID_IDENTIFIERS")
	}
	if !r.Money.Valid() || r.Money.Minor() < 0 {
		return Rule("INVALID_AMOUNT")
	}
	switch r.Kind {
	case Loss:
		if r.Money.Minor() != 0 || r.Reference != "" {
			return Rule("INVALID_LOSS")
		}
	case Bet:
		if r.Reference != "" {
			return Rule("INVALID_REFERENCE")
		}
	case Win:
	case Refund, Rollback:
		if r.Reference == "" {
			return Rule("REFERENCE_REQUIRED")
		}
	default:
		return Rule("INVALID_KIND")
	}
	if r.Kind != Loss && r.Money.Minor() == 0 {
		return Rule("INVALID_AMOUNT")
	}
	if r.Reference != "" && (!ValidToken(r.Reference) || r.Reference == r.ExternalID) {
		return Rule("INVALID_REFERENCE")
	}
	return nil
}

// encoding/json sorts map keys. Only strings/maps are used: no floating point.
func (r Request) Hash() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	v := map[string]any{"providerId": r.ProviderID, "externalTransactionId": r.ExternalID, "playerId": r.PlayerID, "walletId": r.WalletID, "roundId": r.RoundID, "gameId": r.GameID, "kind": string(r.Kind), "money": map[string]string{"amount": r.Money.String(), "currency": r.Money.Currency()}, "referenceExternalTransactionId": r.Reference}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

type TransactionView struct {
	ID          string    `json:"transactionId"`
	Request     Request   `json:"request"`
	State       State     `json:"status"`
	Key         string    `json:"-"`
	Hash        string    `json:"-"`
	ReferenceID string    `json:"referenceTransactionId,omitempty"`
	Failure     string    `json:"failureCode,omitempty"`
	Balance     *Money    `json:"balance,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
type WagerTransaction struct{ v TransactionView }

func NewTransaction(id, key string, r Request, now time.Time) (WagerTransaction, error) {
	h, err := r.Hash()
	if err != nil {
		return WagerTransaction{}, err
	}
	if !ValidID(id) || !ValidToken(key) || now.IsZero() {
		return WagerTransaction{}, Rule("INVALID_TRANSACTION")
	}
	return WagerTransaction{TransactionView{ID: id, Request: r, Key: key, Hash: h, State: Pending, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}}, nil
}
func NewOpening(wallet Wallet, now time.Time) (WagerTransaction, error) {
	v := wallet.View()
	if _, err := RestoreWallet(v); err != nil || v.Balance.Minor() <= 0 || v.Version != 1 || now.IsZero() || now.Before(v.CreatedAt) {
		return WagerTransaction{}, Rule("INVALID_OPENING")
	}
	// Stable internal identity: opening transaction ID equals wallet ID.
	return WagerTransaction{TransactionView{ID: v.ID, Request: Request{WalletID: v.ID, PlayerID: v.PlayerID, Kind: Opening, Money: v.Balance}, State: Processed, Balance: &v.Balance, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}}, nil
}
func RestoreTransaction(v TransactionView) (WagerTransaction, error) {
	if !ValidID(v.ID) || v.CreatedAt.IsZero() || v.UpdatedAt.Before(v.CreatedAt) {
		return WagerTransaction{}, Rule("INVALID_TRANSACTION")
	}
	if v.Request.Kind == Opening {
		if v.ID != v.Request.WalletID || !ValidID(v.Request.PlayerID) || !v.Request.Money.Valid() || v.Request.Money.Minor() <= 0 || v.State != Processed || v.Key != "" || v.Hash != "" || v.Request.ProviderID != "" || v.Request.ExternalID != "" || v.Request.RoundID != "" || v.Request.GameID != "" || v.Request.Reference != "" || v.ReferenceID != "" {
			return WagerTransaction{}, Rule("INVALID_OPENING")
		}
	} else {
		h, err := v.Request.Hash()
		if err != nil {
			return WagerTransaction{}, err
		}
		if !ValidToken(v.Key) || h != v.Hash {
			return WagerTransaction{}, Rule("INVALID_TRANSACTION")
		}
	}
	switch v.State {
	case Pending, PendingReference, Processed, Rejected, Failed:
	default:
		return WagerTransaction{}, Rule("INVALID_STATE")
	}
	if (v.State == Rejected || v.State == Failed) != (v.Failure != "") {
		return WagerTransaction{}, Rule("INVALID_FAILURE_CODE")
	}
	if v.State == PendingReference && v.Request.Reference == "" {
		return WagerTransaction{}, Rule("REFERENCE_REQUIRED")
	}
	if v.State == Processed && (v.Balance == nil || v.Balance.Currency() != v.Request.Money.Currency()) {
		return WagerTransaction{}, Rule("INVALID_RESULT")
	}
	if v.Balance != nil {
		b := *v.Balance
		if !b.Valid() || b.Minor() < 0 {
			return WagerTransaction{}, Rule("INVALID_RESULT")
		}
		v.Balance = &b
	}
	return WagerTransaction{v}, nil
}
func (t WagerTransaction) View() TransactionView {
	v := t.v
	if v.Balance != nil {
		b := *v.Balance
		v.Balance = &b
	}
	return v
}
func (t WagerTransaction) Terminal() bool {
	return t.v.State == Processed || t.v.State == Rejected || t.v.State == Failed
}
func (t *WagerTransaction) Transition(s State, code, referenceID string, balance *Money, now time.Time) error {
	if _, err := RestoreTransaction(t.v); err != nil {
		return err
	}
	if t.Terminal() {
		return Rule("TERMINAL_TRANSACTION")
	}
	if s != PendingReference && s != Processed && s != Rejected && s != Failed {
		return Rule("INVALID_TRANSITION")
	}
	v := t.v
	v.State = s
	v.Failure = code
	v.ReferenceID = referenceID
	v.Balance = balance
	if now.Before(v.UpdatedAt) {
		now = v.UpdatedAt
	}
	v.UpdatedAt = now.UTC()
	n, err := RestoreTransaction(v)
	if err != nil {
		return err
	}
	*t = n
	return nil
}

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

type LedgerView struct {
	ID            string    `json:"id"`
	WalletID      string    `json:"walletId"`
	TransactionID string    `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	Before        Money     `json:"balanceBefore"`
	After         Money     `json:"balanceAfter"`
	CreatedAt     time.Time `json:"createdAt"`
}
type WalletLedgerEntry struct{ v LedgerView }

func NewLedger(v LedgerView) (WalletLedgerEntry, error) {
	if !ValidID(v.ID) || !ValidID(v.WalletID) || !ValidID(v.TransactionID) || v.CreatedAt.IsZero() || !v.Money.Valid() || v.Money.Minor() <= 0 || !v.Before.Valid() || !v.After.Valid() || v.Before.Minor() < 0 || v.After.Minor() < 0 {
		return WalletLedgerEntry{}, Rule("INVALID_LEDGER")
	}
	var expected Money
	var err error
	switch v.Direction {
	case Debit:
		expected, err = v.Before.Sub(v.Money)
	case Credit:
		expected, err = v.Before.Add(v.Money)
	default:
		return WalletLedgerEntry{}, Rule("INVALID_DIRECTION")
	}
	if err != nil {
		return WalletLedgerEntry{}, err
	}
	cmp, err := expected.Compare(v.After)
	if err != nil || cmp != 0 {
		return WalletLedgerEntry{}, Rule("INVALID_LEDGER_BALANCE")
	}
	return WalletLedgerEntry{v}, nil
}
func (l WalletLedgerEntry) View() LedgerView { return l.v }

// EvaluateReference returns the movement direction once a reference is available.
func EvaluateReference(r Request, ref TransactionView, alreadyReversed bool) (Direction, error) {
	if ref.Request.ProviderID != r.ProviderID || ref.Request.PlayerID != r.PlayerID || ref.Request.WalletID != r.WalletID || ref.Request.Money.Currency() != r.Money.Currency() || ref.Request.RoundID != r.RoundID {
		return "", Rule("REFERENCE_MISMATCH")
	}
	if ref.State == Pending || ref.State == PendingReference {
		return "", Rule("REFERENCE_PENDING")
	}
	if ref.State != Processed {
		return "", Rule("REFERENCE_UNSUCCESSFUL")
	}
	if r.Kind == Win {
		if ref.Request.Kind != Bet {
			return "", Rule("INVALID_REFERENCE_KIND")
		}
		return Credit, nil
	}
	if r.Money.Minor() != ref.Request.Money.Minor() {
		return "", Rule("REVERSAL_AMOUNT_MISMATCH")
	}
	if alreadyReversed {
		return "", Rule("ALREADY_REVERSED")
	}
	if r.Kind == Refund {
		if ref.Request.Kind != Bet {
			return "", Rule("INVALID_REFERENCE_KIND")
		}
		return Credit, nil
	}
	if r.Kind == Rollback {
		switch ref.Request.Kind {
		case Bet:
			return Credit, nil
		case Win, Refund:
			return Debit, nil
		default:
			return "", Rule("INVALID_REFERENCE_KIND")
		}
	}
	return "", Rule("INVALID_REFERENCE_KIND")
}
