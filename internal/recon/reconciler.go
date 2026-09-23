package recon

import (
	"context"
	"fmt"
	"sync"
)

// LedgerTx 描述对账所需的账本凭证信息（由 ledger 包适配器提供）。
type LedgerTx struct {
	BusinessID string
	Currency   string
	// Amount 转账的单边发生额（借贷各一条，金额相同）。
	Amount int64
}

// LedgerView 账本只读视图。
type LedgerView interface {
	ListTransactions(ctx context.Context) ([]LedgerTx, error)
}

// Reconciler 结算回执对账器：本地记录已处理回执，
// 保证重试/部分失败重放时不重复判定、不漏记。
type Reconciler struct {
	mu              sync.Mutex
	ledger          LedgerView
	processed       map[string]DiscrepancyType
	matchedBusiness map[string]struct{}
}

// New 创建对账器。
func New(ledger LedgerView) *Reconciler {
	return &Reconciler{
		ledger:          ledger,
		processed:       map[string]DiscrepancyType{},
		matchedBusiness: map[string]struct{}{},
	}
}

// Reconcile 对一批回执做对账。
//
// 判定顺序（每张回执）：
//  1. duplicate_receipt：ReceiptID 在历史批次（或本批次）已处理过，
//     直接回放首次结论，不再次产生账务效果。
//  2. unmatched_receipt：BusinessID 在账本中无对应凭证。
//  3. currency_mismatch：币种与凭证不一致。
//  4. amount_mismatch：金额与凭证不一致。
//  5. match_ok：账实一致。
//
// 批次内还会反向识别 unmatched_entry：账本有凭证而回执未出现，
// 防止“部分失败后漏记”被掩盖。
func (r *Reconciler) Reconcile(ctx context.Context, receipts []Receipt) (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	txns, err := r.ledger.ListTransactions(ctx)
	if err != nil {
		return nil, err
	}
	byBusiness := make(map[string]LedgerTx, len(txns))
	for _, t := range txns {
		byBusiness[t.BusinessID] = t
	}

	rep := &Report{Matched: []string{}}
	seenInBatch := map[string]struct{}{}
	matchedBusiness := map[string]struct{}{}

	for _, rc := range receipts {
		// 1) 重复回执：先查历史，再查本批次
		if first, ok := r.processed[rc.ReceiptID]; ok {
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       DuplicateReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail: fmt.Sprintf("receipt already processed with first conclusion %s",
					first),
			})
			continue
		}
		if _, dup := seenInBatch[rc.ReceiptID]; dup {
			r.processed[rc.ReceiptID] = DuplicateReceipt
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       DuplicateReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     "duplicate receipt within the same batch",
			})
			continue
		}
		seenInBatch[rc.ReceiptID] = struct{}{}

		// 2) 未匹配：账本无此业务单号
		txn, ok := byBusiness[rc.BusinessID]
		if !ok {
			r.processed[rc.ReceiptID] = UnmatchedReceipt
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       UnmatchedReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     "no ledger transaction for business id",
			})
			continue
		}

		// 3) 币种不符
		if txn.Currency != rc.Currency {
			r.processed[rc.ReceiptID] = CurrencyMismatch
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       CurrencyMismatch,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail: fmt.Sprintf("ledger currency %s != receipt currency %s",
					txn.Currency, rc.Currency),
			})
			continue
		}

		// 4) 金额不符
		if txn.Amount != rc.Amount {
			r.processed[rc.ReceiptID] = AmountMismatch
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       AmountMismatch,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail: fmt.Sprintf("ledger amount %d != receipt amount %d",
					txn.Amount, rc.Amount),
			})
			continue
		}

		// 5) 匹配成功
		r.processed[rc.ReceiptID] = MatchOK
		r.matchedBusiness[rc.BusinessID] = struct{}{}
		rep.Matched = append(rep.Matched, rc.ReceiptID)
		matchedBusiness[rc.BusinessID] = struct{}{}
	}

	// 反向核对：账本有凭证但本批回执没出现 -> 可能漏记（仅对从未匹配过的单号）。
	for _, t := range txns {
		if _, m := matchedBusiness[t.BusinessID]; m {
			continue
		}
		if r.businessEverMatched(t.BusinessID) {
			continue
		}
		rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
			Type:       UnmatchedEntry,
			BusinessID: t.BusinessID,
			Detail:     "ledger transaction has no matching receipt (possible missing settlement)",
		})
	}

	return rep, nil
}

// businessEverMatched 判断业务单号是否曾在历史批次匹配成功。
func (r *Reconciler) businessEverMatched(businessID string) bool {
	_, ok := r.matchedBusiness[businessID]
	return ok
}
