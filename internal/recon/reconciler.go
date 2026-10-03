package recon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// receiptRecord 一张回执首次处理时的结论（重放用）。
type receiptRecord struct {
	firstType DiscrepancyType
	detail    string
}

// Reconciler 结算回执对账器：
//   - 同一业务单支持分多次、乱序、跨批次到账，按外部流水号逐笔累积；
//   - processed 保证回执重放不重复产生账务效果；
//   - businesses 保存每个业务单跨批次的累积状态，结论只能向更完整演进；
//   - batches 按批次留痕，同一批重跑返回同一份报告。
type Reconciler struct {
	mu sync.Mutex

	ledger     LedgerView
	processed  map[string]receiptRecord
	businesses map[string]*bizState
	batches    map[string]*Report
	batchSeq   uint64
}

// New 创建对账器。
func New(ledger LedgerView) *Reconciler {
	return &Reconciler{
		ledger:     ledger,
		processed:  map[string]receiptRecord{},
		businesses: map[string]*bizState{},
		batches:    map[string]*Report{},
	}
}

// Reconcile 对一批回执做对账，自动生成批次号。
func (r *Reconciler) Reconcile(ctx context.Context, receipts []Receipt) (*Report, error) {
	return r.ReconcileBatch(ctx, "", receipts)
}

