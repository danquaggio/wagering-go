package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func money(t *testing.T, s string) Money {
	t.Helper()
	m, err := Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func TestMoneyParsing(t *testing.T) {
	for _, s := range []string{"", "NaN", "Infinity", "1e2", "1", "1.0", "1.000", "01.00", "-1.00", "+1.00", " 1.00", "1,00", "92233720368547758.08"} {
		t.Run(s, func(t *testing.T) {
			if _, err := Parse(s, "BRL"); err == nil {
				t.Fatalf("accepted %q", s)
			}
		})
	}
	for _, s := range []string{"0.00", "0.01", "25.00", "92233720368547758.07"} {
		m := money(t, s)
		if m.String() != s {
			t.Fatal(m.String())
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var round Money
		if err = json.Unmarshal(b, &round); err != nil {
			t.Fatal(err)
		}
		if cmp, err := m.Compare(round); err != nil || cmp != 0 {
			t.Fatal("round trip")
		}
	}
	for _, raw := range []string{`{"amount":25.00,"currency":"BRL"}`, `{"amount":"25.00","currency":"ABC"}`, `null`, `{}`, `{"amount":"0.00","currency":"BRL","extra":1}`} {
		var m Money
		if err := json.Unmarshal([]byte(raw), &m); err == nil {
			t.Fatal(raw)
		}
	}
}
func TestMoneyLimits(t *testing.T) {
	max, _ := Minor(math.MaxInt64, "BRL")
	min, _ := Minor(math.MinInt64, "BRL")
	one, _ := Minor(1, "BRL")
	negative, _ := Minor(-1, "BRL")
	for _, f := range []func() (Money, error){
		func() (Money, error) { return max.Add(one) }, func() (Money, error) { return min.Add(negative) },
		func() (Money, error) { return min.Sub(one) }, func() (Money, error) { return max.Sub(negative) }, min.Negate,
	} {
		if _, err := f(); err == nil {
			t.Fatal("overflow accepted")
		}
	}
	if min.String() != "-92233720368547758.08" {
		t.Fatal(min.String())
	}
	got, err := min.Sub(min)
	if err != nil || got.Minor() != 0 {
		t.Fatal(got, err)
	}
	got, err = max.Add(min)
	if err != nil || got.Minor() != -1 {
		t.Fatal(got, err)
	}
	a := money(t, "0.10")
	b := money(t, "0.20")
	got, err = a.Add(b)
	if err != nil || got.String() != "0.30" {
		t.Fatal(got, err)
	}
	usd, _ := Parse("1.00", "USD")
	for _, f := range []func() error{
		func() error { _, e := a.Add(usd); return e }, func() error { _, e := a.Sub(usd); return e }, func() error { _, e := a.Compare(usd); return e },
		func() error { _, e := (Money{}).Add(a); return e }, func() error { _, e := json.Marshal(Money{}); return e },
	} {
		if err := f(); err == nil {
			t.Fatal("invalid money accepted")
		}
	}
	var r *RuleError
	_, err = max.Add(one)
	if !errors.As(err, &r) || r.Code != "MONEY_OVERFLOW" {
		t.Fatal(err)
	}
}
