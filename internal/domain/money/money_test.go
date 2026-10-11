package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestNewExternalValidatesFixedDecimalAmounts(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		currency string
		want     int64
		wantErr  error
	}{
		{name: "zero", amount: "0.00", currency: "BRL", want: 0},
		{name: "positive", amount: "123.45", currency: "BRL", want: 12345},
		{name: "empty", amount: "", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "missing fraction", amount: "1", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "short fraction", amount: "1.0", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "long fraction", amount: "1.000", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "exponent", amount: "1e3", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "leading zero", amount: "01.00", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "negative", amount: "-1.00", currency: "BRL", wantErr: ErrNegativeAmount},
		{name: "whitespace", amount: " 1.00", currency: "BRL", wantErr: ErrInvalidAmount},
		{name: "invalid currency", amount: "1.00", currency: "brl", wantErr: ErrInvalidCurrency},
		{name: "unknown currency", amount: "1.00", currency: "ABC", wantErr: ErrInvalidCurrency},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewExternal(tt.amount, tt.currency)
			if tt.wantErr != nil {
				if err != tt.wantErr {
					t.Fatalf("NewExternal() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewExternal() error = %v", err)
			}
			minor, err := got.AmountMinor()
			if err != nil || minor != tt.want {
				t.Fatalf("AmountMinor() = %d, %v; want %d", minor, err, tt.want)
			}
		})
	}
}

func TestParseSupportsSignedInternalValuesAndInt64Minimum(t *testing.T) {
	maximum, err := Parse("92233720368547758.07", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	maximumMinor, err := maximum.AmountMinor()
	if err != nil || maximumMinor != math.MaxInt64 {
		t.Fatalf("maximum AmountMinor() = %d, %v; want %d", maximumMinor, err, int64(math.MaxInt64))
	}
	if _, err := Parse("92233720368547758.08", "BRL"); err != ErrOverflow {
		t.Fatalf("positive overflow error = %v, want %v", err, ErrOverflow)
	}
	if _, err := Parse("-92233720368547758.09", "BRL"); err != ErrOverflow {
		t.Fatalf("negative overflow error = %v, want %v", err, ErrOverflow)
	}

	got, err := Parse("-92233720368547758.08", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	minor, err := got.AmountMinor()
	if err != nil || minor != math.MinInt64 {
		t.Fatalf("AmountMinor() = %d, %v; want %d", minor, err, int64(math.MinInt64))
	}
	amount, err := got.String()
	if err != nil || amount != "-92233720368547758.08" {
		t.Fatalf("String() = %q, %v", amount, err)
	}
}

func TestArithmeticRejectsOverflowAndCurrencyMismatch(t *testing.T) {
	brlMax, _ := FromMinorUnits(math.MaxInt64, "BRL")
	brlOne, _ := FromMinorUnits(1, "BRL")
	brlMin, _ := FromMinorUnits(math.MinInt64, "BRL")
	usdOne, _ := FromMinorUnits(1, "USD")

	if _, err := brlMax.Add(brlOne); err != ErrOverflow {
		t.Fatalf("Add() error = %v, want %v", err, ErrOverflow)
	}
	if _, err := brlMin.Subtract(brlOne); err != ErrOverflow {
		t.Fatalf("Subtract() error = %v, want %v", err, ErrOverflow)
	}
	if _, err := brlMin.Negate(); err != ErrOverflow {
		t.Fatalf("Negate() error = %v, want %v", err, ErrOverflow)
	}
	if _, err := brlOne.Add(usdOne); err != ErrCurrencyMismatch {
		t.Fatalf("Add() error = %v, want %v", err, ErrCurrencyMismatch)
	}
}

func TestJSONUsesDecimalStringAndRejectsUninitializedMoney(t *testing.T) {
	value, err := NewExternal("25.30", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"amount":"25.30","currency":"BRL"}` {
		t.Fatalf("MarshalJSON() = %s", encoded)
	}

	var decoded Money
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if comparison, err := value.Compare(decoded); err != nil || comparison != 0 {
		t.Fatalf("decoded value comparison = %d, %v", comparison, err)
	}
	if _, err := json.Marshal(Money{}); !errors.Is(err, ErrUninitializedMoney) {
		t.Fatalf("MarshalJSON() error = %v, want %v", err, ErrUninitializedMoney)
	}
}
