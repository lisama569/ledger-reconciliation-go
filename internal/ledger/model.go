package ledger

import (
	"strconv"
	"time"
)

// Currency 币种代码，如 CNY/USD。
type Currency string

// AccountStatus 账户状态。
type AccountStatus string

const (
	StatusActive AccountStatus = "active"
	StatusFrozen AccountStatus = "frozen"
	StatusClosed AccountStatus = "closed"
)

// Account 账户。Balance 以最小货币单位的整数表示（如分）。
type Account struct {
	ID        string
	Currency  Currency
	Balance   int64
	Status    AccountStatus
	Version   int64
	CreatedAt time.Time
}

// Direction 分录方向。
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Entry 单条分录。
type Entry struct {
	AccountID string
	Currency  Currency
	Amount    int64
	Direction Direction
}

// Transaction 一张记账凭证，由至少两条借贷平衡的分录组成。
type Transaction struct {
	// BusinessID 即业务单号，幂等键。
	BusinessID string
	Entries    []Entry
	CreatedAt  time.Time
}

// TransferRequest 双分录转账请求。
type TransferRequest struct {
	BusinessID string
	FromID     string
	ToID       string
	Currency   Currency
	Amount     int64
	Reference  string
}

// TransferResult 转账结果；Duplicated=true 表示命中幂等、返回的是首次结果。
// FromID/ToID/Currency/Amount 构成业务语义指纹，用于识别同号冲突。
type TransferResult struct {
	BusinessID string
	FromID     string
	ToID       string
	Currency   Currency
	Amount     int64
	Entries    []Entry
	Duplicated bool
}

// fingerprint 返回业务语义指纹（不含 Reference 等可空描述字段）。
func (r TransferResult) fingerprint() string {
	return string(r.Currency) + "|" + r.FromID + "->" + r.ToID + "|" +
		strconv.FormatInt(r.Amount, 10)
}

// reqFingerprint 计算请求的语义指纹。
func reqFingerprint(req TransferRequest) string {
	return TransferResult{
		Currency: req.Currency,
		FromID:   req.FromID,
		ToID:     req.ToID,
		Amount:   req.Amount,
	}.fingerprint()
}
