package recon

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// SettlementStatus 业务单在“分次到账累积”模型下的唯一结论。
type SettlementStatus string

const (
	// StatusUnsettled 账本有凭证、但从未收到任何回执（尚未到账）。
	StatusUnsettled SettlementStatus = "unsettled"
	// StatusPartialSettled 已收到币种正确的到账，但累积金额小于账本金额。
	StatusPartialSettled SettlementStatus = "partial_settled"
	// StatusSettled 累积到账金额恰好等于账本金额（结清，终态）。
	StatusSettled SettlementStatus = "settled"
	// StatusOverReceived 单笔或累积到账超过账本金额（超收，异常终态）。
	StatusOverReceived SettlementStatus = "over_received"
	// StatusCurrencyMismatch 只收到过币种不符的回执，尚无币种正确的到账。
	StatusCurrencyMismatch SettlementStatus = "currency_mismatch"
)

// ReceiptIssueType 单张回执级别的问题（一个业务单可同时挂多条回执问题，
// 但业务单本身只有一个 SettlementStatus 结论）。
type ReceiptIssueType string

const (
	// IssueDuplicateReceipt 外部流水号重复（本批次内或历史批次已处理过）。
	IssueDuplicateReceipt ReceiptIssueType = "duplicate_receipt"
	// IssueUnmatchedReceipt 回执引用的业务单在账本中查无凭证。
	IssueUnmatchedReceipt ReceiptIssueType = "unmatched_receipt"
	// IssueCurrencyMismatch 回执币种与账本凭证币种不一致。
	IssueCurrencyMismatch ReceiptIssueType = "currency_mismatch"
	// IssueRedundantSettlement 业务单已结清后又收到新的流水号（多余的重复结算）。
	IssueRedundantSettlement ReceiptIssueType = "redundant_settlement"
)

// SettlementArrival 一张被计入累积的到账记录。
type SettlementArrival struct {
	ReceiptID  string
	BusinessID string
	Currency   string
	Amount     int64
	BatchID    string
	SettledAt  time.Time
}

// ReceiptIssue 单张回执的问题留痕。
type ReceiptIssue struct {
	Type       ReceiptIssueType
	ReceiptID  string
	BusinessID string
	BatchID    string
	Detail     string
}

// BusinessSettlement 某业务单当前的累积结算结论（查询用快照）。
type BusinessSettlement struct {
	BusinessID     string
	LedgerCurrency string
	LedgerAmount   int64
	Status         SettlementStatus
	// ReceivedAmount 币种正确到账的累积总额（可能大于账本金额，即超收）。
	ReceivedAmount int64
	Arrivals       []SettlementArrival
	Issues         []ReceiptIssue
	SettledBatchID string
}

// OverAmount 超出账本金额的部分（未超收时为 0）；超出部分不计入正常结算。
func (s BusinessSettlement) OverAmount() int64 {
	if s.ReceivedAmount > s.LedgerAmount {
		return s.ReceivedAmount - s.LedgerAmount
	}
	return 0
}

// BusinessResult 单个批次内某个业务单的唯一结论（每单至多一条）。
type BusinessResult struct {
	BusinessID     string
	Status         SettlementStatus
	LedgerCurrency string
	LedgerAmount   int64
	ReceivedAmount int64
	// NewArrivals 本批新计入累积的到账。
	NewArrivals []SettlementArrival
	// Issues 本批该业务单上新发现的回执问题。
	Issues []ReceiptIssue
}

// OverAmount 超出账本金额的部分（未超收时为 0）；超出部分不计入正常结算。
func (r BusinessResult) OverAmount() int64 {
	if r.ReceivedAmount > r.LedgerAmount {
		return r.ReceivedAmount - r.LedgerAmount
	}
	return 0
}

// SettlementSummary 批次汇总计数。
type SettlementSummary struct {
	// 业务单结论计数（本批报告中每个业务单恰好落入其一）。
	Settled          int
	PartialSettled   int
	OverReceived     int
	CurrencyMismatch int
	Unsettled        int
	// 回执计数。
	ReceiptsTotal        int
	NewArrivals          int
	DuplicateReceipts    int
	UnmatchedReceipts    int
	RedundantSettlements int
}

// BatchReport 一批回执的对账留痕，可按 BatchID 查询；同批次重放返回同一留痕。
type BatchReport struct {
	BatchID string
	// Business 本批涉及的业务单结论，加上反向核对出的未到账单，每单一条。
	Business []BusinessResult
	// OrphanIssues 无法挂到任何账本业务单上的回执问题（如查无此单、其重复推送）。
	OrphanIssues []ReceiptIssue
	Summary      SettlementSummary
	// Replayed=true 表示本次调用是同一 BatchID 的重跑，返回的是首次留痕。
	Replayed bool
}

