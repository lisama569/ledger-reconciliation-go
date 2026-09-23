package ledger_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/lisama569/ledger-reconciliation-go/internal/ledger"
)

func newSvc(t *testing.T) (*ledger.Service, *ledger.MemoryStore) {
	t.Helper()
	st := ledger.NewMemoryStore()
	return ledger.NewService(st), st
}

func mustOpen(t *testing.T, svc *ledger.Service, id string, ccy ledger.Currency, bal int64) {
	t.Helper()
	if _, err := svc.OpenAccount(context.Background(), id, ccy, bal); err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
}

func total(t *testing.T, svc *ledger.Service, ccy ledger.Currency) int64 {
	t.Helper()
	n, err := svc.TotalBalance(context.Background(), ccy)
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	return n
}

// TestTransferConservationAndResult 基本双分录：守恒 + 余额正确 + 分录结构。
func TestTransferConservationAndResult(t *testing.T) {
	svc, _ := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 1000)
	mustOpen(t, svc, "B", "CNY", 500)
	before := total(t, svc, "CNY")

	res, err := svc.Transfer(context.Background(), ledger.TransferRequest{
		BusinessID: "BIZ-1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 300,
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if res.Duplicated {
		t.Fatalf("first call must not be duplicated")
	}
	if len(res.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(res.Entries))
	}
	a, _ := svc.Account(context.Background(), "A")
	b, _ := svc.Account(context.Background(), "B")
	if a.Balance != 700 || b.Balance != 800 {
		t.Fatalf("balances got A=%d B=%d, want 700/800", a.Balance, b.Balance)
	}
	after := total(t, svc, "CNY")
	t.Logf("输入: A=1000 B=500 转账300; 转账前总额=%d 转账后总额=%d", before, after)
	if before != after {
		t.Fatalf("资金不守恒: %d -> %d", before, after)
	}
}

// TestTransferErrorKinds 所有拒绝原因必须可区分，且不产生半笔入账。
func TestTransferErrorKinds(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		req   ledger.TransferRequest
		want  ledger.ErrorKind
		setup func(t *testing.T, svc *ledger.Service)
	}{
		{"amount_not_positive", ledger.TransferRequest{BusinessID: "X1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 0}, ledger.KindInvalidArgument,
			func(t *testing.T, svc *ledger.Service) {
				mustOpen(t, svc, "A", "CNY", 100)
				mustOpen(t, svc, "B", "CNY", 100)
			}},
		{"same_account", ledger.TransferRequest{BusinessID: "X2", FromID: "A", ToID: "A", Currency: "CNY", Amount: 10}, ledger.KindInvalidArgument,
			func(t *testing.T, svc *ledger.Service) { mustOpen(t, svc, "A", "CNY", 100) }},
		{"from_not_found", ledger.TransferRequest{BusinessID: "X3", FromID: "NOPE", ToID: "B", Currency: "CNY", Amount: 10}, ledger.KindAccountNotFound,
			func(t *testing.T, svc *ledger.Service) { mustOpen(t, svc, "B", "CNY", 100) }},
		{"to_not_found", ledger.TransferRequest{BusinessID: "X4", FromID: "A", ToID: "NOPE", Currency: "CNY", Amount: 10}, ledger.KindAccountNotFound,
			func(t *testing.T, svc *ledger.Service) { mustOpen(t, svc, "A", "CNY", 100) }},
		{"currency_mismatch", ledger.TransferRequest{BusinessID: "X5", FromID: "A", ToID: "B", Currency: "EUR", Amount: 10}, ledger.KindCurrencyMismatch,
			func(t *testing.T, svc *ledger.Service) {
				mustOpen(t, svc, "A", "CNY", 100)
				mustOpen(t, svc, "B", "CNY", 100)
			}},
		{"insufficient_funds", ledger.TransferRequest{BusinessID: "X6", FromID: "A", ToID: "B", Currency: "CNY", Amount: 100000}, ledger.KindInsufficientFunds,
			func(t *testing.T, svc *ledger.Service) {
				mustOpen(t, svc, "A", "CNY", 50)
				mustOpen(t, svc, "B", "CNY", 50)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSvc(t)
			tc.setup(t, svc)
			before := total(t, svc, "CNY")
			_, err := svc.Transfer(ctx, tc.req)
			got := ledger.KindOf(err)
			t.Logf("输入: %+v => 错误分类=%q (err=%v)", tc.req, got, err)
			if got != tc.want {
				t.Fatalf("want kind %q, got %q (err=%v)", tc.want, got, err)
			}
			after := total(t, svc, "CNY")
			if before != after {
				t.Fatalf("拒绝后总额发生变化（半笔入账）: %d -> %d", before, after)
			}
		})
	}
}

