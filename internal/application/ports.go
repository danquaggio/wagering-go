package application

import (
	"context"
	"errors"
	"example.com/wagering/internal/domain"
	"time"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("idempotency conflict")

type Result struct {
	TransactionID string        `json:"transactionId"`
	Status        domain.State  `json:"status"`
	Balance       *domain.Money `json:"balance,omitempty"`
	FailureCode   string        `json:"failureCode,omitempty"`
	Replay        bool          `json:"idempotentReplay"`
}

func result(v domain.TransactionView, replay bool) Result {
	return Result{v.ID, v.State, v.Balance, v.Failure, replay}
}

type Input struct {
	Request                                    domain.Request
	Key, CorrelationID, MessageID, MessageHash string
}
type Unit interface {
	Attempts(context.Context, string) (int, time.Time, error)
	Wallet(context.Context, string) (domain.Wallet, error)
	CreateWallet(context.Context, domain.Wallet) error
	InsertTransaction(context.Context, domain.WagerTransaction) (bool, error)
	ByKeys(context.Context, string, string, string) ([]domain.WagerTransaction, error)
	Transaction(context.Context, string, bool) (domain.WagerTransaction, error)
	Reference(context.Context, string, string) (domain.WagerTransaction, error)
	WasReversed(context.Context, string) (bool, error)
	SaveTransaction(context.Context, domain.WagerTransaction, int, time.Time) error
	Ledger(context.Context, domain.WalletLedgerEntry) error
	Event(context.Context, domain.Event) error
	Inbox(context.Context, string, string) (*Result, error)
	CompleteInbox(context.Context, string, Result) error
}
type Repository interface {
	Within(context.Context, func(Unit) error) error
	Due(context.Context) ([]string, error)
}
type Service struct {
	repo        Repository
	maxAttempts int
	ttl         time.Duration
}

func New(repo Repository, maxAttempts int, ttl time.Duration) (*Service, error) {
	if maxAttempts < 1 || ttl <= 0 {
		return nil, errors.New("invalid reference retry configuration")
	}
	return &Service{repo, maxAttempts, ttl}, nil
}
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Second * time.Duration(1<<attempt)
}
