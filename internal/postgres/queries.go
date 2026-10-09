package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"example.com/wagering/internal/application"
	"example.com/wagering/internal/domain"
	"math/big"
	"strconv"
)

type Page struct {
	Entries []domain.LedgerView `json:"entries"`
	Next    string              `json:"nextCursor,omitempty"`
}
type cursor struct {
	Wallet string `json:"w"`
	Seq    int64  `json:"s"`
}

func (s *Store) Ledger(ctx context.Context, wallet, cur string, limit int) (Page, error) {
	last := int64(0)
	if cur != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cur)
		if err != nil {
			return Page{}, domain.Rule("INVALID_CURSOR")
		}
		var c cursor
		if json.Unmarshal(raw, &c) != nil || c.Wallet != wallet || c.Seq < 0 {
			return Page{}, domain.Rule("INVALID_CURSOR")
		}
		last = c.Seq
	}
	if limit < 1 || limit > 200 {
		return Page{}, domain.Rule("INVALID_LIMIT")
	}
	if _, err := s.Wallet(ctx, wallet); err != nil {
		return Page{}, err
	}
	rows, err := s.Pool.Query(ctx, "SELECT seq,id::text,transaction_id::text,direction,amount,currency,balance_before,balance_after,created_at FROM wallet_ledger WHERE wallet_id=$1 AND seq>$2 ORDER BY seq LIMIT $3", wallet, last, limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	p := Page{Entries: []domain.LedgerView{}}
	seq := last
	for rows.Next() {
		var v domain.LedgerView
		var rowseq, amount, before, after int64
		var currency string
		v.WalletID = wallet
		if err = rows.Scan(&rowseq, &v.ID, &v.TransactionID, &v.Direction, &amount, &currency, &before, &after, &v.CreatedAt); err != nil {
			return Page{}, err
		}
		if len(p.Entries) == limit {
			b, err := json.Marshal(cursor{wallet, seq})
			if err != nil {
				return Page{}, err
			}
			p.Next = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		v.Money, err = domain.Minor(amount, currency)
		if err != nil {
			return Page{}, err
		}
		v.Before, err = domain.Minor(before, currency)
		if err != nil {
			return Page{}, err
		}
		v.After, err = domain.Minor(after, currency)
		if err != nil {
			return Page{}, err
		}
		seq = rowseq
		p.Entries = append(p.Entries, v)
	}
	return p, rows.Err()
}

type Reconciliation struct {
	WalletID   string       `json:"walletId"`
	Stored     domain.Money `json:"storedBalance"`
	Calculated domain.Money `json:"calculatedBalance"`
	Difference domain.Money `json:"difference"`
	Consistent bool         `json:"consistent"`
	Count      int64        `json:"checkedEntries"`
}

func (s *Store) Reconcile(ctx context.Context, id string) (Reconciliation, error) {
	// One SQL statement = one MVCC snapshot for wallet AND ledger.
	var stored int64
	var sum, currency string
	out := Reconciliation{WalletID: id}
	err := s.Pool.QueryRow(ctx, `SELECT w.balance,w.currency,COALESCE(sum(CASE l.direction WHEN 'CREDIT' THEN l.amount::numeric ELSE -l.amount::numeric END),0)::text,count(l.id)
 FROM wallets w LEFT JOIN wallet_ledger l ON l.wallet_id=w.id WHERE w.id=$1 GROUP BY w.id`, id).Scan(&stored, &currency, &sum, &out.Count)
	if err != nil {
		return out, convert(err)
	}
	n, ok := new(big.Int).SetString(sum, 10)
	if !ok || !n.IsInt64() {
		return out, errors.New("reconciliation outside int64 range")
	}
	out.Stored, err = domain.Minor(stored, currency)
	if err != nil {
		return out, err
	}
	out.Calculated, err = domain.Minor(n.Int64(), currency)
	if err != nil {
		return out, err
	}
	out.Difference, err = out.Stored.Sub(out.Calculated)
	if err != nil {
		return out, err
	}
	out.Consistent = out.Difference.Minor() == 0
	return out, nil
}
func Limit(s string) (int, error) {
	if s == "" {
		return 50, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 200 {
		return 0, domain.Rule("INVALID_LIMIT")
	}
	return n, nil
}

var _ application.Repository = (*Store)(nil)
