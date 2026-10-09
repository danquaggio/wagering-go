package domain

import (
	"encoding/json"
	"time"
)

type ProcessedData struct {
	TransactionID string `json:"transactionId"`
	WalletID      string `json:"walletId"`
	PlayerID      string `json:"playerId"`
	ProviderID    string `json:"providerId,omitempty"`
	ExternalID    string `json:"externalTransactionId,omitempty"`
	Kind          Kind   `json:"kind"`
	Money         Money  `json:"money"`
	Balance       Money  `json:"balance"`
}
type RejectedData struct {
	TransactionID string `json:"transactionId"`
	WalletID      string `json:"walletId"`
	ProviderID    string `json:"providerId"`
	FailureCode   string `json:"failureCode"`
}
type PendingData struct {
	TransactionID string `json:"transactionId"`
	WalletID      string `json:"walletId"`
	ProviderID    string `json:"providerId"`
	Reference     string `json:"referenceExternalTransactionId"`
}
type BalanceData struct {
	WalletID      string    `json:"walletId"`
	TransactionID string    `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	Before        Money     `json:"balanceBefore"`
	After         Money     `json:"balanceAfter"`
	Version       int64     `json:"walletVersion"`
}
type Envelope[T any] struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          T         `json:"data"`
}

// Event stores private bytes; Bytes returns a copy, so its snapshot cannot change.
type Event struct {
	id, kind, aggregate string
	at                  time.Time
	payload             []byte
}

func (e Event) ID() string        { return e.id }
func (e Event) Type() string      { return e.kind }
func (e Event) Aggregate() string { return e.aggregate }
func (e Event) At() time.Time     { return e.at }
func (e Event) Bytes() []byte     { return append([]byte(nil), e.payload...) }
func event[T any](kind, aggregate, correlation, causation string, data T, now time.Time) (Event, error) {
	id := ID()
	now = now.UTC()
	b, err := json.Marshal(Envelope[T]{id, kind, aggregate, correlation, causation, now, 1, data})
	return Event{id, kind, aggregate, now, b}, err
}
func ProcessedEvent(v TransactionView, correlation, causation string) (Event, error) {
	if v.State != Processed || v.Balance == nil {
		return Event{}, Rule("INVALID_EVENT_STATE")
	}
	return event("WagerTransactionProcessed", v.Request.WalletID, correlation, causation, ProcessedData{v.ID, v.Request.WalletID, v.Request.PlayerID, v.Request.ProviderID, v.Request.ExternalID, v.Request.Kind, v.Request.Money, *v.Balance}, v.UpdatedAt)
}
func RejectedEvent(v TransactionView, correlation, causation string) (Event, error) {
	if v.State != Rejected || v.Failure == "" {
		return Event{}, Rule("INVALID_EVENT_STATE")
	}
	return event("WagerTransactionRejected", v.Request.WalletID, correlation, causation, RejectedData{v.ID, v.Request.WalletID, v.Request.ProviderID, v.Failure}, v.UpdatedAt)
}
func PendingEvent(v TransactionView, correlation, causation string) (Event, error) {
	if v.State != PendingReference {
		return Event{}, Rule("INVALID_EVENT_STATE")
	}
	return event("WagerTransactionPendingReference", v.Request.WalletID, correlation, causation, PendingData{v.ID, v.Request.WalletID, v.Request.ProviderID, v.Request.Reference}, v.UpdatedAt)
}
func BalanceEvent(l WalletLedgerEntry, version int64, correlation, causation string) (Event, error) {
	v := l.View()
	if version < 1 {
		return Event{}, Rule("INVALID_VERSION")
	}
	return event("WalletBalanceChanged", v.WalletID, correlation, causation, BalanceData{v.WalletID, v.TransactionID, v.Direction, v.Money, v.Before, v.After, version}, v.CreatedAt)
}
