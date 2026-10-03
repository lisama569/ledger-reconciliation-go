package recon_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lisama569/ledger-reconciliation-go/internal/recon"
)

// logBatch 打印一批的输入规则、回执级差异与每个业务单的唯一最终结论。
func logBatch(t *testing.T, tag, batchID string, receipts []recon.Receipt, rep *recon.Report) {
	t.Helper()
	t.Logf("[%s] batch=%s 输入回执 %d 张:", tag, batchID, len(receipts))
	for _, rc := range receipts {
		t.Logf("[%s]   输入 receipt=%s business=%s ccy=%s amount=%d",
			tag, rc.ReceiptID, rc.BusinessID, rc.Currency, rc.Amount)
	}
	t.Logf("[%s] 规则: 同业务单按流水号逐笔累积, ==账本结清 / <账本部分结算 / "+
		">账本超收(多出部分不计); 已结清再来新流水=多余重复结算; ReceiptID 重放=duplicate 不改累积", tag)
	t.Logf("[%s] 计入正常结算的回执=%v", tag, rep.Matched)
	for _, d := range rep.Discrepancies {
		t.Logf("[%s] 回执级差异 type=%s receipt=%s business=%s detail=%q",
			tag, d.Type, d.ReceiptID, d.BusinessID, d.Detail)
	}
	s := rep.Summary
	t.Logf("[%s] 汇总: 业务单数=%d 结清=%d 部分结算=%d 超收=%d 币种不符=%d 无回执=%d",
		tag, s.Total, s.Settled, s.Partial, s.OverReceived, s.CurrencyMismatch, s.UnmatchedEntry)
	for _, b := range rep.BusinessResults {
		t.Logf("[%s] 业务单结论 business=%s status=%s 累计=%d/%d 流水明细=%v 超额=%d 多余流水=%v detail=%q",
			tag, b.BusinessID, b.Status, b.SettledAmount, b.LedgerAmount,
			b.Receipts, b.OverflowAmount, b.ExtraReceiptIDs, b.Detail)
	}
}

// statusOf 从批次报告里取某业务单结论。
func statusOf(rep *recon.Report, biz string) recon.BusinessStatus {
	b, ok := rep.Business(biz)
	if !ok {
		return "MISSING"
	}
	return b.Status
}

// TestAccumulatePartialToSettledCrossBatch 分次到账：部分结算 -> 跨批次补齐结清。
func TestAccumulatePartialToSettledCrossBatch(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	rep1, err := r.Reconcile(ctx, []recon.Receipt{
		rc("R1a", "T1", "CNY", 400), // T1 账本 1000，首笔 400 -> 部分结算
	})
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "partial-batch1", rep1.BatchID, []recon.Receipt{rc("R1a", "T1", "CNY", 400)}, rep1)

	if got := statusOf(rep1, "T1"); got != recon.StatusPartial {
		t.Fatalf("T1 want partial, got %s", got)
	}
	if rep1.Summary.Partial != 1 || rep1.Summary.UnmatchedEntry != 2 {
		t.Fatalf("batch1 summary wrong: %+v", rep1.Summary)
	}
	// 部分结算不是“没有回执”：T1 本批不得出现 unmatched_entry
	for _, d := range rep1.Discrepancies {
		if d.Type == recon.UnmatchedEntry && d.BusinessID == "T1" {
			t.Fatal("partial-settled T1 must not be reported as unmatched_entry")
		}
	}

	cur, ok := r.Business(ctx, "T1")
	if !ok || cur.SettledAmount != 400 || len(cur.Receipts) != 1 {
		t.Fatalf("current T1 state wrong: %+v ok=%v", cur, ok)
	}

	// 第二批补齐 600（乱序语义下这就是后到的尾款）-> 恰好结清
	rep2Input := []recon.Receipt{rc("R1b", "T1", "CNY", 600)}
	rep2, err := r.Reconcile(ctx, rep2Input)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "settled-batch2", rep2.BatchID, rep2Input, rep2)

	if got := statusOf(rep2, "T1"); got != recon.StatusSettled {
		t.Fatalf("T1 want settled, got %s", got)
	}
	if len(rep2.Matched) != 1 || rep2.Matched[0] != "R1b" {
		t.Fatalf("R1b should be counted, got %v", rep2.Matched)
	}
	cur, _ = r.Business(ctx, "T1")
	if cur.SettledAmount != 1000 || cur.OverflowAmount != 0 {
		t.Fatalf("T1 accumulated wrong: %+v", cur)
	}
	if len(cur.Receipts) != 2 || cur.Receipts[0].Amount != 400 || cur.Receipts[1].Amount != 600 {
		t.Fatalf("T1 receipt trail wrong: %+v", cur.Receipts)
	}
}

