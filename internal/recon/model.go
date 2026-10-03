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

// DiscrepancyType 对账差异分类（回执级：每张回执至多一条结论）。
type DiscrepancyType string

const (
	// MatchOK 账实一致。
	MatchOK DiscrepancyType = "match_ok"
	// UnmatchedReceipt 回执无对应账本凭证。
	UnmatchedReceipt DiscrepancyType = "unmatched_receipt"
	// UnmatchedEntry 账本凭证无对应回执（以补对方向，占位）。
	UnmatchedEntry DiscrepancyType = "unmatched_entry"
	// AmountMismatch 金额不符（单笔模式下首张有效回执金额就对不上）。
	AmountMismatch DiscrepancyType = "amount_mismatch"
	// CurrencyMismatch 币种不符。
	CurrencyMismatch DiscrepancyType = "currency_mismatch"
	// DuplicateReceipt 同一回执号重复提交。
	DuplicateReceipt DiscrepancyType = "duplicate_receipt"
	// OverReceived 超收：分次累积后超过账本金额，或已结清后又来了新流水。
	// 多出的部分不计入正常结算。
	OverReceived DiscrepancyType = "over_received"
)

// Discrepancy 单条差异记录。
type Discrepancy struct {
	Type       DiscrepancyType
	ReceiptID  string
	BusinessID string
	Detail     string
}

// BusinessStatus 业务单级别的最终结论（每个业务单在同一批里只能有一个）。
type BusinessStatus string

const (
	// StatusUnseen 本批视角下未收到任何回执（仍可能是历史部分结算，
	// 该状态只用于批次快照，历史已有回执的业务单不会再被判成“没有回执”）。
	StatusUnseen BusinessStatus = "unseen"
	// StatusPartial 部分结算：已收到至少一张有效回执，但累计金额小于账本金额。
	StatusPartial BusinessStatus = "partial_settled"
	// StatusSettled 结清：有效回执累计金额恰好等于账本金额。
	StatusSettled BusinessStatus = "settled"
	// StatusOverReceived 超收：单笔或累计金额超过账本金额，或结清后又来新流水。
	StatusOverReceived BusinessStatus = "over_received"
	// StatusCurrencyMismatch 币种不符：收到过币种与凭证不一致的回执。
	StatusCurrencyMismatch BusinessStatus = "currency_mismatch"
)

// SettledReceipt 计入业务单正常结算累计的一笔到账。
type SettledReceipt struct {
	ReceiptID string
	Amount    int64
}

// BusinessResult 单个业务单在某一批跑完后的对账结论与累计明细。
type BusinessResult struct {
	BusinessID   string
	Status       BusinessStatus
	LedgerAmount int64
	Currency     string
	// SettledAmount 当前累计已正常结算金额（超收多出的部分不计入）。
	SettledAmount int64
	// Receipts 累计金额由哪些外部流水号、各多少金额组成（按首次入账顺序）。
	Receipts []SettledReceipt
	// OverflowAmount 超收多出的金额（0 表示没有超收部分）。
	OverflowAmount int64
	// ExtraReceiptIDs 结清/超收之后又到达的多余流水号（含超收那一笔）。
	ExtraReceiptIDs []string
	Detail          string
}

// BatchSummary 一批对账的汇总计数。
type BatchSummary struct {
	Total            int // 本批给出结论的业务单数（含没收到回执的）
	Settled          int
	Partial          int
	OverReceived     int
	CurrencyMismatch int
	UnmatchedEntry   int
}

// Report 对账报告（按批次留痕）。
type Report struct {
	// BatchID 批次标识；同一批次重跑返回同一份报告，不重复产生差异。
	BatchID string
	// Matched 本批中促成/确认结清的新有效回执流水号（兼容上一轮单笔模式）。
	Matched []string
	// Discrepancies 回执级差异 + unmatched_entry 反向差异。
	Discrepancies []Discrepancy
	// BusinessResults 业务单级结论：同一业务单在本报告里恰好出现一次。
	BusinessResults []BusinessResult
	// Summary 按业务单结论统计的汇总。
	Summary BatchSummary
}

// HasDiff 报告是否存在差异。
func (r *Report) HasDiff() bool { return len(r.Discrepancies) > 0 }

// Business 按业务单号查询本批报告中的结论与累计明细。
func (r *Report) Business(businessID string) (BusinessResult, bool) {
	for _, b := range r.BusinessResults {
		if b.BusinessID == businessID {
			return b, true
		}
	}
	return BusinessResult{}, false
}
