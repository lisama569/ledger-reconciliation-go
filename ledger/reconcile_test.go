package ledger

import "testing"

// newReconcileFixture 构造 alice/bob 两个账户与一个账本。
func newReconcileFixture(t *testing.T) *Ledger {
	t.Helper()
	l := NewLedger()
	if _, err := l.CreateAccount("alice", "Alice", map[string]int64{"CNY": 10000, "USD": 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.CreateAccount("bob", "Bob", map[string]int64{"CNY": 2000, "USD": 100}); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReconcileMatched(t *testing.T) {
	l := newReconcileFixture(t)
	res, err := l.Transfer(IdempotentRequest{"R-OK", "alice", "bob", 800, "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	rs := NewReceiptStore()
	rs.Add(&SettlementReceipt{ReceiptID: "RC-1", BizID: "R-OK", Amount: 800, Currency: "CNY", Source: "bank-A"})

	t.Logf("输入回执={RC-1 bizID=R-OK 800 CNY}，对应流水 txnID=%s（金额/币种/BizID 均一致）", res.TxnID)
	rep := NewReconciler(l, rs).Run()
	if len(rep.Discrepancies) != 0 {
		t.Fatalf("unexpected discrepancies: %v", rep.Discrepancies)
	}
	if len(rep.Matched) != 1 || rep.Matched[0].Discrepancy != DiscrepancyNone {
		t.Fatalf("matched = %+v", rep.Matched)
	}
	t.Logf("对账结果: 匹配成功 1 笔，无差异")
}

func TestReconcileDiscrepancies(t *testing.T) {
	l := newReconcileFixture(t)

	// 账内流水：
	// R-AMT 300 CNY —— 回执金额不符
	// R-CCY 50 USD —— 回执币种不符
	// R-MISS 100 CNY —— 无回执（漏回）
	// R-DUP 200 CNY —— 收到两份不同回执
	for _, r := range []IdempotentRequest{
		{"R-AMT", "alice", "bob", 300, "CNY"},
		{"R-CCY", "alice", "bob", 50, "USD"},
		{"R-MISS", "alice", "bob", 100, "CNY"},
		{"R-DUP", "alice", "bob", 200, "CNY"},
	} {
		if _, err := l.Transfer(r); err != nil {
			t.Fatal(err)
		}
	}

	rs := NewReceiptStore()
	rs.Add(&SettlementReceipt{ReceiptID: "RC-AMT", BizID: "R-AMT", Amount: 301, Currency: "CNY"})
	rs.Add(&SettlementReceipt{ReceiptID: "RC-CCY", BizID: "R-CCY", Amount: 50, Currency: "CNY"}) // 流水是 USD
	rs.Add(&SettlementReceipt{ReceiptID: "RC-GHOST", BizID: "R-NEVER", Amount: 10, Currency: "CNY"})
	rs.Add(&SettlementReceipt{ReceiptID: "RC-DUP1", BizID: "R-DUP", Amount: 200, Currency: "CNY"})
	rs.Add(&SettlementReceipt{ReceiptID: "RC-DUP2", BizID: "R-DUP", Amount: 200, Currency: "CNY"})

	t.Logf("输入回执 5 份: 金额不符(R-AMT 301 vs 300), 币种不符(R-CCY CNY vs USD), " +
		"未匹配(R-NEVER 无流水), 重复回执(R-DUP 两份), 另有流水 R-MISS 无回执")

	rep := NewReconciler(l, rs).Run()

	want := map[ReconcileDiscrepancyType]int{
		DiscrepancyAmountMismatch:   1,
		DiscrepancyCurrencyMismatch: 1,
		DiscrepancyUnmatched:        1,
		DiscrepancyDuplicateReceipt: 1,
		DiscrepancyMissingReceipt:   1,
	}
	got := map[ReconcileDiscrepancyType]int{}
	for _, d := range rep.Discrepancies {
		got[d.Type]++
		t.Logf("对账差异: 类型=%s receipt=%+v entry=%s 说明=%s", d.Type, d.Receipt, d.EntryID, d.Detail)
	}
	for typ, n := range want {
		if got[typ] != n {
			t.Errorf("差异 %s 数量=%d, want %d", typ, got[typ], n)
		}
	}
	// 只有 R-DUP 的第一份回执完全匹配；金额/币种不符、未匹配均不消费
	if len(rep.Matched) != 1 {
		t.Fatalf("matched = %d, want 1", len(rep.Matched))
	}
}

func TestReconcileRetryNoDuplicate(t *testing.T) {
	l := newReconcileFixture(t)
	beforeCNY := l.TotalFunds("CNY")
	beforeUSD := l.TotalFunds("USD")

	if _, err := l.Transfer(IdempotentRequest{"R-1", "alice", "bob", 100, "CNY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Transfer(IdempotentRequest{"R-2", "alice", "bob", 10, "USD"}); err != nil {
		t.Fatal(err)
	}
	// 第一份回执金额有误，模拟部分失败
	rs := NewReceiptStore()
	rs.Add(&SettlementReceipt{ReceiptID: "RC-1", BizID: "R-1", Amount: 999, Currency: "CNY"})

	rec := NewReconciler(l, rs)
	rep1 := rec.Run()
	if !rep1.HasType(DiscrepancyAmountMismatch) || !rep1.HasType(DiscrepancyMissingReceipt) {
		t.Fatalf("first run discrepancies = %v", rep1.Discrepancies)
	}
	t.Logf("第 1 轮对账（部分失败）：匹配=%d 差异=%d（金额不符 + R-2 缺回执），账本未改动",
		len(rep1.Matched), len(rep1.Discrepancies))

	// 渠道修正后重投 RC-1，并补齐 RC-2
	rs.Add(&SettlementReceipt{ReceiptID: "RC-1", BizID: "R-1", Amount: 100, Currency: "CNY"})
	rs.Add(&SettlementReceipt{ReceiptID: "RC-2", BizID: "R-2", Amount: 10, Currency: "USD"})
	rep2 := rec.Run()
	if len(rep2.Matched) != 2 {
		t.Fatalf("second run matched = %d, want 2; discrepancies=%v", len(rep2.Matched), rep2.Discrepancies)
	}
	if len(rep2.Discrepancies) != 0 {
		t.Fatalf("second run discrepancies = %v", rep2.Discrepancies)
	}
	t.Logf("第 2 轮对账（重试）：匹配=%d 差异=0，修正后的回执被正常消费", len(rep2.Matched))

	// 再次重跑：同批回执已是重复投递，必须可区分地标为 DUPLICATE_RECEIPT，
	// 而不是静默丢弃；账本余额与流水不发生任何变化
	rep3 := rec.Run()
	if len(rep3.Discrepancies) != 2 || !rep3.HasType(DiscrepancyDuplicateReceipt) {
		t.Fatalf("third run discrepancies = %v", rep3.Discrepancies)
	}
	for _, d := range rep3.Discrepancies {
		t.Logf("第 3 轮对账（重跑）差异: 类型=%s 说明=%s", d.Type, d.Detail)
	}
	if l.TotalFunds("CNY") != beforeCNY || l.TotalFunds("USD") != beforeUSD {
		t.Fatalf("对账改变了账本资金: CNY %d->%d USD %d->%d",
			beforeCNY, l.TotalFunds("CNY"), beforeUSD, l.TotalFunds("USD"))
	}
	_, es, _ := l.Stores()
	if len(es.All()) != 2 {
		t.Fatalf("transactions = %d, want 2（对账不得记账）", len(es.All()))
	}
	t.Logf("三轮对账后账本不变: CNY 总额=%d, USD 总额=%d, 流水=%d 笔（无重复无漏记）",
		l.TotalFunds("CNY"), l.TotalFunds("USD"), len(es.All()))
}
