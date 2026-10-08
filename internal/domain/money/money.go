package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

var (
	ErrInvalidAmount      = errors.New("invalid amount")
	ErrInvalidCurrency    = errors.New("invalid currency")
	ErrNegativeAmount     = errors.New("negative amount")
	ErrCurrencyMismatch   = errors.New("currency mismatch")
	ErrOverflow           = errors.New("money overflow")
	ErrUninitializedMoney = errors.New("uninitialized money")
)

type Money struct {
	minorUnits int64
	currency   string
}

func Parse(amount, currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidCurrency
	}
	if amount == "" {
		return Money{}, ErrInvalidAmount
	}

	negative := amount[0] == '-'
	if negative {
		amount = amount[1:]
	}
	if amount == "" {
		return Money{}, ErrInvalidAmount
	}

	parts := splitAmount(amount)
	if parts == nil || len(parts.fraction) != 2 || !digits(parts.whole) || !digits(parts.fraction) {
		return Money{}, ErrInvalidAmount
	}
	if len(parts.whole) > 1 && parts.whole[0] == '0' {
		return Money{}, ErrInvalidAmount
	}

	limit := uint64(math.MaxInt64)
	if negative {
		limit++
	}
	whole, err := parseDigits(parts.whole, limit/100)
	if err != nil {
		return Money{}, ErrInvalidAmount
	}
	fraction, _ := parseDigits(parts.fraction, 99)
	minor := whole*100 + fraction
	if minor > limit {
		return Money{}, ErrOverflow
	}
	if negative {
		if minor == uint64(math.MaxInt64)+1 {
			return Money{minorUnits: math.MinInt64, currency: currency}, nil
		}
		return Money{minorUnits: -int64(minor), currency: currency}, nil
	}
	return Money{minorUnits: int64(minor), currency: currency}, nil
}

func NewExternal(amount, currency string) (Money, error) {
	value, err := Parse(amount, currency)
	if err != nil {
		return Money{}, err
	}
	if value.minorUnits < 0 {
		return Money{}, ErrNegativeAmount
	}
	return value, nil
}

func Zero(currency string) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidCurrency
	}
	return Money{currency: currency}, nil
}

func (m Money) AmountMinor() (int64, error) {
	if !m.valid() {
		return 0, ErrUninitializedMoney
	}
	return m.minorUnits, nil
}

func (m Money) Currency() (string, error) {
	if !m.valid() {
		return "", ErrUninitializedMoney
	}
	return m.currency, nil
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if other.minorUnits > 0 && m.minorUnits > math.MaxInt64-other.minorUnits ||
		other.minorUnits < 0 && m.minorUnits < math.MinInt64-other.minorUnits {
		return Money{}, ErrOverflow
	}
	return Money{minorUnits: m.minorUnits + other.minorUnits, currency: m.currency}, nil
}

func (m Money) Subtract(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	if other.minorUnits == math.MinInt64 {
		if m.minorUnits >= 0 {
			return Money{}, ErrOverflow
		}
		return Money{minorUnits: m.minorUnits - other.minorUnits, currency: m.currency}, nil
	}
	return m.Add(Money{minorUnits: -other.minorUnits, currency: other.currency})
}

func (m Money) Negate() (Money, error) {
	if !m.valid() {
		return Money{}, ErrUninitializedMoney
	}
	if m.minorUnits == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{minorUnits: -m.minorUnits, currency: m.currency}, nil
}

func (m Money) Compare(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.minorUnits < other.minorUnits:
		return -1, nil
	case m.minorUnits > other.minorUnits:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) IsZero() bool {
	return m.valid() && m.minorUnits == 0
}

func (m Money) String() (string, error) {
	if !m.valid() {
		return "", ErrUninitializedMoney
	}
	var amount string
	if m.minorUnits == math.MinInt64 {
		amount = "-92233720368547758.08"
	} else {
		value := m.minorUnits
		if value < 0 {
			value = -value
			amount = fmt.Sprintf("-%d.%02d", value/100, value%100)
		} else {
			amount = fmt.Sprintf("%d.%02d", value/100, value%100)
		}
	}
	return amount, nil
}

func (m Money) MarshalJSON() ([]byte, error) {
	amount, err := m.String()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{Amount: amount, Currency: m.currency})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var value struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	parsed, err := Parse(value.Amount, value.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

func (m Money) valid() bool {
	return validCurrency(m.currency)
}

func (m Money) compatible(other Money) error {
	if !m.valid() || !other.valid() {
		return ErrUninitializedMoney
	}
	if m.currency != other.currency {
		return ErrCurrencyMismatch
	}
	return nil
}

type amountParts struct {
	whole    string
	fraction string
}

func splitAmount(value string) *amountParts {
	for i := range value {
		if value[i] == '.' {
			if i == 0 || i == len(value)-1 || contains(value[i+1:], '.') {
				return nil
			}
			return &amountParts{whole: value[:i], fraction: value[i+1:]}
		}
	}
	return nil
}

func parseDigits(value string, limit uint64) (uint64, error) {
	var result uint64
	for i := range value {
		digit := uint64(value[i] - '0')
		if result > (limit-digit)/10 {
			return 0, ErrOverflow
		}
		result = result*10 + digit
	}
	return result, nil
}

func digits(value string) bool {
	if value == "" {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func contains(value string, target byte) bool {
	for i := range value {
		if value[i] == target {
			return true
		}
	}
	return false
}

func validCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for i := range currency {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	return true
}