// ReconcileBatch 对一批回执做对账并按 batchID 留痕。
//
// batchID 为空时自动生成；同一 batchID 重跑（含上一轮“部分失败后整批重放”）
// 直接返回首次报告，不重复产生任何差异记录。
//
// 每张回执自上而下首个命中即回执级结论：
//  1. duplicate_receipt：ReceiptID 已处理过（历史/本批次），回放首次结论；
//  2. unmatched_receipt：BusinessID 在账本中无对应凭证；
//  3. currency_mismatch：币种与凭证不一致（业务单级结论粘性保留）；
//  4. 币种正确则进入分次累积：计入未结清额度；
//     累计 == 账本金额 -> 结清；累计 < 账本金额 -> 部分结算；
//     累计 > 账本金额 -> over_received，多出部分不计入正常结算；
//     已结清后再来的新流水一律按多余的重复结算处理。
//
// 反向核对：账本有凭证但从未收到任何回执（连金额/币种不符的都没有）
// -> unmatched_entry；收到过回执后该结论永久消除。
func (r *Reconciler) ReconcileBatch(ctx context.Context, batchID string, receipts []Receipt) (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if batchID != "" {
		if rep, ok := r.batches[batchID]; ok {
			return cloneReport(rep), nil
		}
	} else {
		batchID = "batch-" + strconv.FormatUint(atomic.AddUint64(&r.batchSeq, 1), 10)
	}

	txns, err := r.ledger.ListTransactions(ctx)
	if err != nil {
		return nil, err
	}
	byBusiness := make(map[string]LedgerTx, len(txns))
	for _, t := range txns {
		byBusiness[t.BusinessID] = t
	}

	rep := &Report{
		BatchID:       batchID,
		Matched:       []string{},
		Discrepancies: []Discrepancy{},
	}
	seenInBatch := map[string]struct{}{}
	// touched：本批出现过新结论的业务单（币种错误或币种正确的新回执）。
	touched := map[string]struct{}{}
	notes := map[string][]string{}

	note := func(biz, format string, args ...any) {
		notes[biz] = append(notes[biz], fmt.Sprintf(format, args...))
	}

	getOrCreate := func(t LedgerTx) *bizState {
		st := r.businesses[t.BusinessID]
		if st == nil {
			st = &bizState{businessID: t.BusinessID, currency: t.Currency, ledgerAmount: t.Amount}
			r.businesses[t.BusinessID] = st
		}
		return st
	}

	for _, rc := range receipts {
		// 1) 重复回执：先查历史，再查本批次。重放不改变任何累积状态。
		if rec, ok := r.processed[rc.ReceiptID]; ok {
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       DuplicateReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail: fmt.Sprintf("receipt replayed, first conclusion %s: %s",
					rec.firstType, rec.detail),
			})
			continue
		}
		if _, dup := seenInBatch[rc.ReceiptID]; dup {
			r.processed[rc.ReceiptID] = receiptRecord{
				firstType: DuplicateReceipt,
				detail:    "duplicate receipt within the same batch",
			}
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       DuplicateReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     "duplicate receipt within the same batch",
			})
			continue
		}
		seenInBatch[rc.ReceiptID] = struct{}{}

		// 2) 未匹配：账本无此业务单号（不产生业务单累积状态）。
		txn, ok := byBusiness[rc.BusinessID]
		if !ok {
			detail := "no ledger transaction for business id"
			r.processed[rc.ReceiptID] = receiptRecord{firstType: UnmatchedReceipt, detail: detail}
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       UnmatchedReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     detail,
			})
			continue
		}

		st := getOrCreate(txn)

		// 3) 币种不符：粘性结论，回执不参与金额累积。
		if txn.Currency != rc.Currency {
			detail := fmt.Sprintf("ledger currency %s != receipt currency %s",
				txn.Currency, rc.Currency)
			r.processed[rc.ReceiptID] = receiptRecord{firstType: CurrencyMismatch, detail: detail}
			st.markCurrencyMismatch(rc.ReceiptID)
			touched[rc.BusinessID] = struct{}{}
			note(rc.BusinessID, "receipt %s rejected by currency (%s)", rc.ReceiptID, detail)
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       CurrencyMismatch,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     detail,
			})
			continue
		}

		// 4) 分次累积：乱序/跨批次同样按流水逐笔加总。
		before := st.status()
		counted, overflowAmt, kind := st.applyValidReceipt(rc.ReceiptID, rc.Amount)
		touched[rc.BusinessID] = struct{}{}

		switch kind {
		case MatchOK:
			r.processed[rc.ReceiptID] = receiptRecord{
				firstType: MatchOK,
				detail: fmt.Sprintf("counted %d, accumulated %d/%d",
					counted, st.settledAmount, st.ledgerAmount),
			}
			rep.Matched = append(rep.Matched, rc.ReceiptID)
			note(rc.BusinessID,
				"receipt %s amount %d counted, accumulated %d/%d -> %s",
				rc.ReceiptID, rc.Amount, st.settledAmount, st.ledgerAmount, st.status())
		case AmountMismatch:
			// 首张有效回执就超过账本金额：回执级保留单笔模式语义。
			detail := fmt.Sprintf("single receipt amount %d != ledger amount %d (overflow %d)",
				rc.Amount, txn.Amount, overflowAmt)
			r.processed[rc.ReceiptID] = receiptRecord{firstType: AmountMismatch, detail: detail}
			note(rc.BusinessID,
				"receipt %s amount %d overshoots ledger %d on first arrival, overflow %d not counted -> %s",
				rc.ReceiptID, rc.Amount, txn.Amount, overflowAmt, st.status())
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       AmountMismatch,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     detail,
			})
		case OverReceived:
			detail := fmt.Sprintf("over-received %d after status %s (ledger %d, settled %d)",
				overflowAmt, before, txn.Amount, st.settledAmount)
			r.processed[rc.ReceiptID] = receiptRecord{firstType: OverReceived, detail: detail}
			note(rc.BusinessID,
				"receipt %s amount %d is redundant after %s, overflow %d not counted -> %s",
				rc.ReceiptID, rc.Amount, before, overflowAmt, st.status())
			rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
				Type:       OverReceived,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				Detail:     detail,
			})
		}
	}

	// 5) 反向核对 + 组装业务单唯一结论。先处理本批触及的业务单。
	emitted := map[string]struct{}{}
	for biz := range touched {
		st := r.businesses[biz]
		rep.BusinessResults = append(rep.BusinessResults,
			st.toResult(joinNotes(notes[biz])))
		emitted[biz] = struct{}{}
	}
	// 从未收到过任何回执的账本凭证 -> unmatched_entry（每个业务单一条）。
	for _, t := range txns {
		if _, done := emitted[t.BusinessID]; done {
			continue
		}
		if st := r.businesses[t.BusinessID]; st != nil {
			// 历史收到过回执（哪怕金额/币种不符），本批不再当成没有回执，
			// 也不在本批重复给出结论。
			continue
		}
		rep.Discrepancies = append(rep.Discrepancies, Discrepancy{
			Type:       UnmatchedEntry,
			BusinessID: t.BusinessID,
			Detail:     "ledger transaction has no matching receipt (possible missing settlement)",
		})
		rep.BusinessResults = append(rep.BusinessResults, BusinessResult{
			BusinessID:   t.BusinessID,
			Status:       StatusUnseen,
			LedgerAmount: t.Amount,
			Currency:     t.Currency,
			Detail:       "no receipt received in any batch",
		})
	}

	rep.Summary = summarize(rep.BusinessResults)

	r.batches[batchID] = cloneReport(rep)
	return cloneReport(rep), nil
}