// TestAccumulateOutOfOrder 乱序（批内打乱）与跨批次分次都能正确累积到结清。
func TestAccumulateOutOfOrder(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	// T2 账本 2500 = 1000 + 500 + 1000，批内故意打乱到达顺序。
	batchInput := []recon.Receipt{
		rc("R2c", "T2", "CNY", 1000),
		rc("R2a", "T2", "CNY", 500),
		rc("R2b", "T2", "CNY", 1000),
	}
	rep1, err := r.Reconcile(ctx, batchInput)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "out-of-order-in-batch", rep1.BatchID, batchInput, rep1)
	if got := statusOf(rep1, "T2"); got != recon.StatusSettled {
		t.Fatalf("T2 want settled after shuffled installments, got %s", got)
	}
	b, _ := rep1.Business("T2")
	if b.SettledAmount != 2500 || len(b.Receipts) != 3 {
		t.Fatalf("T2 accumulation wrong: %+v", b)
	}
	// 流水明细按实际到达顺序留痕
	if b.Receipts[0].ReceiptID != "R2c" || b.Receipts[1].ReceiptID != "R2a" ||
		b.Receipts[2].ReceiptID != "R2b" {
		t.Fatalf("receipt trail must follow arrival order: %+v", b.Receipts)
	}

	// 另一个业务单跨两批分次到账：300 -> 400（T3 账本 700）。
	cross := []recon.Receipt{
		rc("R3a", "T3", "CNY", 300),
	}
	rep2, err := r.Reconcile(ctx, cross)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "cross-batch-partial", rep2.BatchID, cross, rep2)
	if got := statusOf(rep2, "T3"); got != recon.StatusPartial {
		t.Fatalf("T3 want partial 300/700, got %s", got)
	}
	fin := []recon.Receipt{rc("R3b", "T3", "CNY", 400)}
	rep3, err := r.Reconcile(ctx, fin)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "cross-batch-settled", rep3.BatchID, fin, rep3)
	if got := statusOf(rep3, "T3"); got != recon.StatusSettled {
		t.Fatalf("T3 want settled 700/700, got %s", got)
	}
}

// TestOverReceivedSplitAndSticky 分次累积超过账本金额：超收单列、多出不计、结论粘性。
func TestOverReceivedSplitAndSticky(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	in1 := []recon.Receipt{
		rc("R1a", "T1", "CNY", 400),
		rc("R1b", "T1", "CNY", 700), // 400+700=1100 > 1000，本笔只计 600，超额 100
	}
	rep1, err := r.Reconcile(ctx, in1)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "split-overflow", rep1.BatchID, in1, rep1)

	cnt := countTypes(rep1)
	if cnt[recon.OverReceived] != 1 {
		t.Fatalf("want 1 over_received, got %d", cnt[recon.OverReceived])
	}
	if got := statusOf(rep1, "T1"); got != recon.StatusOverReceived {
		t.Fatalf("T1 want over_received, got %s", got)
	}
	b, _ := rep1.Business("T1")
	if b.SettledAmount != 1000 || b.OverflowAmount != 100 {
		t.Fatalf("T1 overflow accounting wrong: %+v", b)
	}
	if len(b.Receipts) != 2 || b.Receipts[1].Amount != 600 {
		t.Fatalf("only remaining room 600 may be counted: %+v", b.Receipts)
	}
	if len(rep1.Matched) != 1 || rep1.Matched[0] != "R1a" {
		t.Fatalf("only R1a counted normally, got %v", rep1.Matched)
	}

	// 超收之后再来一笔：依旧超收，不回退、不二次结清
	in2 := []recon.Receipt{rc("R1c", "T1", "CNY", 50)}
	rep2, err := r.Reconcile(ctx, in2)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "overflow-sticky", rep2.BatchID, in2, rep2)
	if countTypes(rep2)[recon.OverReceived] != 1 {
		t.Fatalf("R1c must be over_received again")
	}
	cur, _ := r.Business(ctx, "T1")
	if cur.Status != recon.StatusOverReceived || cur.SettledAmount != 1000 || cur.OverflowAmount != 150 {
		t.Fatalf("sticky over_received state wrong: %+v", cur)
	}
	if len(rep2.Matched) != 0 {
		t.Fatalf("over-received flow must not be counted as normal: %v", rep2.Matched)
	}

	// 首张有效回执就超账本金额：回执级保留 amount_mismatch，业务级是超收
	in3 := []recon.Receipt{rc("R2x", "T2", "CNY", 2600)} // T2 账本 2500
	rep3, err := r.Reconcile(ctx, in3)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "single-overshoot-compat", rep3.BatchID, in3, rep3)
	if countTypes(rep3)[recon.AmountMismatch] != 1 {
		t.Fatal("single overshooting receipt keeps amount_mismatch disposition")
	}
	if got := statusOf(rep3, "T2"); got != recon.StatusOverReceived {
		t.Fatalf("single overshoot business status must be over_received, got %s", got)
	}
	b2, _ := rep3.Business("T2")
	if b2.SettledAmount != 2500 || b2.OverflowAmount != 100 {
		t.Fatalf("single overshoot accounting wrong: %+v", b2)
	}
}

