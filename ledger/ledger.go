package ledger

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// TransferResult 入账结果。重复提交返回同一个结果。
type TransferResult struct {
	TxnID    string
	BizID    string
	Replay   bool   // 是否为幂等重放
	Decision string // 幂等判定依据：NEW / REPLAY / CONFLICT
	Money    Money
	FromAcct string
	ToAcct   string
}

// Ledger 本地双分录账本：聚合账户、流水、幂等三个存储。
// 写操作以单一互斥锁串行化，配合幂等表，保证并发下同一业务单号
// 只会入账一次；双分录同成同败，资金总额在任意入账前后守恒。
type Ledger struct {
	accounts *AccountStore
	entries  *EntryStore
	idem     *IdempotencyStore

	mu  sync.Mutex
	seq atomic.Int64
}

// NewLedger 创建账本。
func NewLedger() *Ledger {
	return &Ledger{
		accounts: NewAccountStore(),
		entries:  NewEntryStore(),
		idem:     NewIdempotencyStore(),
	}
}

// Stores 暴露存储给对账等组件。
func (l *Ledger) Stores() (*AccountStore, *EntryStore, *IdempotencyStore) {
	return l.accounts, l.entries, l.idem
}

// CreateAccount 开户，opening 为各币种初始余额。
func (l *Ledger) CreateAccount(id, name string, opening map[string]int64) (*Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accounts.Create(id, name, opening)
}

// SetAccountStatus 调整账户状态（冻结/关闭/恢复）。
func (l *Ledger) SetAccountStatus(id string, status AccountStatus) error {
	return l.accounts.SetStatus(id, status)
}

// newTxnID 生成单调唯一的事务 ID。
func (l *Ledger) newTxnID() string {
	n := l.seq.Add(1)
	return fmt.Sprintf("TXN%010d", n)
}

// Transfer 以业务单号幂等提交一笔双分录转账。
// 步骤（全程持有账本写锁，天然串行化）：
//  1. 幂等判定：NEW 继续；REPLAY 返回首次结果；CONFLICT 返回冲突错误；
//  2. 业务校验：金额、账户存在性、账户状态、收付方不同、币种一致；
//  3. 双分录落账：先借后贷。借方余额不足则整体放弃，不留半笔；
//  4. 写入流水与幂等记录。
func (l *Ledger) Transfer(req IdempotentRequest) (TransferResult, error) {
	m := NewMoney(req.Amount, req.Currency)

	l.mu.Lock()
	defer l.mu.Unlock()

	// 1. 幂等判定
	decision, rec := l.idem.Decide(req)
	switch decision {
	case IdemConflict:
		return TransferResult{
				BizID:    req.BizID,
				Decision: string(IdemConflict),
			}, fmt.Errorf("%w: bizID=%s storedFingerprint=%s incomingFingerprint=%s",
				ErrIdempotencyConflict, req.BizID, rec.Fingerprint, req.fingerprint())
	case IdemReplay:
		txn, ok := l.entries.GetByBizID(req.BizID)
		if !ok {
			// 理论上不会发生：幂等记录与流水同事务写入
			return TransferResult{}, fmt.Errorf("idempotency record exists but transaction missing for bizID=%s", req.BizID)
		}
		return TransferResult{
			TxnID:    txn.ID,
			BizID:    txn.BizID,
			Replay:   true,
			Decision: string(IdemReplay),
			Money:    txn.Money,
			FromAcct: txn.FromAcct,
			ToAcct:   txn.ToAcct,
		}, nil
	}

	// 2. 业务校验
	if req.BizID == "" {
		return TransferResult{}, errors.New("bizID is required")
	}
	if !m.Positive() {
		return TransferResult{}, ErrInvalidAmount
	}
	if req.FromAcct == req.ToAcct {
		return TransferResult{}, ErrSameAccount
	}
	from, ok := l.accounts.Get(req.FromAcct)
	if !ok {
		return TransferResult{}, fmt.Errorf("%w: %s", ErrAccountNotFound, req.FromAcct)
	}
	to, ok := l.accounts.Get(req.ToAcct)
	if !ok {
		return TransferResult{}, fmt.Errorf("%w: %s", ErrAccountNotFound, req.ToAcct)
	}
	switch from.StatusOf() {
	case StatusClosed:
		return TransferResult{}, fmt.Errorf("%w: %s", ErrAccountClosed, req.FromAcct)
	case StatusFrozen:
		return TransferResult{}, fmt.Errorf("%w: %s", ErrAccountFrozen, req.FromAcct)
	}
	switch to.StatusOf() {
	case StatusClosed:
		return TransferResult{}, fmt.Errorf("%w: %s", ErrAccountClosed, req.ToAcct)
	case StatusFrozen:
		return TransferResult{}, fmt.Errorf("%w: %s", ErrAccountFrozen, req.ToAcct)
	}
	// 币种一致性：转账只能在同币种账户间进行（双方都必须持有该币种轨迹）。
	if !from.HasCurrency(m.Currency) {
		return TransferResult{}, fmt.Errorf("%w: account %s has no %s ledger", ErrCurrencyMismatch, req.FromAcct, m.Currency)
	}
	if !to.HasCurrency(m.Currency) {
		return TransferResult{}, fmt.Errorf("%w: account %s has no %s ledger", ErrCurrencyMismatch, req.ToAcct, m.Currency)
	}

	// 3. 双分录落账：先借（余额不足在此暴露），再贷。
	if err := from.debit(m); err != nil {
		return TransferResult{}, err
	}
	to.credit(m)

	// 4. 写流水（借贷各一条，金额相等方向相反）
	txnID := l.newTxnID()
	txn := &Transaction{
		ID:       txnID,
		BizID:    req.BizID,
		FromAcct: req.FromAcct,
		ToAcct:   req.ToAcct,
		Money:    m,
		Entries: []Entry{
			{ID: txnID + "-D", TxnID: txnID, AccountID: req.FromAcct, Direction: Debit, Money: m},
			{ID: txnID + "-C", TxnID: txnID, AccountID: req.ToAcct, Direction: Credit, Money: m},
		},
	}
	if !l.entries.put(txn) {
		// 同 BizID 已入账但幂等表无记录属于内部不一致，直接失败暴露问题
		return TransferResult{}, fmt.Errorf("transaction already posted for bizID=%s", req.BizID)
	}
	l.idem.Save(req.BizID, req.fingerprint(), txnID)

	return TransferResult{
		TxnID:    txnID,
		BizID:    req.BizID,
		Replay:   false,
		Decision: string(IdemNew),
		Money:    m,
		FromAcct: req.FromAcct,
		ToAcct:   req.ToAcct,
	}, nil
}

// Balance 查询某账户某币种余额。
func (l *Ledger) Balance(accountID, currency string) (int64, error) {
	a, ok := l.accounts.Get(accountID)
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	return a.BalanceOf(currency), nil
}

// TotalFunds 返回系统内某币种全部账户余额之和（守恒断言用）。
// 双分录转账只在账户间搬运资金，因此该值在任意入账前后保持不变。
func (l *Ledger) TotalFunds(currency string) int64 {
	var total int64
	for _, a := range l.accounts.All() {
		total += a.BalanceOf(currency)
	}
	return total
}
