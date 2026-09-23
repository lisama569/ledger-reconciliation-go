package ledger

import (
	"fmt"
	"sync"
)

// SettlementReceipt 外部结算回执（本地模拟）。
type SettlementReceipt struct {
	ReceiptID string // 回执自身 ID（同一 ID 再次送达视为渠道重投）
	BizID     string // 对应业务单号
	Amount    int64
	Currency  string
	Source    string // 回执来源（模拟不同结算渠道）
}

// MatchedReceipt 对账匹配结果。
type MatchedReceipt struct {
	Receipt     *SettlementReceipt
	Txn         *Transaction
	Discrepancy ReconcileDiscrepancyType
}

// ReconcileReport 一轮对账报告。
type ReconcileReport struct {
	Matched       []*MatchedReceipt
	Discrepancies []*ReconcileError
}

// HasType 报告中是否存在某类差异。
func (rep *ReconcileReport) HasType(t ReconcileDiscrepancyType) bool {
	for _, d := range rep.Discrepancies {
		if d.Type == t {
			return true
		}
	}
	return false
}

// ReceiptStore 回执存储：按回执 ID 索引，模拟外部结算渠道的回执投递。
type ReceiptStore struct {
	mu       sync.RWMutex
	receipts map[string]*SettlementReceipt
}

// NewReceiptStore 创建空回执存储。
func NewReceiptStore() *ReceiptStore {
	return &ReceiptStore{receipts: make(map[string]*SettlementReceipt)}
}

// Add 存入一份回执。相同 ReceiptID 重复投递会覆盖旧内容，
// 是否属于"重复回执"由 Reconciler 依据消费记录判定。
func (s *ReceiptStore) Add(r *SettlementReceipt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts[r.ReceiptID] = r
}

// All 返回全部回执快照，顺序按 ReceiptID 稳定排序。
func (s *ReceiptStore) All() []*SettlementReceipt {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.receipts))
	for id := range s.receipts {
		ids = append(ids, id)
	}
	// 简单插入排序，避免引入 sort 包外的额外依赖
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	out := make([]*SettlementReceipt, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.receipts[id])
	}
	return out
}

// Reconciler 对账器：以账本流水为准，与外部回执逐笔核对。
// 对账过程对账本只读：任何回执异常都不会改动余额或流水，
// 因此重试或部分失败不会导致重复/漏记账务。
type Reconciler struct {
	ledger   *Ledger
	receipts *ReceiptStore

	mu               sync.Mutex
	matchedByBiz     map[string]string // bizID -> 已匹配的 receiptID
	consumedReceipts map[string]bool   // receiptID -> 是否已成功消费
}

// NewReconciler 创建对账器。
func NewReconciler(l *Ledger, rs *ReceiptStore) *Reconciler {
	return &Reconciler{
		ledger:           l,
		receipts:         rs,
		matchedByBiz:     make(map[string]string),
		consumedReceipts: make(map[string]bool),
	}
}

// Run 执行一轮对账。分类规则：
//   - DUPLICATE_RECEIPT：已成功消费的回执再次送达，或同一业务单出现第二份回执；
//   - UNMATCHED_RECEIPT：回执 BizID 在账本中找不到流水；
//   - CURRENCY_MISMATCH：币种与流水不符；
//   - AMOUNT_MISMATCH：金额与流水不符；
//   - MISSING_RECEIPT：账本有流水但始终没有可匹配的回执。
//
// 只有完全匹配的回执才会写入消费记录；异常回执不写任何状态，
// 因此重试会再次如实报告同一异常，账本本身不受影响。
func (r *Reconciler) Run() *ReconcileReport {
	r.mu.Lock()
	defer r.mu.Unlock()

	_, entries, _ := r.ledger.Stores()
	rep := &ReconcileReport{}
	seenBiz := make(map[string]bool) // 本轮出现过回执的业务单（含异常回执）

	for _, rc := range r.receipts.All() {
		seenBiz[rc.BizID] = true
		// 渠道重投：同一回执已消费过
		if r.consumedReceipts[rc.ReceiptID] {
			rep.Discrepancies = append(rep.Discrepancies, &ReconcileError{
				Type:    DiscrepancyDuplicateReceipt,
				Receipt: rc,
				EntryID: r.matchedByBiz[rc.BizID],
				Detail:  fmt.Sprintf("receipt %s already consumed", rc.ReceiptID),
			})
			continue
		}

		txn, ok := entries.GetByBizID(rc.BizID)
		if !ok {
			rep.Discrepancies = append(rep.Discrepancies, &ReconcileError{
				Type:    DiscrepancyUnmatched,
				Receipt: rc,
				Detail:  fmt.Sprintf("no ledger transaction for bizID=%s", rc.BizID),
			})
			continue
		}

		if rc.Currency != txn.Money.Currency {
			rep.Discrepancies = append(rep.Discrepancies, &ReconcileError{
				Type:    DiscrepancyCurrencyMismatch,
				Receipt: rc,
				EntryID: txn.ID,
				Detail:  fmt.Sprintf("receipt currency=%s ledger currency=%s", rc.Currency, txn.Money.Currency),
			})
			continue
		}
		if rc.Amount != txn.Money.Amount {
			rep.Discrepancies = append(rep.Discrepancies, &ReconcileError{
				Type:    DiscrepancyAmountMismatch,
				Receipt: rc,
				EntryID: txn.ID,
				Detail:  fmt.Sprintf("receipt amount=%d ledger amount=%d", rc.Amount, txn.Money.Amount),
			})
			continue
		}

		// 同一业务单已经被另一份回执匹配：第二份为重复回执
		if prev, exists := r.matchedByBiz[rc.BizID]; exists && prev != rc.ReceiptID {
			rep.Discrepancies = append(rep.Discrepancies, &ReconcileError{
				Type:    DiscrepancyDuplicateReceipt,
				Receipt: rc,
				EntryID: txn.ID,
				Detail:  fmt.Sprintf("bizID=%s already matched by receipt=%s", rc.BizID, prev),
			})
			continue
		}

		// 完全匹配，登记消费状态
		r.matchedByBiz[rc.BizID] = rc.ReceiptID
		r.consumedReceipts[rc.ReceiptID] = true
		rep.Matched = append(rep.Matched, &MatchedReceipt{
			Receipt:     rc,
			Txn:         txn,
			Discrepancy: DiscrepancyNone,
		})
	}

	// 反向核对：有流水但本轮没有任何回执的业务单才算缺回执；
	// 有回执但金额/币种不符的，差异已在前面分类，避免重复归因。
	for _, txn := range entries.All() {
		if seenBiz[txn.BizID] {
			continue
		}
		if _, ok := r.matchedByBiz[txn.BizID]; !ok {
			rep.Discrepancies = append(rep.Discrepancies, &ReconcileError{
				Type:    DiscrepancyMissingReceipt,
				EntryID: txn.ID,
				Detail:  fmt.Sprintf("no settlement receipt for bizID=%s", txn.BizID),
			})
		}
	}

	return rep
}