// TestSettledThenExtraSerial 结清后再收到新流水号：按多余重复结算，不二次结清。
func TestSettledThenExtraSerial(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	in1 := []recon.Receipt{rc("R3", "T3", "CNY", 700)}
	rep1, err := r.Reconcile(ctx, in1)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "settled-first", rep1.BatchID, in1, rep1)
	if got := statusOf(rep1, "T3"); got != recon.StatusSettled {
		t.Fatalf("T3 want settled, got %s", got)
	}

	in2 := []recon.Receipt{rc("R3-extra", "T3", "CNY", 100)}
	rep2, err := r.Reconcile(ctx, in2)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "extra-after-settled", rep2.BatchID, in2, rep2)
	if countTypes(rep2)[recon.OverReceived] != 1 {
		t.Fatal("extra serial after settled must be over_received")
	}
	if len(rep2.Matched) != 0 {
		t.Fatalf("extra serial must not count as a second settlement: %v", rep2.Matched)
	}
	cur, _ := r.Business(ctx, "T3")
	if cur.SettledAmount != 700 || cur.OverflowAmount != 100 || len(cur.Receipts) != 1 {
		t.Fatalf("principal stays settled, overflow tracked separately: %+v", cur)
	}
	if len(cur.ExtraReceiptIDs) != 1 || cur.ExtraReceiptIDs[0] != "R3-extra" {
		t.Fatalf("extra serial not recorded: %+v", cur.ExtraReceiptIDs)
	}
}

// TestDuplicatePushNotCounted 重复推送（批内/跨批重放）不重复累积、不重复产生差异。
func TestDuplicatePushNotCounted(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	in1 := []recon.Receipt{
		rc("R1a", "T1", "CNY", 400),
		rc("R1a", "T1", "CNY", 400), // 同批次重复流水号
	}
	rep1, err := r.Reconcile(ctx, in1)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "dup-in-batch", rep1.BatchID, in1, rep1)
	if countTypes(rep1)[recon.DuplicateReceipt] != 1 {
		t.Fatal("want exactly 1 in-batch duplicate")
	}
	cur, _ := r.Business(ctx, "T1")
	if cur.SettledAmount != 400 || len(cur.Receipts) != 1 {
		t.Fatalf("duplicate must not accumulate twice: %+v", cur)
	}

	in2 := []recon.Receipt{rc("R1a", "T1", "CNY", 400)} // 跨批次重放旧回执
	rep2, err := r.Reconcile(ctx, in2)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "dup-replay-batch", rep2.BatchID, in2, rep2)
	if countTypes(rep2)[recon.DuplicateReceipt] != 1 {
		t.Fatal("replay must produce one duplicate replay only")
	}
	cur, _ = r.Business(ctx, "T1")
	if cur.SettledAmount != 400 || len(cur.Receipts) != 1 {
		t.Fatalf("replayed receipt must not change accumulation: %+v", cur)
	}
	// 已收到过回执的业务单不得再被当成没有回执
	for _, d := range rep2.Discrepancies {
		if d.Type == recon.UnmatchedEntry && d.BusinessID == "T1" {
			t.Fatal("T1 must never be unmatched_entry after receiving a receipt")
		}
	}
}

