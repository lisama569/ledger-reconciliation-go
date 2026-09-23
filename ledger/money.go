package ledger

import "fmt"

// Money 表示一笔不可变的货币金额：金额使用最小货币单位的整数保存，
// 避免浮点误差；currency 为 ISO 4217 风格的币种代码（如 CNY/USD）。
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// NewMoney 构造金额对象。
func NewMoney(amount int64, currency string) Money {
	return Money{Amount: amount, Currency: currency}
}

// String 返回可读表示。
func (m Money) String() string {
	return fmt.Sprintf("%d %s", m.Amount, m.Currency)
}

// SameCurrency 判断两笔金额币种是否相同。
func (m Money) SameCurrency(other Money) bool {
	return m.Currency == other.Currency
}

// Positive 金额是否为正。
func (m Money) Positive() bool {
	return m.Amount > 0
}

// Equal 金额与币种均相等。
func (m Money) Equal(other Money) bool {
	return m.Amount == other.Amount && m.Currency == other.Currency
}