// TestAccountStatusRejected 冻结/关闭账户的操作必须被拒绝，closed 优先于 frozen。
func TestAccountStatusRejected(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 1000)
	mustOpen(t, svc, "B", "CNY", 1000)
	mustOpen(t, svc, "C", "CNY", 1000)
	if err := svc.SetStatus(ctx, "B", ledger.StatusFrozen); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetStatus(ctx, "C", ledger.StatusClosed); err != nil {
		t.Fatal(err)
	}

	_, err := svc.Transfer(ctx, ledger.TransferRequest{BusinessID: "S1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 10})
	t.Logf("转入冻结账户 => %q: %v", ledger.KindOf(err), err)
	if ledger.KindOf(err) != ledger.KindAccountFrozen {
		t.Fatalf("want frozen, got %v", err)
	}
	_, err = svc.Transfer(ctx, ledger.TransferRequest{BusinessID: "S2", FromID: "C", ToID: "A", Currency: "CNY", Amount: 10})
	t.Logf("从关闭账户转出 => %q: %v", ledger.KindOf(err), err)
	if ledger.KindOf(err) != ledger.KindAccountClosed {
		t.Fatalf("want closed, got %v", err)
	}
}

// TestIdempotentDuplicate 重复提交同号同语义：只入账一次，结果一致。
func TestIdempotentDuplicate(t *testing.T) {
	ctx := context.Background()
	svc, st := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 1000)
	mustOpen(t, svc, "B", "CNY", 0)
	req := ledger.TransferRequest{BusinessID: "DUP-1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 250}

	r1, err := svc.Transfer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.Transfer(ctx, req)
	if err != nil {
		t.Fatalf("duplicate must not error: %v", err)
	}
	if !r2.Duplicated {
		t.Fatalf("second call must be flagged duplicated")
	}
	if !sameEntries(r1.Entries, r2.Entries) {
		t.Fatalf("duplicate returned different entries:\n%v\n%v", r1.Entries, r2.Entries)
	}
	if stored, ok, _ := st.GetTransfer(ctx, "DUP-1"); !ok || len(stored.Entries) != 2 {
		t.Fatalf("idempotency record missing or wrong")
	}
	t.Logf("幂等判定依据: businessID=%s 在幂等表命中，首次/重复返回同一凭证; A=%d B=%d",
		"DUP-1", mustBal(t, svc, "A"), mustBal(t, svc, "B"))
	if mustBal(t, svc, "A") != 750 || mustBal(t, svc, "B") != 250 {
		t.Fatal("duplicate produced extra accounting effect")
	}
}

// TestIdempotencyConflict 同号不同语义：冲突，不覆盖，不入账。
func TestIdempotencyConflict(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 1000)
	mustOpen(t, svc, "B", "CNY", 1000)
	mustOpen(t, svc, "C", "CNY", 1000)
	r1 := ledger.TransferRequest{BusinessID: "BIZ-X", FromID: "A", ToID: "B", Currency: "CNY", Amount: 100}
	if _, err := svc.Transfer(ctx, r1); err != nil {
		t.Fatal(err)
	}
	// 同号：金额不同
	r2 := ledger.TransferRequest{BusinessID: "BIZ-X", FromID: "A", ToID: "B", Currency: "CNY", Amount: 999}
	_, err := svc.Transfer(ctx, r2)
	kind := ledger.KindOf(err)
	t.Logf("输入1=%+v 输入2=%+v => 冲突分类=%q err=%v", r1, r2, kind, err)
	if kind != ledger.KindIdempotencyConflict {
		t.Fatalf("want conflict, got %q: %v", kind, err)
	}
	// 同号：收款方不同
	r3 := ledger.TransferRequest{BusinessID: "BIZ-X", FromID: "A", ToID: "C", Currency: "CNY", Amount: 100}
	if k := ledger.KindOf(func() error { _, e := svc.Transfer(ctx, r3); return e }()); k != ledger.KindIdempotencyConflict {
		t.Fatalf("different payee must conflict, got %q", k)
	}
	// 原记录语义未变
	if mustBal(t, svc, "A") != 900 || mustBal(t, svc, "B") != 1100 || mustBal(t, svc, "C") != 1000 {
		t.Fatalf("conflict must not overwrite or apply; got A=%d B=%d C=%d",
			mustBal(t, svc, "A"), mustBal(t, svc, "B"), mustBal(t, svc, "C"))
	}
}