// TestNoContradictoryConclusion 同一业务单同一批里只能有一个结论：
// 被判异常的业务单不得同时被判“没有回执”，且业务单结论唯一。
func TestNoContradictoryConclusion(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	in1 := []recon.Receipt{
		rc("R1bad", "T1", "USD", 1000), // 币种不符
		rc("R2bad", "T2", "CNY", 9999), // 首张即超额 -> amount_mismatch
	}
	rep1, err := r.Reconcile(ctx, in1)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "no-contradiction-b1", rep1.BatchID, in1, rep1)

	seen := map[string]int{}
	for _, b := range rep1.BusinessResults {
		seen[b.BusinessID]++
	}
	for biz, n := range seen {
		if n != 1 {
			t.Fatalf("business %s has %d conclusions in one batch", biz, n)
		}
	}
	unmatched := map[string]bool{}
	for _, d := range rep1.Discrepancies {
		if d.Type == recon.UnmatchedEntry {
			unmatched[d.BusinessID] = true
		}
	}
	for _, biz := range []string{"T1", "T2"} {
		if unmatched[biz] {
			t.Fatalf("%s has discrepancy but also reported unmatched_entry", biz)
		}
	}
	if statusOf(rep1, "T1") != recon.StatusCurrencyMismatch {
		t.Fatal("T1 must be currency_mismatch")
	}
	if statusOf(rep1, "T2") != recon.StatusOverReceived {
		t.Fatal("T2 must be over_received")
	}

	// 后续空批次（整批丢失重跑）也不能把 T1/T2 再当成没有回执
	rep2, err := r.Reconcile(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "no-contradiction-b2-empty", rep2.BatchID, nil, rep2)
	for _, d := range rep2.Discrepancies {
		if d.Type == recon.UnmatchedEntry && (d.BusinessID == "T1" || d.BusinessID == "T2") {
			t.Fatalf("%s must stay known after discrepant receipt", d.BusinessID)
		}
	}
}

// TestBatchTraceAndRerunIdempotent 批次留痕、按批次/业务单查询，同批重跑不重复产生差异。
func TestBatchTraceAndRerunIdempotent(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	in1 := []recon.Receipt{rc("R1a", "T1", "CNY", 400)}
	rep1, err := r.ReconcileBatch(ctx, "day1", in1)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "trace-day1", "day1", in1, rep1)
	if rep1.Summary.Partial != 1 || rep1.Summary.UnmatchedEntry != 2 {
		t.Fatalf("day1 summary: %+v", rep1.Summary)
	}

	in2 := []recon.Receipt{rc("R1b", "T1", "CNY", 600)}
	rep2, err := r.ReconcileBatch(ctx, "day2", in2)
	if err != nil {
		t.Fatal(err)
	}
	logBatch(t, "trace-day2", "day2", in2, rep2)
	if rep2.Summary.Settled != 1 {
		t.Fatalf("day2 summary: %+v", rep2.Summary)
	}

	// 同批次号重跑：哪怕携带的回执内容不同，也直接回放首次报告
	rerun, err := r.ReconcileBatch(ctx, "day1", []recon.Receipt{
		rc("BOGUS", "T2", "CNY", 2500),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rerun.BatchID != "day1" || len(rerun.Matched) != len(rep1.Matched) ||
		len(rerun.Discrepancies) != len(rep1.Discrepancies) ||
		len(rerun.BusinessResults) != len(rep1.BusinessResults) {
		t.Fatal("rerun of same batch id must return the stored first report")
	}
	t.Logf("[trace-rerun] day1 重跑直接回放留痕: matched=%v discrepancies=%d",
		rerun.Matched, len(rerun.Discrepancies))

	// 查询接口
	if got, ok := r.Batch("day1"); !ok || got.BatchID != "day1" {
		t.Fatal("Batch(day1) trace missing")
	}
	if _, ok := r.Batch("nope"); ok {
		t.Fatal("unknown batch should not be found")
	}
	batches := r.ListBatches()
	if len(batches) != 2 || batches[0].BatchID != "day1" || batches[1].BatchID != "day2" {
		t.Fatalf("batch trace order wrong: %+v", batches)
	}
	cur, ok := r.Business(ctx, "T1")
	if !ok || cur.Status != recon.StatusSettled || cur.SettledAmount != 1000 {
		t.Fatalf("business query current state wrong: %+v", cur)
	}
	t.Logf("[trace-query] Business(T1)=%s 累计=%d/%d 流水=%v; ListBatches=%d 批",
		cur.Status, cur.SettledAmount, cur.LedgerAmount, cur.Receipts, len(batches))

	// 重跑没有污染累积：BOGUS 未被处理，T2 仍未收到回执
	curT2, _ := r.Business(ctx, "T2")
	if curT2.Status != "" {
		t.Fatalf("rerun must not apply receipts, T2 leaked: %+v", curT2)
	}
}

// fakeView 大规模性能测试用的账本视图（避免走账本快照拷贝）。
type fakeView struct {
	txns []recon.LedgerTx
}

func (f fakeView) ListTransactions(context.Context) ([]recon.LedgerTx, error) {
	return f.txns, nil
}

func makeView(n int64) fakeView {
	txns := make([]recon.LedgerTx, n)
	for i := range txns {
		txns[i] = recon.LedgerTx{
			BusinessID: fmt.Sprintf("B%d", i),
			Currency:   "CNY",
			Amount:     100,
		}
	}
	return fakeView{txns: txns}
}

func receiptsFor(view fakeView, start, count int64) []recon.Receipt {
	out := make([]recon.Receipt, count)
	now := time.Now()
	for i := range out {
		id := start + int64(i)
		out[i] = recon.Receipt{
			ReceiptID:  fmt.Sprintf("S%d", id),
			BusinessID: fmt.Sprintf("B%d", id),
			Currency:   "CNY",
			Amount:     100,
			SettledAt:  now,
		}
	}
	return out
}

// TestScaleLinearWithBatch 几万条规模：单次耗时随本批规模增长，
// 不因历史已处理回执堆积而明显变慢。
func TestScaleLinearWithBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	ctx := context.Background()
	const ledgerN int64 = 40000

	// 1) 冷对账器跑 10000 张
	cold := recon.New(makeView(ledgerN))
	coldIn := receiptsFor(makeView(ledgerN), 0, 10000)
	t0 := time.Now()
	if _, err := cold.Reconcile(ctx, coldIn); err != nil {
		t.Fatal(err)
	}
	cold10k := time.Since(t0)

	// 2) 预热 30000 张历史回执后，再跑 10000 张新回执
	warm := recon.New(makeView(ledgerN))
	if _, err := warm.Reconcile(ctx, receiptsFor(makeView(ledgerN), 0, 30000)); err != nil {
		t.Fatal(err)
	}
	t0 = time.Now()
	if _, err := warm.Reconcile(ctx, receiptsFor(makeView(ledgerN), 30000, 10000)); err != nil {
		t.Fatal(err)
	}
	warm10k := time.Since(t0)

	t.Logf("[scale] 账本凭证=%d; 冷跑1万=%v; 堆积3万历史后跑1万=%v",
		ledgerN, cold10k, warm10k)
	if warm10k > cold10k*3+50*time.Millisecond {
		t.Fatalf("history-dependent slowdown: cold=%v warm=%v", cold10k, warm10k)
	}

	// 3) 批规模翻倍，耗时大致成正比（容差宽松，机器抖动不致误报）
	small := recon.New(makeView(ledgerN))
	t0 = time.Now()
	if _, err := small.Reconcile(ctx, receiptsFor(makeView(ledgerN), 0, 5000)); err != nil {
		t.Fatal(err)
	}
	cost5k := time.Since(t0)
	t.Logf("[scale] 同账本跑5千=%v (5千->1万 约 %.2fx)", cost5k,
		float64(cold10k)/float64(cost5k))
	if cold10k > cost5k*4+50*time.Millisecond {
		t.Fatalf("cost not roughly linear: 5k=%v 10k=%v", cost5k, cold10k)
	}
}

