package recon_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lisama569/ledger-reconciliation-go/internal/ledger"
	"github.com/lisama569/ledger-reconciliation-go/internal/recon"
)

func setupSettlement(t *testing.T, reqs []ledger.TransferRequest) (*ledger.Service, *recon.SettlementReconciler) {
	t.Helper()
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	svc := ledger.NewService(st)
	if _, err := svc.OpenAccount(ctx, "A", "CNY", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenAccount(ctx, "B", "CNY", 10_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenAccount(ctx, "U", "USD", 10_000_000); err != nil {
		t.Fatal(err)
	}
	for _, req := range reqs {
		if _, err := svc.Transfer(ctx, req); err != nil {
			t.Fatalf("setup transfer %s: %v", req.BusinessID, err)
		}
	}
	return svc, recon.NewSettlementReconciler(recon.LedgerServiceView{Svc: svc})
}

func sReceipt(id, biz, ccy string, amt int64, at time.Time) recon.Receipt {
	return recon.Receipt{ReceiptID: id, BusinessID: biz, Currency: ccy, Amount: amt, SettledAt: at}
}

func findBiz(rep *recon.BatchReport, biz string) (recon.BusinessResult, bool) {
	for _, b := range rep.Business {
		if b.BusinessID == biz {
			return b, true
		}
	}
	return recon.BusinessResult{}, false
}

func mustFindBiz(t *testing.T, rep *recon.BatchReport, biz string) recon.BusinessResult {
	t.Helper()
	b, ok := findBiz(rep, biz)
	if !ok {
		t.Fatalf("business %s missing from batch report: %+v", biz, rep.Business)
	}
	return b
}

func logBatch(t *testing.T, rep *recon.BatchReport) {
	t.Helper()
	t.Logf("批次 %s 汇总: 结清=%d 部分结算=%d 超收=%d 币种不符=%d 未到账=%d | "+
		"回执总数=%d 新计入=%d 重复流水=%d 查无此单=%d 多余重复结算=%d (replayed=%v)",
		rep.BatchID,
		rep.Summary.Settled, rep.Summary.PartialSettled, rep.Summary.OverReceived,
		rep.Summary.CurrencyMismatch, rep.Summary.Unsettled,
		rep.Summary.ReceiptsTotal, rep.Summary.NewArrivals,
		rep.Summary.DuplicateReceipts, rep.Summary.UnmatchedReceipts,
		rep.Summary.RedundantSettlements, rep.Replayed)
	for _, b := range rep.Business {
		t.Logf("  业务单=%s 结论=%s 账本=%d%s 已累积=%d%s 本批新到账=%d",
			b.BusinessID, b.Status, b.LedgerAmount, b.LedgerCurrency,
			b.ReceivedAmount, b.LedgerCurrency, len(b.NewArrivals))
		for _, a := range b.NewArrivals {
			t.Logf("    累积规则: 计入流水 %s amount=%d%s -> 累积=%d%s",
				a.ReceiptID, a.Amount, a.Currency, b.ReceivedAmount, b.LedgerCurrency)
		}
		for _, is := range b.Issues {
			t.Logf("    回执问题: type=%s receipt=%s detail=%q", is.Type, is.ReceiptID, is.Detail)
		}
	}
	for _, is := range rep.OrphanIssues {
		t.Logf("  孤儿回执问题: type=%s receipt=%s business=%s detail=%q",
			is.Type, is.ReceiptID, is.BusinessID, is.Detail)
	}
}

func logBusiness(t *testing.T, r *recon.SettlementReconciler, biz string) recon.BusinessSettlement {
	t.Helper()
	bs, ok := r.Business(biz)
	if !ok {
		t.Fatalf("Business(%s) not found", biz)
	}
	t.Logf("查询业务单 %s: 当前结论=%s 账本=%d%s 累积=%d%s 超收部分=%d 结清批次=%s",
		bs.BusinessID, bs.Status, bs.LedgerAmount, bs.LedgerCurrency,
		bs.ReceivedAmount, bs.LedgerCurrency, bs.OverAmount(), bs.SettledBatchID)
	for _, a := range bs.Arrivals {
		t.Logf("  到账来源: 流水=%s 批次=%s amount=%d%s at=%s",
			a.ReceiptID, a.BatchID, a.Amount, a.Currency, a.SettledAt.Format(time.RFC3339))
	}
	for _, is := range bs.Issues {
		t.Logf("  历史问题: type=%s receipt=%s batch=%s detail=%q",
			is.Type, is.ReceiptID, is.BatchID, is.Detail)
	}
	return bs
}

// TestSettlementPartialAccumulatesToSettled 分次到账：部分结算 -> 补齐 -> 结清。
func TestSettlementPartialAccumulatesToSettled(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
		{BusinessID: "T2", FromID: "A", ToID: "B", Currency: "CNY", Amount: 500},
	})
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	t.Log("输入批次1: T1 先到 300，T2 未到账")
	rep1, err := r.ReconcileBatch(context.Background(), "B1", []recon.Receipt{
		sReceipt("R1", "T1", "CNY", 300, base),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)
	if got := mustFindBiz(t, rep1, "T1"); got.Status != recon.StatusPartialSettled || got.ReceivedAmount != 300 {
		t.Fatalf("T1 want partial/300, got %s/%d", got.Status, got.ReceivedAmount)
	}
	if rep1.Summary.PartialSettled != 1 || rep1.Summary.Unsettled != 1 {
		t.Fatalf("batch1 summary wrong: %+v", rep1.Summary)
	}

	t.Log("输入批次2: T1 再到 700（累积 1000 恰好结清）")
	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{
		sReceipt("R2", "T1", "CNY", 700, base.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	if got := mustFindBiz(t, rep2, "T1"); got.Status != recon.StatusSettled || got.ReceivedAmount != 1000 {
		t.Fatalf("T1 want settled/1000, got %s/%d", got.Status, got.ReceivedAmount)
	}
	// T2 依然未到账，但 T1 已结清后不再出现未到账/部分结论。
	if rep2.Summary.Settled != 1 || rep2.Summary.Unsettled != 1 {
		t.Fatalf("batch2 summary wrong: %+v", rep2.Summary)
	}

	bs := logBusiness(t, r, "T1")
	if bs.Status != recon.StatusSettled || len(bs.Arrivals) != 2 || bs.OverAmount() != 0 {
		t.Fatalf("T1 final state wrong: %+v", bs)
	}
}

// TestSettlementOutOfOrderAcrossBatches 乱序到账 + 跨批次累积，结论与输入顺序无关。
func TestSettlementOutOfOrderAcrossBatches(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 600},
	})
	base := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

	// 先到的回执结算时间反而更晚（乱序），且分散在两个批次。
	t.Log("输入批次1: 先到 400（结算时间 11:00）")
	rep1, err := r.ReconcileBatch(context.Background(), "B1", []recon.Receipt{
		sReceipt("R-late", "T1", "CNY", 400, base.Add(2*time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)

	t.Log("输入批次2: 后到 200（结算时间 09:00，乱序），累积恰好 600")
	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{
		sReceipt("R-early", "T1", "CNY", 200, base),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	if got := mustFindBiz(t, rep2, "T1"); got.Status != recon.StatusSettled || got.ReceivedAmount != 600 {
		t.Fatalf("T1 want settled/600 regardless of order, got %s/%d", got.Status, got.ReceivedAmount)
	}
	bs := logBusiness(t, r, "T1")
	if len(bs.Arrivals) != 2 || bs.SettledBatchID != "B2" {
		t.Fatalf("T1 arrivals/settled batch wrong: %+v", bs)
	}
}

// TestSettlementOverReceived 单笔超收与累积超收都必须识别为 over_received，
// 多出部分不计入正常结算，且后续再来回执也不能“凑”成结清。
func TestSettlementOverReceived(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
		{BusinessID: "T2", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
	})
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

	// T1：单笔直接超收 1200 > 1000。
	rep1, err := r.ReconcileBatch(context.Background(), "B1", []recon.Receipt{
		sReceipt("R1", "T1", "CNY", 1200, base),
		// T2：先部分 600，再补 500，累积 1100 超收。
		sReceipt("R2", "T2", "CNY", 600, base),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)
	if got := mustFindBiz(t, rep1, "T1"); got.Status != recon.StatusOverReceived {
		t.Fatalf("T1 want over_received, got %s", got.Status)
	}
	if got := mustFindBiz(t, rep1, "T2"); got.Status != recon.StatusPartialSettled || got.ReceivedAmount != 600 {
		t.Fatalf("T2 want partial/600, got %s/%d", got.Status, got.ReceivedAmount)
	}

	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{
		sReceipt("R3", "T2", "CNY", 500, base.Add(time.Hour)),
		// T1 超收后再来新流水，也不能翻成结清。
		sReceipt("R4", "T1", "CNY", 100, base.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	if got := mustFindBiz(t, rep2, "T2"); got.Status != recon.StatusOverReceived || got.OverAmount() != 100 {
		t.Fatalf("T2 want over_received with over 100, got %s over=%d", got.Status, got.OverAmount())
	}
	if got := mustFindBiz(t, rep2, "T1"); got.Status != recon.StatusOverReceived {
		t.Fatalf("T1 must stay over_received, got %s", got.Status)
	}

	bs1 := logBusiness(t, r, "T1")
	bs2 := logBusiness(t, r, "T2")
	if bs1.OverAmount() != 300 || bs2.OverAmount() != 100 {
		t.Fatalf("over amount wrong: T1=%d T2=%d", bs1.OverAmount(), bs2.OverAmount())
	}
}

// TestSettlementDuplicateSerialNotCounted 重复流水号（跨批次/本批次）不重复累积、
// 不重复产生差异；同批次重放返回首次留痕。
func TestSettlementDuplicateSerialNotCounted(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
	})
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	in := []recon.Receipt{
		sReceipt("R1", "T1", "CNY", 300, base),
		sReceipt("R1", "T1", "CNY", 300, base), // 本批次内重复
	}

	rep1, err := r.ReconcileBatch(context.Background(), "B1", in)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)
	if got := mustFindBiz(t, rep1, "T1"); got.Status != recon.StatusPartialSettled || got.ReceivedAmount != 300 {
		t.Fatalf("duplicate serial must not accumulate twice: got %s/%d", got.Status, got.ReceivedAmount)
	}
	if rep1.Summary.DuplicateReceipts != 1 {
		t.Fatalf("want 1 duplicate issue, got %d", rep1.Summary.DuplicateReceipts)
	}

	// 同批次重跑：返回首次留痕，不重复产生差异记录。
	rep1Replay, err := r.ReconcileBatch(context.Background(), "B1", in)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1Replay)
	if !rep1Replay.Replayed || rep1Replay.Summary.DuplicateReceipts != 1 {
		t.Fatalf("batch replay must reuse first trail: replayed=%v dup=%d",
			rep1Replay.Replayed, rep1Replay.Summary.DuplicateReceipts)
	}

	// 在新批次重放旧流水：只回放判重，累积金额不变。
	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{
		sReceipt("R1", "T1", "CNY", 300, base),
		sReceipt("R2", "T1", "CNY", 700, base.Add(time.Hour)), // 真正补齐 -> 结清
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	if got := mustFindBiz(t, rep2, "T1"); got.Status != recon.StatusSettled || got.ReceivedAmount != 1000 {
		t.Fatalf("T1 want settled/1000, got %s/%d", got.Status, got.ReceivedAmount)
	}
	if rep2.Summary.DuplicateReceipts != 1 || rep2.Summary.NewArrivals != 1 {
		t.Fatalf("B2: want 1 duplicate replay + 1 new arrival, got %+v", rep2.Summary)
	}

	// 批次留痕可按 ID 查询。
	stored, ok := r.Batch("B1")
	if !ok || stored.Replayed {
		t.Fatalf("stored batch B1 should be queryable and not marked replayed")
	}
	logBatch(t, stored)
}

// TestSettlementSingleConclusionNoContradiction 同一业务单在同一批里只能有一个结论：
// 币种不符的回执与部分到账并存时，只保留一个业务结论，且不再被当成未收到回执。
func TestSettlementSingleConclusionNoContradiction(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
		{BusinessID: "T2", FromID: "A", ToID: "B", Currency: "CNY", Amount: 800},
	})
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

	// 批次1：T1 同时收到币种不符和币种正确的部分到账；T2 只收到币种不符。
	rep1, err := r.ReconcileBatch(context.Background(), "B1", []recon.Receipt{
		sReceipt("R-bad", "T1", "USD", 1000, base),
		sReceipt("R-good", "T1", "CNY", 400, base),
		sReceipt("R-usd", "T2", "USD", 800, base),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)
	count := map[string]int{}
	for _, b := range rep1.Business {
		count[b.BusinessID]++
		if b.BusinessID == "T1" && b.Status != recon.StatusPartialSettled {
			t.Fatalf("T1 must have single conclusion partial_settled, got %s", b.Status)
		}
		if b.BusinessID == "T2" && b.Status != recon.StatusCurrencyMismatch {
			t.Fatalf("T2 must have single conclusion currency_mismatch, got %s", b.Status)
		}
	}
	if count["T1"] != 1 || count["T2"] != 1 {
		t.Fatalf("each business appears exactly once per batch: %v", count)
	}

	// 批次2：没有 T1/T2 的任何新回执 —— 已收到过回执的单子
	// 绝不能又被当成“没有收到回执（unsettled）”。
	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	for _, b := range rep2.Business {
		if b.BusinessID == "T1" || b.BusinessID == "T2" {
			t.Fatalf("%s already had receipts, must not be reported unsettled: %s",
				b.BusinessID, b.Status)
		}
	}

	// 给 T2 补上币种正确的到账 -> 结论从 currency_mismatch 演进为结清。
	rep3, err := r.ReconcileBatch(context.Background(), "B3", []recon.Receipt{
		sReceipt("R-fix1", "T2", "CNY", 300, base.Add(2*time.Hour)),
		sReceipt("R-fix2", "T2", "CNY", 500, base.Add(3*time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep3)
	if got := mustFindBiz(t, rep3, "T2"); got.Status != recon.StatusSettled {
		t.Fatalf("T2 should evolve to settled after correct arrivals, got %s", got.Status)
	}
}

// TestSettlementRedundantAfterSettled 结清之后再来一笔新流水号：
// 按多余的重复结算处理，不重新结清、不计入正常金额，业务结论保持 settled。
func TestSettlementRedundantAfterSettled(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
	})
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

	rep1, err := r.ReconcileBatch(context.Background(), "B1", []recon.Receipt{
		sReceipt("R1", "T1", "CNY", 400, base),
		sReceipt("R2", "T1", "CNY", 600, base.Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)
	if got := mustFindBiz(t, rep1, "T1"); got.Status != recon.StatusSettled {
		t.Fatalf("T1 should be settled, got %s", got.Status)
	}

	// 新批次里来了一个全新的流水号 R3，又打了一笔。
	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{
		sReceipt("R3", "T1", "CNY", 1000, base.Add(2*time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	got := mustFindBiz(t, rep2, "T1")
	if got.Status != recon.StatusSettled {
		t.Fatalf("already settled business must stay settled, got %s", got.Status)
	}
	if rep2.Summary.RedundantSettlements != 1 || rep2.Summary.Settled != 1 {
		t.Fatalf("want 1 redundant issue and still 1 settled, got %+v", rep2.Summary)
	}
	bs := logBusiness(t, r, "T1")
	if bs.ReceivedAmount != 1000 || len(bs.Arrivals) != 2 {
		t.Fatalf("redundant serial must not count into normal accumulation: %+v", bs)
	}
	if len(bs.Issues) != 1 || bs.Issues[0].Type != recon.IssueRedundantSettlement {
		t.Fatalf("want single redundant_settlement issue, got %+v", bs.Issues)
	}
}

// TestSettlementUnmatchedReceiptTracked 查无此单的回执及其重放都是孤儿问题，
// 不会挂到任何业务结论上。
func TestSettlementUnmatchedReceiptTracked(t *testing.T) {
	_, r := setupSettlement(t, []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
	})
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

	rep1, err := r.ReconcileBatch(context.Background(), "B1", []recon.Receipt{
		sReceipt("X1", "GHOST", "CNY", 100, base),
		sReceipt("R1", "T1", "CNY", 1000, base),
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep1)
	if rep1.Summary.UnmatchedReceipts != 1 {
		t.Fatalf("want 1 unmatched receipt, got %d", rep1.Summary.UnmatchedReceipts)
	}

	rep2, err := r.ReconcileBatch(context.Background(), "B2", []recon.Receipt{
		sReceipt("X1", "GHOST", "CNY", 100, base), // 重放旧孤儿回执
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, rep2)
	if rep2.Summary.UnmatchedReceipts != 0 || rep2.Summary.DuplicateReceipts != 1 {
		t.Fatalf("replayed orphan serial is a duplicate, not a fresh unmatched: %+v", rep2.Summary)
	}
	if len(rep2.OrphanIssues) != 1 {
		t.Fatalf("duplicate orphan stays orphan: %+v", rep2.OrphanIssues)
	}
}

// staticLedgerView 预构造的账本只读视图，避免账本建账开销干扰对账性能度量。
type staticLedgerView struct {
	txns []recon.LedgerTx
}

func (v staticLedgerView) ListTransactions(context.Context) ([]recon.LedgerTx, error) {
	return v.txns, nil
}

// TestSettlementScale 几万条回执量级：耗时与本批规模大致成正比，
// 历史数据越堆越多也不会让后续同规模批次明显变慢。
func TestSettlementScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	const n = 30_000
	txns := make([]recon.LedgerTx, n)
	for i := range txns {
		txns[i] = recon.LedgerTx{
			BusinessID: fmt.Sprintf("B%05d", i), Currency: "CNY", Amount: 100,
		}
	}
	r := recon.NewSettlementReconciler(staticLedgerView{txns: txns})
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

	// 批次1：3 万笔一次性结清。
	batch := make([]recon.Receipt, n)
	for i := range batch {
		batch[i] = sReceipt(fmt.Sprintf("S1-%05d", i), fmt.Sprintf("B%05d", i),
			"CNY", 100, base)
	}
	start1 := time.Now()
	rep1, err := r.ReconcileBatch(context.Background(), "BIG-1", batch)
	d1 := time.Since(start1)
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Summary.Settled != n {
		t.Fatalf("want %d settled, got %d", n, rep1.Summary.Settled)
	}
	t.Logf("批次 BIG-1: %d 张回执全部结清，耗时 %v", n, d1)

	// 批次2/3：同样 3 万张回执（全部是已结清业务单的“新流水号”），
	// 每张都只走判重+状态路径。若实现随历史变慢，这两批会明显慢于批次1。
	measure := func(batchID string) time.Duration {
		rs := make([]recon.Receipt, n)
		for i := range rs {
			rs[i] = sReceipt(fmt.Sprintf("%s-%05d", batchID, i), fmt.Sprintf("B%05d", i),
				"CNY", 100, base)
		}
		start := time.Now()
		rep, err := r.ReconcileBatch(context.Background(), batchID, rs)
		took := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Summary.RedundantSettlements != n || rep.Summary.Settled != n {
			t.Fatalf("%s summary wrong: %+v", batchID, rep.Summary)
		}
		t.Logf("批次 %s: %d 张回执耗时 %v", batchID, n, took)
		return took
	}
	d2 := measure("BIG-2")
	d3 := measure("BIG-3")

	t.Logf("3 万回执量级（含 map/排序）三批耗时 %v / %v / %v；规模相同，耗时应接近，不随历史翻倍", d1, d2, d3)
	// 同规模批次耗时不应因为历史累积而翻倍（宽松上界，避免机器抖动误杀）。
	if d3 > 2*d2+50*time.Millisecond {
		t.Fatalf("later batch much slower than earlier same-size batch: %v vs %v", d3, d2)
	}
}