// SettlementReconciler 在单笔对账之上叠加“分次到账累积”的对账器：
// 同一业务单的多笔到账可跨批次、乱序累积，结论只向更完整方向演进。
type SettlementReconciler struct {
	mu sync.Mutex

	ledger LedgerView
	loaded bool

	// ledgerTxs 账本凭证索引，仅在首次对账 / SyncLedger 时构建（懒加载），
	// 之后每批不再全量扫描历史数据。
	ledgerTxs map[string]LedgerTx
	// pending 从未收到过任何回执的业务单；收到第一张回执后即移除，
	// 反向核对只需遍历它，耗时随“还没到账的单数”收缩，而不是历史总量。
	pending map[string]struct{}

	// seenReceipts 已处理过的外部流水号 -> 首次被处理时的简要信息（判重用）。
	seenReceipts map[string]seenReceipt
	// states 业务单 -> 当前累积状态（收到过回执才会出现）。
	states map[string]*bizState
	// batches 批次号 -> 首次对账留痕（同批次重跑直接回放，不重复产生差异）。
	batches map[string]*BatchReport
}

type seenReceipt struct {
	firstBatchID string
	// unmatched 首次处理时该流水号是否查无对应业务单。
	unmatched bool
}

// bizState 业务单跨批次累积状态（仅内部可变表示）。
type bizState struct {
	businessID     string
	ledgerCurrency string
	ledgerAmount   int64
	status         SettlementStatus
	receivedAmount int64
	arrivals       []SettlementArrival
	issues         []ReceiptIssue
	settledBatchID string
}

// NewSettlementReconciler 创建分次结算对账器。
func NewSettlementReconciler(ledger LedgerView) *SettlementReconciler {
	return &SettlementReconciler{
		ledger:       ledger,
		ledgerTxs:    map[string]LedgerTx{},
		pending:      map[string]struct{}{},
		seenReceipts: map[string]seenReceipt{},
		states:       map[string]*bizState{},
		batches:      map[string]*BatchReport{},
	}
}