// Business 查询某业务单当前的结论与金额累计明细（跨批次最新状态）。
func (r *Reconciler) Business(ctx context.Context, businessID string) (BusinessResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.businesses[businessID]
	if st == nil {
		return BusinessResult{}, false
	}
	return st.toResult("current accumulated state across all batches"), true
}

// Batch 按批次号查询留痕报告。
func (r *Reconciler) Batch(batchID string) (*Report, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep, ok := r.batches[batchID]
	if !ok {
		return nil, false
	}
	return cloneReport(rep), true
}

// ListBatches 返回全部已留痕批次（按批次号排序）。
func (r *Reconciler) ListBatches() []*Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.batches))
	for id := range r.batches {
		ids = append(ids, id)
	}
	// 自动批次号 batch-N 按数字排序，显式批次名按字典序排序。
	sortStrings := func(a, b string) bool {
		na, oka := autoBatchSeq(a)
		nb, okb := autoBatchSeq(b)
		if oka && okb {
			return na < nb
		}
		return a < b
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && sortStrings(ids[j], ids[j-1]); j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	out := make([]*Report, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneReport(r.batches[id]))
	}
	return out
}

// autoBatchSeq 解析自动批次号 batch-N 的序号，非自动批次号返回 ok=false。
func autoBatchSeq(id string) (uint64, bool) {
	if !strings.HasPrefix(id, "batch-") {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "batch-"), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func joinNotes(notes []string) string {
	out := ""
	for i, n := range notes {
		if i > 0 {
			out += "; "
		}
		out += n
	}
	return out
}

func summarize(results []BusinessResult) BatchSummary {
	s := BatchSummary{Total: len(results)}
	for _, b := range results {
		switch b.Status {
		case StatusSettled:
			s.Settled++
		case StatusPartial:
			s.Partial++
		case StatusOverReceived:
			s.OverReceived++
		case StatusCurrencyMismatch:
			s.CurrencyMismatch++
		case StatusUnseen:
			s.UnmatchedEntry++
		}
	}
	return s
}

func cloneReport(rep *Report) *Report {
	if rep == nil {
		return nil
	}
	cp := *rep
	cp.Matched = append([]string(nil), rep.Matched...)
	cp.Discrepancies = append([]Discrepancy(nil), rep.Discrepancies...)
	cp.BusinessResults = make([]BusinessResult, len(rep.BusinessResults))
	for i, b := range rep.BusinessResults {
		b.Receipts = append([]SettledReceipt(nil), b.Receipts...)
		b.ExtraReceiptIDs = append([]string(nil), b.ExtraReceiptIDs...)
		cp.BusinessResults[i] = b
	}
	return &cp
}
