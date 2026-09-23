package ledger

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// newTestLedger 构造两个 CNY 账户：alice 10000，bob 2000。
func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l := NewLedger()
	if _, err := l.CreateAccount("alice", "Alice", map[string]int64{"CNY": 10000}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, err := l.CreateAccount("bob", "Bob", map[string]int64{"CNY": 2000}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	return l
}

func TestTransferBasicAndConservation(t *testing.T) {
	l := newTestLedger(t)

	before := l.TotalFunds("CNY")
	t.Logf("输入: alice->bob 3000 CNY; 入账前总资金=%d, alice=%d, bob=%d",
		before, mustBalance(t, l, "alice", "CNY"), mustBalance(t, l, "bob", "CNY"))

	res, err := l.Transfer(IdempotentRequest{
		BizID: "B-001", FromAcct: "alice", ToAcct: "bob", Amount: 3000, Currency: "CNY",
	})
	if err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	t.Logf("幂等判定依据: BizID=B-001 decision=%s txnID=%s", res.Decision, res.TxnID)

	if got := mustBalance(t, l, "alice", "CNY"); got != 7000 {
		t.Fatalf("alice balance = %d, want 7000", got)
	}
	if got := mustBalance(t, l, "bob", "CNY"); got != 5000 {
		t.Fatalf("bob balance = %d, want 5000", got)
	}
	after := l.TotalFunds("CNY")
	if after != before {
		t.Fatalf("资金不守恒: before=%d after=%d", before, after)
	}
	t.Logf("入账后总资金=%d（守恒），alice=%d, bob=%d",
		after, mustBalance(t, l, "alice", "CNY"), mustBalance(t, l, "bob", "CNY"))

	// 流水必须是一借一贷、金额相等方向相反
	_, es, _ := l.Stores()
	txn, ok := es.GetByBizID("B-001")
	if !ok {
		t.Fatal("transaction not posted")
	}
	if len(txn.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(txn.Entries))
	}
	d, c := txn.Entries[0], txn.Entries[1]
	if d.Direction != Debit || c.Direction != Credit ||
		d.Money.Amount != 3000 || c.Money.Amount != 3000 ||
		d.Money.Currency != "CNY" || c.Money.Currency != "CNY" {
		t.Fatalf("double-entry malformed: %+v", txn.Entries)
	}
}

func TestRejectReasons(t *testing.T) {
	l := newTestLedger(t)
	cases := []struct {
		name string
		req  IdempotentRequest
		want error
	}{
		{"余额不足", IdempotentRequest{"B-NSF", "alice", "bob", 99999, "CNY"}, ErrInsufficientBalance},
		{"金额非正", IdempotentRequest{"B-ZERO", "alice", "bob", 0, "CNY"}, ErrInvalidAmount},
		{"收付方相同", IdempotentRequest{"B-SAME", "alice", "alice", 10, "CNY"}, ErrSameAccount},
		{"转出账户不存在", IdempotentRequest{"B-NF1", "nobody", "bob", 10, "CNY"}, ErrAccountNotFound},
		{"转入账户不存在", IdempotentRequest{"B-NF2", "alice", "nobody", 10, "CNY"}, ErrAccountNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := l.Transfer(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			t.Logf("输入=%+v 被拒绝，原因可区分: %v", tc.req, err)
		})
	}

	// 冻结与关闭账户
	if err := l.SetAccountStatus("bob", StatusFrozen); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Transfer(IdempotentRequest{"B-FRZ", "alice", "bob", 10, "CNY"}); !errors.Is(err, ErrAccountFrozen) {
		t.Fatalf("frozen: err = %v, want ErrAccountFrozen", err)
	} else {
		t.Logf("转入冻结账户被拒绝: %v", err)
	}
	if err := l.SetAccountStatus("bob", StatusClosed); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Transfer(IdempotentRequest{"B-CLS", "alice", "bob", 10, "CNY"}); !errors.Is(err, ErrAccountClosed) {
		t.Fatalf("closed: err = %v, want ErrAccountClosed", err)
	} else {
		t.Logf("转入关闭账户被拒绝: %v", err)
	}

	// 所有拒绝均不得产生半笔：余额与总资金保持初始值
	if got := mustBalance(t, l, "alice", "CNY"); got != 10000 {
		t.Fatalf("alice balance after rejects = %d, want 10000", got)
	}
	if got := mustBalance(t, l, "bob", "CNY"); got != 2000 {
		t.Fatalf("bob balance after rejects = %d, want 2000", got)
	}
	if total := l.TotalFunds("CNY"); total != 12000 {
		t.Fatalf("total after rejects = %d, want 12000", total)
	}
}