// ReconcileBatch 对一个批次的回执做累积对账；同一 batchID 重跑返回首次留痕。
//
// 累积与判定规则：
//   - 以外部结算流水号（ReceiptID）判重：历史批次或本批次出现过的流水号
//     只回放，不再次累积；
//   - 币种正确的到账按时间顺序无关地累加（乱序、跨批次结果一致）：
//     累积 < 账本金额 => 部分结算；累积 == 账本金额 => 结清（终态）；
//     单笔或累积 > 账本金额 => 超收（异常终态），多出部分不计入正常结算；
//   - 币种不符、查无此单、结清后新流水号，分别记为回执级问题，
//     同一业务单在本批报告中只有一个业务结论，不会自相矛盾。
func (s *SettlementReconciler) ReconcileBatch(ctx context.Context, batchID string, receipts []Receipt) (*BatchReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}

	// 同批次重跑：回放首次留痕，不重复产生差异记录。
	if rep, ok := s.batches[batchID]; ok {
		cp := cloneBatchReport(rep)
		cp.Replayed = true
		return cp, nil
	}

	rep := &BatchReport{BatchID: batchID}
	touched := map[string]*bizState{}
	// touchedOrder 保持业务单首次出现顺序，报告排序前可追溯；报告最终排序。
	seenInBatch := map[string]struct{}{}
	orphans := []ReceiptIssue{}

	rep.Summary.ReceiptsTotal = len(receipts)

	for _, rc := range receipts {
		// 1) 流水号判重：先查历史批次，再查本批次。
		if first, ok := s.seenReceipts[rc.ReceiptID]; ok {
			issue := ReceiptIssue{
				Type:       IssueDuplicateReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				BatchID:    batchID,
				Detail: fmt.Sprintf("settlement serial already processed in batch %s",
					first.firstBatchID),
			}
			if first.unmatched {
				// 首次就是查无此单的回执，重放仍然挂不到任何账本业务单上。
				orphans = append(orphans, issue)
			} else if st, ok := s.states[rc.BusinessID]; ok {
				st.issues = append(st.issues, issue)
				touched[rc.BusinessID] = st
			} else {
				orphans = append(orphans, issue)
			}
			continue
		}
		if _, dup := seenInBatch[rc.ReceiptID]; dup {
			// 本批次内重复的第二张：记录在第一张所属的业务单上；
			// 若第一张查无此单，则作为孤儿问题。
			issue := ReceiptIssue{
				Type:       IssueDuplicateReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				BatchID:    batchID,
				Detail:     "duplicate settlement serial within the same batch",
			}
			if st, ok := s.states[rc.BusinessID]; ok {
				st.issues = append(st.issues, issue)
				touched[rc.BusinessID] = st
			} else {
				orphans = append(orphans, issue)
			}
			continue
		}
		seenInBatch[rc.ReceiptID] = struct{}{}

		// 2) 查无对应业务单。
		txn, ok := s.ledgerTxs[rc.BusinessID]
		if !ok {
			s.seenReceipts[rc.ReceiptID] = seenReceipt{firstBatchID: batchID, unmatched: true}
			orphans = append(orphans, ReceiptIssue{
				Type:       IssueUnmatchedReceipt,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				BatchID:    batchID,
				Detail:     "no ledger transaction for business id",
			})
			continue
		}

		st := s.getOrCreateStateLocked(txn)
		touched[rc.BusinessID] = st
		delete(s.pending, rc.BusinessID)

		// 3) 已结清的业务单再收到新流水号：多余的重复结算，
		//    不重新累积，业务结论保持 settled，不会又算一次结清。
		if st.status == StatusSettled {
			s.seenReceipts[rc.ReceiptID] = seenReceipt{firstBatchID: batchID}
			issue := ReceiptIssue{
				Type:       IssueRedundantSettlement,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				BatchID:    batchID,
				Detail: fmt.Sprintf("business already settled in batch %s; extra serial "+
					"amount %d %s is not counted again", st.settledBatchID, rc.Amount, rc.Currency),
			}
			st.issues = append(st.issues, issue)
			continue
		}

		// 4) 币种不符：该回执不计入累积，记为回执级问题；
		//    业务单结论随后按“是否已有币种正确的到账”统一裁决，不会矛盾。
		if txn.Currency != rc.Currency {
			s.seenReceipts[rc.ReceiptID] = seenReceipt{firstBatchID: batchID}
			st.issues = append(st.issues, ReceiptIssue{
				Type:       IssueCurrencyMismatch,
				ReceiptID:  rc.ReceiptID,
				BusinessID: rc.BusinessID,
				BatchID:    batchID,
				Detail: fmt.Sprintf("ledger currency %s != receipt currency %s",
					txn.Currency, rc.Currency),
			})
			if st.status == "" {
				st.status = StatusCurrencyMismatch
			}
			continue
		}

		// 5) 币种正确的新到账：计入累积（顺序无关）。
		s.seenReceipts[rc.ReceiptID] = seenReceipt{firstBatchID: batchID}
		arrival := SettlementArrival{
			ReceiptID:  rc.ReceiptID,
			BusinessID: rc.BusinessID,
			Currency:   rc.Currency,
			Amount:     rc.Amount,
			BatchID:    batchID,
			SettledAt:  rc.SettledAt,
		}
		st.arrivals = append(st.arrivals, arrival)
		st.receivedAmount += rc.Amount

		switch {
		case st.receivedAmount == st.ledgerAmount:
			st.status = StatusSettled
			st.settledBatchID = batchID
		case st.receivedAmount > st.ledgerAmount:
			// 超收：多出的部分只体现在 over_amount，不算结清。
			st.status = StatusOverReceived
		default:
			// 部分结算会覆盖“只见过币种不符回执”的临时结论，
			// 结论只向更完整的方向演进。
			st.status = StatusPartialSettled
		}
	}

	// 汇总 touched 业务单结论；每个业务单在本批恰好一条结论。
	touchedIDs := make([]string, 0, len(touched))
	for id := range touched {
		touchedIDs = append(touchedIDs, id)
	}
	sort.Strings(touchedIDs)
	for _, id := range touchedIDs {
		st := touched[id]
		res := BusinessResult{
			BusinessID:     st.businessID,
			Status:         st.status,
			LedgerCurrency: st.ledgerCurrency,
			LedgerAmount:   st.ledgerAmount,
			ReceivedAmount: st.receivedAmount,
		}
		for _, a := range st.arrivals {
			if a.BatchID == batchID {
				res.NewArrivals = append(res.NewArrivals, a)
			}
		}
		// 只带本批新发现的问题（历史问题可通过 Business 查询看到完整留痕）。
		for _, is := range st.issues {
			if is.BatchID == batchID {
				res.Issues = append(res.Issues, is)
			}
		}
		rep.Business = append(rep.Business, res)
		countBusinessStatus(&rep.Summary, res.Status)
		rep.Summary.NewArrivals += len(res.NewArrivals)
		countReceiptIssues(&rep.Summary, res.Issues)
	}

	// 反向核对：账本有凭证、但从未收到任何回执的业务单 -> unsettled。
	// 只遍历 pending（收到过任何回执即移出），不扫历史全量。
	pendingIDs := make([]string, 0, len(s.pending))
	for id := range s.pending {
		pendingIDs = append(pendingIDs, id)
	}
	sort.Strings(pendingIDs)
	for _, id := range pendingIDs {
		txn := s.ledgerTxs[id]
		rep.Business = append(rep.Business, BusinessResult{
			BusinessID:     id,
			Status:         StatusUnsettled,
			LedgerCurrency: txn.Currency,
			LedgerAmount:   txn.Amount,
		})
		rep.Summary.Unsettled++
	}

	rep.OrphanIssues = orphans
	countReceiptIssues(&rep.Summary, rep.OrphanIssues)

	s.batches[batchID] = cloneBatchReport(rep)
	return rep, nil
}

