package recon_test

import (
	"context"
	"testing"
	"time"

	"github.com/lisama569/ledger-reconciliation-go/internal/ledger"
	"github.com/lisama569/ledger-reconciliation-go/internal/recon"
)

func setupLedger(t *testing.T, reqs []ledger.TransferRequest) (*ledger.Service, *recon.Reconciler) {
	t.Helper()
	ctx := context.Background()
	st := ledger.NewMemoryStore()
	svc := ledger.NewService(st)
	if _, err := svc.OpenAccount(ctx, "A", "CNY", 1_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenAccount(ctx, "B", "CNY", 1_000_000); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenAccount(ctx, "U", "USD", 1_000_000); err != nil {
		t.Fatal(err)
	}
	for _, req := range reqs {
		if _, err := svc.Transfer(ctx, req); err != nil {
			t.Fatalf("setup transfer %s: %v", req.BusinessID, err)
		}
	}
	r := recon.New(recon.LedgerServiceView{Svc: svc})
	return svc, r
}

func baseReqs() []ledger.TransferRequest {
	return []ledger.TransferRequest{
		{BusinessID: "T1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000},
		{BusinessID: "T2", FromID: "A", ToID: "B", Currency: "CNY", Amount: 2500},
		{BusinessID: "T3", FromID: "A", ToID: "B", Currency: "CNY", Amount: 700},
	}
}

func rc(id, biz, ccy string, amt int64) recon.Receipt {
	return recon.Receipt{ReceiptID: id, BusinessID: biz, Currency: ccy, Amount: amt, SettledAt: time.Now()}
}

func countTypes(rep *recon.Report) map[recon.DiscrepancyType]int {
	m := map[recon.DiscrepancyType]int{}
	for _, d := range rep.Discrepancies {
		m[d.Type]++
	}
	return m
}

func logReport(t *testing.T, tag string, rep *recon.Report) {
	t.Helper()
	t.Logf("[%s] 匹配成功回执=%v", tag, rep.Matched)
	for _, d := range rep.Discrepancies {
		t.Logf("[%s] 对账差异 type=%s receipt=%s business=%s detail=%q",
			tag, d.Type, d.ReceiptID, d.BusinessID, d.Detail)
	}
}

// TestReconcileAllMatched 全部匹配，无差异。
func TestReconcileAllMatched(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	rep, err := r.Reconcile(context.Background(), []recon.Receipt{
		rc("R1", "T1", "CNY", 1000),
		rc("R2", "T2", "CNY", 2500),
		rc("R3", "T3", "CNY", 700),
	})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, "all-matched", rep)
	if rep.HasDiff() {
		t.Fatalf("unexpected discrepancies: %+v", rep.Discrepancies)
	}
	if len(rep.Matched) != 3 {
		t.Fatalf("want 3 matched, got %d", len(rep.Matched))
	}
}

// TestReconcileDiscrepancyKinds 四类回执异常必须被区分标记。
func TestReconcileDiscrepancyKinds(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	rep, err := r.Reconcile(context.Background(), []recon.Receipt{
		rc("R1", "T1", "CNY", 1000),   // OK
		rc("R2", "T2", "CNY", 9999),   // 金额不符
		rc("R3", "T3", "USD", 700),    // 币种不符
		rc("R4", "GHOST", "CNY", 100), // 未匹配
		rc("R1", "T1", "CNY", 1000),   // 重复回执（同批次）
	})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, "discrepancies", rep)
	cnt := countTypes(rep)
	want := map[recon.DiscrepancyType]int{
		recon.AmountMismatch:   1,
		recon.CurrencyMismatch: 1,
		recon.UnmatchedReceipt: 1,
		recon.DuplicateReceipt: 1,
	}
	for k, v := range want {
		if cnt[k] != v {
			t.Fatalf("discrepancy %s: want %d, got %d", k, v, cnt[k])
		}
	}
	if len(rep.Matched) != 1 || rep.Matched[0] != "R1" {
		t.Fatalf("only R1 should match, got %v", rep.Matched)
	}
}

// TestReconcileRetryIdempotent 部分失败后重试：已处理回执回放首次结论，
// 账本不重复记、差异不重复产生（只回放一条 duplicate 记录）。
func TestReconcileRetryIdempotent(t *testing.T) {
	svc, r := setupLedger(t, baseReqs())
	before, _ := svc.TotalBalance(context.Background(), "CNY")

	// 第一批：一张正确 + 一张金额不符
	rep1, err := r.Reconcile(context.Background(), []recon.Receipt{
		rc("R1", "T1", "CNY", 1000),
		rc("R2", "T2", "CNY", 9999),
	})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, "batch-1", rep1)

	// 第二批（重试重放全部 + 补上剩余正确回执）
	rep2, err := r.Reconcile(context.Background(), []recon.Receipt{
		rc("R1", "T1", "CNY", 1000), // 已处理 -> duplicate，且回放 match_ok 结论
		rc("R2", "T2", "CNY", 9999), // 已处理 -> duplicate，回放 amount_mismatch 结论
		rc("R3", "T3", "CNY", 700),  // 新回执 -> match
	})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, "batch-2-retry", rep2)
	cnt := countTypes(rep2)
	if cnt[recon.DuplicateReceipt] != 2 {
		t.Fatalf("want 2 duplicate replays, got %d", cnt[recon.DuplicateReceipt])
	}
	if len(rep2.Matched) != 1 || rep2.Matched[0] != "R3" {
		t.Fatalf("only new R3 should match, got %v", rep2.Matched)
	}
	// 差异细节必须保留首次结论
	for _, d := range rep2.Discrepancies {
		if d.Type == recon.DuplicateReceipt && d.ReceiptID == "R2" {
			if d.Detail == "" {
				t.Fatal("duplicate replay should carry first conclusion detail")
			}
		}
	}
	after, _ := svc.TotalBalance(context.Background(), "CNY")
	t.Logf("幂等判定依据: processed[receiptID] 回放首次结论; 重试前后账本总额 %d / %d", before, after)
	if before != after {
		t.Fatalf("对账重试不得改变账本: %d -> %d", before, after)
	}
}

// TestReconcileMissingEntry 部分失败：账本有凭证但整批没有回执 -> unmatched_entry。
func TestReconcileMissingEntry(t *testing.T) {
	_, r := setupLedger(t, baseReqs())
	rep, err := r.Reconcile(context.Background(), []recon.Receipt{
		rc("R1", "T1", "CNY", 1000),
		// T2/T3 的回执丢失（模拟部分失败）
	})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, "missing-entry", rep)
	cnt := countTypes(rep)
	if cnt[recon.UnmatchedEntry] != 2 {
		t.Fatalf("want 2 unmatched_entry, got %d", cnt[recon.UnmatchedEntry])
	}

	// 补传：T2 对上后，后续批次不再重复报 T2 的 unmatched_entry
	rep2, err := r.Reconcile(context.Background(), []recon.Receipt{
		rc("R2", "T2", "CNY", 2500),
	})
	if err != nil {
		t.Fatal(err)
	}
	logReport(t, "catch-up", rep2)
	cnt2 := countTypes(rep2)
	if cnt2[recon.UnmatchedEntry] != 1 {
		t.Fatalf("only T3 should remain unmatched, got %d", cnt2[recon.UnmatchedEntry])
	}
	if len(rep2.Matched) != 1 {
		t.Fatalf("R2 should match on catch-up, got %v", rep2.Matched)
	}
}