func TestIdempotentReplay(t *testing.T) {
	l := newTestLedger(t)
	req := IdempotentRequest{"B-IDEM", "alice", "bob", 1500, "CNY"}

	first, err := l.Transfer(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("首次提交: 输入=%+v decision=%s txnID=%s", req, first.Decision, first.TxnID)

	// 串行重复提交
	for i := 0; i < 3; i++ {
		res, err := l.Transfer(req)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !res.Replay || res.TxnID != first.TxnID {
			t.Fatalf("replay %d result inconsistent: %+v", i, res)
		}
		t.Logf("重复提交#%d: 幂等判定依据 decision=%s txnID=%s（与首次一致）", i, res.Decision, res.TxnID)
	}

	if got := mustBalance(t, l, "alice", "CNY"); got != 8500 {
		t.Fatalf("alice = %d, want 8500（只扣一次）", got)
	}
	if got := mustBalance(t, l, "bob", "CNY"); got != 3500 {
		t.Fatalf("bob = %d, want 3500（只入一次）", got)
	}
	_, es, _ := l.Stores()
	if len(es.All()) != 1 {
		t.Fatalf("transactions = %d, want 1", len(es.All()))
	}
}

func TestIdempotencyConflict(t *testing.T) {
	l := newTestLedger(t)
	first := IdempotentRequest{"B-X", "alice", "bob", 100, "CNY"}
	if _, err := l.Transfer(first); err != nil {
		t.Fatal(err)
	}

	// 同一 BizID 但金额不同
	conflict := IdempotentRequest{"B-X", "alice", "bob", 200, "CNY"}
	_, err := l.Transfer(conflict)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
	t.Logf("输入=%+v 复用 B-X 但金额=200，指纹不同，冲突拒绝（不静默覆盖）: %v", conflict, err)

	// 冲突不得产生任何账务效果
	if got := mustBalance(t, l, "alice", "CNY"); got != 9900 {
		t.Fatalf("alice = %d, want 9900", got)
	}
	if got := mustBalance(t, l, "bob", "CNY"); got != 2100 {
		t.Fatalf("bob = %d, want 2100", got)
	}

	// 原单据仍可正常重放
	res, err := l.Transfer(first)
	if err != nil || !res.Replay {
		t.Fatalf("original replay failed: %+v %v", res, err)
	}
	t.Logf("冲突后原单据重放正常: decision=%s txnID=%s", res.Decision, res.TxnID)
}

func TestConcurrentTransfer(t *testing.T) {
	l := newTestLedger(t)
	// 30 个 goroutine，各自使用独立 BizID 并发提交
	const n = 30
	const amt = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := IdempotentRequest{
				BizID:    fmt.Sprintf("B-C-%02d", i),
				FromAcct: "alice", ToAcct: "bob",
				Amount: amt, Currency: "CNY",
			}
			res, err := l.Transfer(req)
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
				return
			}
			t.Logf("并发 worker %d: 输入=%+v 幂等判定=%s txnID=%s", i, req, res.Decision, res.TxnID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// 全部独立单据入账成功
	_, es, _ := l.Stores()
	if len(es.All()) != n {
		t.Fatalf("transactions = %d, want %d", len(es.All()), n)
	}
	if got := mustBalance(t, l, "alice", "CNY"); got != 10000-n*amt {
		t.Fatalf("alice = %d, want %d", got, 10000-n*amt)
	}
	if got := mustBalance(t, l, "bob", "CNY"); got != 2000+n*amt {
		t.Fatalf("bob = %d, want %d", got, 2000+n*amt)
	}
	if total := l.TotalFunds("CNY"); total != 12000 {
		t.Fatalf("concurrent total = %d, want 12000（守恒）", total)
	}
	t.Logf("并发入账后: alice=%d bob=%d 总资金=%d（守恒，无丢失/重复）",
		mustBalance(t, l, "alice", "CNY"), mustBalance(t, l, "bob", "CNY"), l.TotalFunds("CNY"))

	// 同一单据被多个 goroutine 并发重复提交：只能入账一次
	const dup = 20
	var wg2 sync.WaitGroup
	var dupErr error
	var errMu sync.Mutex
	txnIDs := make(chan string, dup)
	wg2.Add(dup)
	for i := 0; i < dup; i++ {
		go func() {
			defer wg2.Done()
			res, err := l.Transfer(IdempotentRequest{"B-DUP", "alice", "bob", 5, "CNY"})
			if err != nil {
				errMu.Lock()
				dupErr = err
				errMu.Unlock()
				return
			}
			txnIDs <- res.TxnID
		}()
	}
	wg2.Wait()
	close(txnIDs)
	if dupErr != nil {
		t.Fatal(dupErr)
	}
	first := ""
	count := 0
	for id := range txnIDs {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("inconsistent txn id under concurrent duplicate: %s vs %s", first, id)
		}
		count++
	}
	if count != dup {
		t.Fatalf("dup responses = %d, want %d", count, dup)
	}
	if len(es.All()) != n+1 {
		t.Fatalf("transactions after dup = %d, want %d", len(es.All()), n+1)
	}
	t.Logf("并发重复提交 B-DUP %d 次：全部返回同一 txnID=%s，仅入账一次，判定依据=幂等表指纹", dup, first)
}

func mustBalance(t *testing.T, l *Ledger, acct, ccy string) int64 {
	t.Helper()
	v, err := l.Balance(acct, ccy)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