// TestConcurrentSameBusinessID 并发提交同一单号：恰好一次账务效果，全部返回一致结果。
func TestConcurrentSameBusinessID(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 100000)
	mustOpen(t, svc, "B", "CNY", 0)
	req := ledger.TransferRequest{BusinessID: "CON-1", FromID: "A", ToID: "B", Currency: "CNY", Amount: 1000}

	const n = 64
	var wg sync.WaitGroup
	results := make([]*ledger.TransferResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.Transfer(ctx, req)
		}(i)
	}
	close(start)
	wg.Wait()

	okCount, dupCount := 0, 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent call %d: %v", i, errs[i])
		}
		if results[i].Duplicated {
			dupCount++
		} else {
			okCount++
		}
		if !sameEntries(results[0].Entries, results[i].Entries) {
			t.Fatalf("call %d returned inconsistent entries", i)
		}
	}
	t.Logf("输入并发数=%d 同一businessID=%s => 首次入账=%d 幂等回放=%d; A=%d B=%d",
		n, "CON-1", okCount, dupCount, mustBal(t, svc, "A"), mustBal(t, svc, "B"))
	if okCount != 1 || dupCount != n-1 {
		t.Fatalf("want exactly 1 first + %d duplicates, got %d/%d", n-1, okCount, dupCount)
	}
	if mustBal(t, svc, "A") != 99000 || mustBal(t, svc, "B") != 1000 {
		t.Fatal("concurrent duplicate changed balances unexpectedly")
	}
}

// TestConcurrentDistinctTransfers 并发不同单号：全部成功且总额守恒。
func TestConcurrentDistinctTransfers(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 1_000_000)
	mustOpen(t, svc, "B", "CNY", 1_000_000)
	before := total(t, svc, "CNY")

	const n = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := ledger.TransferRequest{
				BusinessID: fmt.Sprintf("CD-%04d", i),
				FromID:     "A", ToID: "B", Currency: "CNY", Amount: 100,
			}
			if i%2 == 1 {
				req.FromID, req.ToID = "B", "A"
			}
			if _, err := svc.Transfer(ctx, req); err != nil {
				t.Errorf("transfer %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	after := total(t, svc, "CNY")
	t.Logf("并发不同单号数=%d; 总额 before=%d after=%d", n, before, after)
	if before != after {
		t.Fatalf("不守恒: %d -> %d", before, after)
	}
	// A 与 B 各转 100 笔 *100，净变化为 0
	if a, b := mustBal(t, svc, "A"), mustBal(t, svc, "B"); a != 1_000_000 || b != 1_000_000 {
		t.Fatalf("unexpected balances A=%d B=%d", a, b)
	}
}

// TestRejectedTransferNoHalfEntry 失败回滚在并发竞争余额时也不能半笔。
func TestRejectedTransferNoHalfEntry(t *testing.T) {
	ctx := context.Background()
	svc, _ := newSvc(t)
	mustOpen(t, svc, "A", "CNY", 150)
	mustOpen(t, svc, "B", "CNY", 0)
	mustOpen(t, svc, "C", "CNY", 0)

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	var deniedMu sync.Mutex
	denied := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.Transfer(ctx, ledger.TransferRequest{
				BusinessID: fmt.Sprintf("RACE-%02d", i),
				FromID:     "A", ToID: "B", Currency: "CNY", Amount: 100,
			})
			if err != nil {
				if ledger.KindOf(err) != ledger.KindInsufficientFunds {
					t.Errorf("call %d unexpected kind: %v", i, err)
				}
				deniedMu.Lock()
				denied++
				deniedMu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	// 150 的余额只够 1 笔 100
	if a, b := mustBal(t, svc, "A"), mustBal(t, svc, "B"); a != 50 || b != 100 {
		t.Fatalf("want A=50 B=100, got A=%d B=%d", a, b)
	}
	if c := mustBal(t, svc, "C"); c != 0 {
		t.Fatalf("C must stay 0, got %d", c)
	}
	t.Logf("余额竞争: 并发=%d 成功=%d 拒绝(insufficient_funds)=%d; 守恒 OK A+B=%d",
		n, n-denied, denied, mustBal(t, svc, "A")+mustBal(t, svc, "B"))
	if denied != n-1 {
		t.Fatalf("want %d denied, got %d", n-1, denied)
	}
}

func mustBal(t *testing.T, svc *ledger.Service, id string) int64 {
	t.Helper()
	a, err := svc.Account(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func sameEntries(x, y []ledger.Entry) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
