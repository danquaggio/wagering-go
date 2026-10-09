package application

import (
	"context"
	"errors"
	"example.com/wagering/internal/domain"
	"time"
)

func (s *Service) Open(ctx context.Context, player string, initial domain.Money) (domain.WalletView, error) {
	now := time.Now().UTC()
	w, err := domain.NewWallet(domain.ID(), player, initial, now)
	if err != nil {
		return domain.WalletView{}, err
	}
	err = s.repo.Within(ctx, func(u Unit) error {
		zero, err := domain.Zero(initial.Currency())
		if err != nil {
			return err
		}
		empty, err := domain.NewWallet(w.View().ID, player, zero, now)
		if err != nil {
			return err
		}
		if err = u.CreateWallet(ctx, empty); err != nil {
			return err
		}
		if initial.Minor() == 0 {
			return nil
		}
		t, err := domain.NewOpening(w, now)
		if err != nil {
			return err
		}
		if _, err = u.InsertTransaction(ctx, t); err != nil {
			return err
		}
		l, err := domain.NewLedger(domain.LedgerView{ID: domain.ID(), WalletID: w.View().ID, TransactionID: t.View().ID, Direction: domain.Credit, Money: initial, Before: zero, After: initial, CreatedAt: now})
		if err != nil {
			return err
		}
		if err = u.Ledger(ctx, l); err != nil {
			return err
		}
		e, err := domain.ProcessedEvent(t.View(), t.View().ID, "")
		if err != nil {
			return err
		}
		if err = u.Event(ctx, e); err != nil {
			return err
		}
		e, err = domain.BalanceEvent(l, 1, t.View().ID, "")
		if err != nil {
			return err
		}
		return u.Event(ctx, e)
	})
	return w.View(), err
}
func (s *Service) Execute(ctx context.Context, in Input) (Result, error) {
	if err := in.Request.Validate(); err != nil {
		return Result{}, err
	}
	if !domain.ValidToken(in.Key) {
		return Result{}, domain.Rule("INVALID_IDEMPOTENCY_KEY")
	}
	candidate, err := domain.NewTransaction(domain.ID(), in.Key, in.Request, time.Now().UTC())
	if err != nil {
		return Result{}, err
	}
	var out Result
	err = s.repo.Within(ctx, func(u Unit) error {
		if in.MessageID != "" {
			cached, err := u.Inbox(ctx, in.MessageID, in.MessageHash)
			if err != nil {
				return err
			}
			if cached != nil {
				out = *cached
				out.Replay = true
				return nil
			}
		}
		// Detect an existing idempotency key before attempting to locate a changed wallet.
		previous, err := u.ByKeys(ctx, in.Request.ProviderID, in.Key, in.Request.ExternalID)
		if err != nil {
			return err
		}
		if len(previous) > 0 {
			if len(previous) != 1 {
				return ErrConflict
			}
			v := previous[0].View()
			if v.Key != in.Key || v.Hash != candidate.View().Hash {
				return ErrConflict
			}
			out = result(v, true)
			if in.MessageID != "" {
				return u.CompleteInbox(ctx, in.MessageID, out)
			}
			return nil
		}
		// Every writer locks this one wallet FIRST, before touching existing transactions.
		w, err := u.Wallet(ctx, in.Request.WalletID)
		if err != nil {
			return err
		}
		inserted, err := u.InsertTransaction(ctx, candidate)
		if err != nil {
			return err
		}
		if !inserted {
			rows, err := u.ByKeys(ctx, in.Request.ProviderID, in.Key, in.Request.ExternalID)
			if err != nil {
				return err
			}
			if len(rows) != 1 {
				return ErrConflict
			}
			old := rows[0].View()
			// Another key for the same external operation is an explicit conflict.
			if old.Key != in.Key || old.Hash != candidate.View().Hash {
				return ErrConflict
			}
			out = result(old, true)
		} else {
			if err = s.process(ctx, u, &w, &candidate, in, 0); err != nil {
				return err
			}
			out = result(candidate.View(), false)
		}
		if in.MessageID != "" {
			return u.CompleteInbox(ctx, in.MessageID, out)
		}
		return nil
	})
	return out, err
}
func (s *Service) process(ctx context.Context, u Unit, w *domain.Wallet, t *domain.WagerTransaction, in Input, attempts int) error {
	now := time.Now().UTC()
	v := t.View()
	r := v.Request
	wallet := w.View()
	before := wallet.Balance
	reject := func(code string) error {
		snapshot := &before
		if code == "PLAYER_MISMATCH" {
			snapshot = nil
		}
		if err := t.Transition(domain.Rejected, code, "", snapshot, now); err != nil {
			return err
		}
		if err := u.SaveTransaction(ctx, *t, attempts, now); err != nil {
			return err
		}
		e, err := domain.RejectedEvent(t.View(), in.CorrelationID, in.MessageID)
		if err != nil {
			return err
		}
		return u.Event(ctx, e)
	}
	pending := func() error {
		if attempts >= s.maxAttempts || now.Sub(v.CreatedAt) >= s.ttl {
			return reject("REFERENCE_NOT_FOUND")
		}
		first := v.State != domain.PendingReference
		if err := t.Transition(domain.PendingReference, "", "", nil, now); err != nil {
			return err
		}
		if err := u.SaveTransaction(ctx, *t, attempts+1, now.Add(Backoff(attempts))); err != nil {
			return err
		}
		if first {
			e, err := domain.PendingEvent(t.View(), in.CorrelationID, in.MessageID)
			if err != nil {
				return err
			}
			return u.Event(ctx, e)
		}
		return nil
	}
	if v.State == domain.PendingReference && (attempts >= s.maxAttempts || now.Sub(v.CreatedAt) >= s.ttl) {
		return reject("REFERENCE_NOT_FOUND")
	}
	if wallet.PlayerID != r.PlayerID {
		return reject("PLAYER_MISMATCH")
	}
	if before.Currency() != r.Money.Currency() {
		return reject("CURRENCY_MISMATCH")
	}
	direction := domain.Credit
	if r.Kind == domain.Bet {
		direction = domain.Debit
	}
	refID := ""
	if r.Reference != "" {
		ref, err := u.Reference(ctx, r.ProviderID, r.Reference)
		if errors.Is(err, ErrNotFound) {
			return pending()
		}
		if err != nil {
			return err
		}
		refID = ref.View().ID
		reversed, err := u.WasReversed(ctx, refID)
		if err != nil {
			return err
		}
		direction, err = domain.EvaluateReference(r, ref.View(), reversed)
		if err != nil {
			var rule *domain.RuleError
			if !errors.As(err, &rule) {
				return err
			}
			if rule.Code == "REFERENCE_PENDING" {
				return pending()
			}
			return reject(rule.Code)
		}
	}
	if r.Kind != domain.Loss {
		var err error
		if direction == domain.Debit {
			err = w.Debit(r.Money, now)
		} else {
			err = w.Credit(r.Money, now)
		}
		if err != nil {
			var rule *domain.RuleError
			if !errors.As(err, &rule) {
				return err
			}
			code := rule.Code
			if code == "INSUFFICIENT_FUNDS" && r.Kind == domain.Rollback {
				code = "REVERSAL_INSUFFICIENT_FUNDS"
			}
			return reject(code)
		}
	}
	after := w.View().Balance
	if err := t.Transition(domain.Processed, "", refID, &after, now); err != nil {
		return err
	}
	if err := u.SaveTransaction(ctx, *t, attempts, now); err != nil {
		return err
	}
	if r.Kind != domain.Loss {
		l, err := domain.NewLedger(domain.LedgerView{ID: domain.ID(), WalletID: r.WalletID, TransactionID: v.ID, Direction: direction, Money: r.Money, Before: before, After: after, CreatedAt: now})
		if err != nil {
			return err
		}
		if err = u.Ledger(ctx, l); err != nil {
			return err
		}
		e, err := domain.BalanceEvent(l, w.View().Version, in.CorrelationID, in.MessageID)
		if err != nil {
			return err
		}
		if err = u.Event(ctx, e); err != nil {
			return err
		}
	}
	e, err := domain.ProcessedEvent(t.View(), in.CorrelationID, in.MessageID)
	if err != nil {
		return err
	}
	return u.Event(ctx, e)
}
func (s *Service) Resume(ctx context.Context, id string) error {
	_, err := s.ResumeResult(ctx, id)
	return err
}
func (s *Service) ResumeResult(ctx context.Context, id string) (*Result, error) {
	var out *Result
	err := s.repo.Within(ctx, func(u Unit) error {
		initial, err := u.Transaction(ctx, id, false)
		if err != nil {
			return err
		}
		w, err := u.Wallet(ctx, initial.View().Request.WalletID)
		if err != nil {
			return err
		}
		t, err := u.Transaction(ctx, id, true)
		if err != nil {
			return err
		}
		if t.Terminal() {
			return nil
		}
		attempts, due, err := u.Attempts(ctx, id)
		if err != nil {
			return err
		}
		if time.Now().Before(due) {
			return nil
		}
		in := Input{Request: t.View().Request, Key: t.View().Key, CorrelationID: id}
		if err = s.process(ctx, u, &w, &t, in, attempts); err != nil {
			return err
		}
		r := result(t.View(), false)
		out = &r
		return nil
	})
	return out, err
}

// FailPending is used only after a rolled-back attempt with a positively classified,
// permanent storage data error. Unknown outcomes and connection failures never reach it.
func (s *Service) FailPending(ctx context.Context, id string) error {
	return s.repo.Within(ctx, func(u Unit) error {
		initial, err := u.Transaction(ctx, id, false)
		if err != nil {
			return err
		}
		if _, err = u.Wallet(ctx, initial.View().Request.WalletID); err != nil {
			return err
		}
		t, err := u.Transaction(ctx, id, true)
		if err != nil {
			return err
		}
		if t.Terminal() {
			return nil
		}
		attempts, _, err := u.Attempts(ctx, id)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if err = t.Transition(domain.Failed, "PERMANENT_STORAGE_ERROR", "", nil, now); err != nil {
			return err
		}
		return u.SaveTransaction(ctx, t, attempts, now)
	})
}