// SyncLedger 重新同步账本视图（默认首次对账时懒加载一次；
// 之后账本新增凭证需显式同步，避免每批全量扫描历史数据）。
func (s *SettlementReconciler) SyncLedger(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(ctx)
}

func (s *SettlementReconciler) ensureLoadedLocked(ctx context.Context) error {
	if s.loaded {
		return nil
	}
	return s.loadLocked(ctx)
}

// loadLocked 全量读取一次账本并建立索引；已存在的累积状态不受影响，
// 账本中新出现的凭证（业务单）加入 pending 等待回执。
func (s *SettlementReconciler) loadLocked(ctx context.Context) error {
	txns, err := s.ledger.ListTransactions(ctx)
	if err != nil {
		return err
	}
	for _, t := range txns {
		s.ledgerTxs[t.BusinessID] = t
		if _, seen := s.states[t.BusinessID]; !seen {
			if _, pending := s.pending[t.BusinessID]; !pending {
				s.pending[t.BusinessID] = struct{}{}
			}
		}
	}
	s.loaded = true
	return nil
}

func (s *SettlementReconciler) getOrCreateStateLocked(txn LedgerTx) *bizState {
	if st, ok := s.states[txn.BusinessID]; ok {
		return st
	}
	st := &bizState{
		businessID:     txn.BusinessID,
		ledgerCurrency: txn.Currency,
		ledgerAmount:   txn.Amount,
	}
	s.states[txn.BusinessID] = st
	return st
}

func countBusinessStatus(sm *SettlementSummary, status SettlementStatus) {
	switch status {
	case StatusSettled:
		sm.Settled++
	case StatusPartialSettled:
		sm.PartialSettled++
	case StatusOverReceived:
		sm.OverReceived++
	case StatusCurrencyMismatch:
		sm.CurrencyMismatch++
	case StatusUnsettled:
		sm.Unsettled++
	}
}

func countReceiptIssues(sm *SettlementSummary, issues []ReceiptIssue) {
	for _, is := range issues {
		switch is.Type {
		case IssueDuplicateReceipt:
			sm.DuplicateReceipts++
		case IssueUnmatchedReceipt:
			sm.UnmatchedReceipts++
		case IssueRedundantSettlement:
			sm.RedundantSettlements++
		}
	}
}

func cloneBatchReport(in *BatchReport) *BatchReport {
	if in == nil {
		return nil
	}
	out := *in
	out.Business = append([]BusinessResult(nil), in.Business...)
	for i := range out.Business {
		out.Business[i].NewArrivals = append([]SettlementArrival(nil), in.Business[i].NewArrivals...)
		out.Business[i].Issues = append([]ReceiptIssue(nil), in.Business[i].Issues...)
	}
	out.OrphanIssues = append([]ReceiptIssue(nil), in.OrphanIssues...)
	return &out
}

// Business 查询业务单当前结论与金额累积过程。
func (s *SettlementReconciler) Business(businessID string) (BusinessSettlement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.states[businessID]
	if !ok {
		// 可能只是还没收到回执：账本里有凭证时返回 unsettled 快照。
		if txn, inLedger := s.ledgerTxs[businessID]; inLedger {
			return BusinessSettlement{
				BusinessID:     businessID,
				LedgerCurrency: txn.Currency,
				LedgerAmount:   txn.Amount,
				Status:         StatusUnsettled,
				Arrivals:       []SettlementArrival{},
				Issues:         []ReceiptIssue{},
			}, true
		}
		return BusinessSettlement{}, false
	}
	return BusinessSettlement{
		BusinessID:     st.businessID,
		LedgerCurrency: st.ledgerCurrency,
		LedgerAmount:   st.ledgerAmount,
		Status:         st.status,
		ReceivedAmount: st.receivedAmount,
		Arrivals:       append([]SettlementArrival(nil), st.arrivals...),
		Issues:         append([]ReceiptIssue(nil), st.issues...),
		SettledBatchID: st.settledBatchID,
	}, true
}

// Batch 按批次号查询历史留痕。
func (s *SettlementReconciler) Batch(batchID string) (*BatchReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep, ok := s.batches[batchID]
	if !ok {
		return nil, false
	}
	return cloneBatchReport(rep), true
}
