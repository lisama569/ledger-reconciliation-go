package recon

import "time"

// Receipt 外部结算回执（本地模拟的外部结算结果）。
type Receipt struct {
	// ReceiptID 外部结算流水号。
	ReceiptID string
	// BusinessID 对应的业务单号（可能缺失/错误 -> 未匹配）。
	BusinessID string
	Currency   string
	Amount     int64
	SettledAt  time.Time
}

// DiscrepancyType 对账差异分类。
type DiscrepancyType string

const (
	// MatchOK 账实一致。
	MatchOK DiscrepancyType = "match_ok"
	// UnmatchedReceipt 回执无对应账本凭证。
	UnmatchedReceipt DiscrepancyType = "unmatched_receipt"
	// UnmatchedEntry 账本凭证无对应回执（以补对方向，占位）。
	UnmatchedEntry DiscrepancyType = "unmatched_entry"
	// AmountMismatch 金额不符。
	AmountMismatch DiscrepancyType = "amount_mismatch"
	// CurrencyMismatch 币种不符。
	CurrencyMismatch DiscrepancyType = "currency_mismatch"
	// DuplicateReceipt 同一回执号重复提交。
	DuplicateReceipt DiscrepancyType = "duplicate_receipt"
)

// Discrepancy 单条差异记录。
type Discrepancy struct {
	Type       DiscrepancyType
	ReceiptID  string
	BusinessID string
	Detail     string
}

// Report 对账报告。
type Report struct {
	Matched       []string
	Discrepancies []Discrepancy
}

// HasDiff 报告是否存在差异。
func (r *Report) HasDiff() bool { return len(r.Discrepancies) > 0 }
