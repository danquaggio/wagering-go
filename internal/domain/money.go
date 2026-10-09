// Package domain contains no transport, framework or persistence dependencies.
package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

type RuleError struct{ Code string }

func (e *RuleError) Error() string { return e.Code }
func Rule(code string) error       { return &RuleError{Code: code} }

type Money struct {
	minor    int64
	currency string
}

var decimal = regexp.MustCompile(`^(0|[1-9][0-9]*)\.[0-9]{2}$`)

// Parse accepts only unsigned canonical decimal strings with exactly two places.
func Parse(s, currency string) (Money, error) {
	if !decimal.MatchString(s) {
		return Money{}, Rule("INVALID_AMOUNT")
	}
	digits := strings.ReplaceAll(s, ".", "")
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Money{}, Rule("MONEY_OVERFLOW")
	}
	return Minor(n, currency)
}
func Minor(n int64, c string) (Money, error) {
	switch c {
	case "BRL", "USD", "EUR":
	default:
		return Money{}, Rule("INVALID_CURRENCY")
	}
	return Money{minor: n, currency: c}, nil
}
func Zero(c string) (Money, error) { return Minor(0, c) }
func (m Money) Valid() bool        { return m.currency == "BRL" || m.currency == "USD" || m.currency == "EUR" }
func (m Money) Minor() int64       { return m.minor }
func (m Money) Currency() string   { return m.currency }
func (m Money) compatible(b Money) error {
	if !m.Valid() || !b.Valid() {
		return Rule("UNINITIALIZED_MONEY")
	}
	if m.currency != b.currency {
		return Rule("CURRENCY_MISMATCH")
	}
	return nil
}
func (m Money) Add(b Money) (Money, error) {
	if err := m.compatible(b); err != nil {
		return Money{}, err
	}
	if (b.minor > 0 && m.minor > math.MaxInt64-b.minor) || (b.minor < 0 && m.minor < math.MinInt64-b.minor) {
		return Money{}, Rule("MONEY_OVERFLOW")
	}
	return Minor(m.minor+b.minor, m.currency)
}
func (m Money) Sub(b Money) (Money, error) {
	if err := m.compatible(b); err != nil {
		return Money{}, err
	}
	if (b.minor > 0 && m.minor < math.MinInt64+b.minor) || (b.minor < 0 && m.minor > math.MaxInt64+b.minor) {
		return Money{}, Rule("MONEY_OVERFLOW")
	}
	return Minor(m.minor-b.minor, m.currency)
}
func (m Money) Negate() (Money, error) {
	if !m.Valid() {
		return Money{}, Rule("UNINITIALIZED_MONEY")
	}
	if m.minor == math.MinInt64 {
		return Money{}, Rule("MONEY_OVERFLOW")
	}
	return Minor(-m.minor, m.currency)
}
func (m Money) Compare(b Money) (int, error) {
	if err := m.compatible(b); err != nil {
		return 0, err
	}
	if m.minor < b.minor {
		return -1, nil
	}
	if m.minor > b.minor {
		return 1, nil
	}
	return 0, nil
}
func (m Money) String() string {
	// Unsigned magnitude also supports MinInt64 without overflowing its negation.
	n := uint64(m.minor)
	prefix := ""
	if m.minor < 0 {
		n = uint64(-(m.minor + 1)) + 1
		prefix = "-"
	}
	return fmt.Sprintf("%s%d.%02d", prefix, n/100, n%100)
}
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.Valid() {
		return nil, Rule("UNINITIALIZED_MONEY")
	}
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{m.String(), m.currency})
}
func (m *Money) UnmarshalJSON(b []byte) error {
	var v struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return Rule("INVALID_MONEY")
	}
	n, err := Parse(v.Amount, v.Currency)
	if err != nil {
		return err
	}
	*m = n
	return nil
}