// TestConcurrentAccumulation race 下并发分批/重放同一业务单：最终恰好结清且不重复累计。
func TestConcurrentAccumulation(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	ctx := context.Background()

	var wg sync.WaitGroup
	push := func(receipts ...recon.Receipt) {
		defer wg.Done()
		if _, err := r.Reconcile(ctx, receipts); err != nil {
			t.Errorf("concurrent reconcile: %v", err)
		}
	}
	// T1=1000：并发推 300 和 700（最终恰好结清），并夹杂重复重放
	wg.Add(5)
	go push(rc("R1a", "T1", "CNY", 300))
	go push(rc("R1b", "T1", "CNY", 700))
	go push(rc("R1a", "T1", "CNY", 300))
	go push(rc("R1b", "T1", "CNY", 700))
	go push(rc("R1a", "T1", "CNY", 300))
	wg.Wait()

	cur, ok := r.Business(ctx, "T1")
	if !ok {
		t.Fatal("T1 missing")
	}
	t.Logf("[concurrent] T1 最终 status=%s 累计=%d 流水=%v 超额=%d",
		cur.Status, cur.SettledAmount, cur.Receipts, cur.OverflowAmount)
	if cur.Status != recon.StatusSettled || cur.SettledAmount != 1000 || cur.OverflowAmount != 0 {
		t.Fatalf("concurrent accumulation must end exactly settled: %+v", cur)
	}
	if len(cur.Receipts) != 2 {
		t.Fatalf("each serial counted once, want 2 receipts got %v", cur.Receipts)
	}
}
