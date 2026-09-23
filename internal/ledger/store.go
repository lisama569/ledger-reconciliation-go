package ledger

import (
	"context"
	"sync"
	"time"
)

// Store 是本地持久化的抽象（内存实现模拟本地存储）。
// 所有写操作都在同一把互斥锁下的事务中完成，保证多账户更新的原子性：
// 事务回调返回 error（或 panic）时全部回滚，绝不产生半笔入账。
type Store interface {
	// CreateAccount 幂等地创建账户（已存在则返回既有账户）。
	CreateAccount(ctx context.Context, acct Account) (*Account, error)
	// GetAccount 查询账户快照，不存在返回 KindAccountNotFound。
	GetAccount(ctx context.Context, id string) (*Account, error)
	// ListAccounts 返回全部账户快照（用于对账与守恒校验）。
	ListAccounts(ctx context.Context) ([]Account, error)
	// ListTransactions 返回全部已入账凭证（按入账顺序）。
	ListTransactions(ctx context.Context) ([]Transaction, error)

	// Begin 在全局锁下执行 fn。fn 内通过 Tx 读取/写入，
	// 返回 error 时整体回滚（不写回任何变更），返回 nil 时一次提交。
	Begin(ctx context.Context, fn func(tx Tx) error) error

	// GetTransfer 按业务单号查询已成功入账的转账结果（只读，事务外诊断用）。
	GetTransfer(ctx context.Context, businessID string) (TransferResult, bool, error)
}

// Tx 是事务句柄，仅允许在 Store.Begin 的回调内使用。
type Tx interface {
	GetAccount(id string) (*Account, error)
	// GetTransfer 查询同事务内已提交的幂等转账记录。
	GetTransfer(businessID string) (TransferResult, bool)
	// Apply 写入凭证、幂等记录并更新账户余额；
	// 借贷不平或账户不存在时返回错误（由 Begin 负责回滚）。
	Apply(txn Transaction, res TransferResult, updates map[string]int64) error
	// ApplyStatus 更新账户状态（来自 GetAccount 的快照）。
	ApplyStatus(acct *Account) error
}

// MemoryStore 基于 map + 单把互斥锁的本地存储。
type MemoryStore struct {
	mu       sync.Mutex
	accounts map[string]*Account
	transfer map[string]TransferResult
	txns     []Transaction
}

// NewMemoryStore 创建空的内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		accounts: map[string]*Account{},
		transfer: map[string]TransferResult{},
	}
}

func cloneAccount(a *Account) *Account {
	c := *a
	return &c
}

func (s *MemoryStore) CreateAccount(ctx context.Context, acct Account) (*Account, error) {
	if acct.ID == "" {
		return nil, NewError(KindInvalidArgument, "account id is empty")
	}
	if acct.Currency == "" {
		return nil, NewError(KindInvalidArgument, "account currency is empty")
	}
	if acct.Balance < 0 {
		return nil, NewError(KindInvalidArgument, "initial balance must be non-negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if exist, ok := s.accounts[acct.ID]; ok {
		return cloneAccount(exist), nil
	}
	if acct.Status == "" {
		acct.Status = StatusActive
	}
	if acct.CreatedAt.IsZero() {
		acct.CreatedAt = time.Now()
	}
	a := acct
	s.accounts[a.ID] = &a
	return cloneAccount(&a), nil
}

func (s *MemoryStore) GetAccount(ctx context.Context, id string) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, NewError(KindAccountNotFound, "account not found: "+id)
	}
	return cloneAccount(a), nil
}

func (s *MemoryStore) ListAccounts(ctx context.Context) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, *cloneAccount(a))
	}
	return out, nil
}

func (s *MemoryStore) ListTransactions(ctx context.Context) ([]Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Transaction, len(s.txns))
	copy(out, s.txns)
	return out, nil
}

func (s *MemoryStore) GetTransfer(ctx context.Context, businessID string) (TransferResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.transfer[businessID]
	if ok {
		r.Entries = append([]Entry(nil), r.Entries...)
	}
	return r, ok, nil
}

// snapshot 是回滚用的深拷贝快照。
type snapshot struct {
	accounts map[string]*Account
	transfer map[string]TransferResult
	txns     []Transaction
}

func (s *MemoryStore) takeSnapshot() snapshot {
	accts := make(map[string]*Account, len(s.accounts))
	for k, v := range s.accounts {
		accts[k] = cloneAccount(v)
	}
	trs := make(map[string]TransferResult, len(s.transfer))
	for k, v := range s.transfer {
		v.Entries = append([]Entry(nil), v.Entries...)
		trs[k] = v
	}
	txns := append([]Transaction(nil), s.txns...)
	return snapshot{accts, trs, txns}
}

func (s *MemoryStore) restore(snap snapshot) {
	s.accounts = snap.accounts
	s.transfer = snap.transfer
	s.txns = snap.txns
}

func (s *MemoryStore) Begin(ctx context.Context, fn func(tx Tx) error) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.takeSnapshot()
	v := &txView{s: s}
	committed := false
	defer func() {
		if p := recover(); p != nil {
			s.restore(snap)
			panic(p)
		}
		if err != nil && !committed {
			s.restore(snap)
		}
	}()
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = fn(v); err != nil {
		return err
	}
	committed = true
	return nil
}

// txView 是事务内的一致性视图，持锁期间直接操作实时 map。
type txView struct {
	s *MemoryStore
}

func (v *txView) GetAccount(id string) (*Account, error) {
	a, ok := v.s.accounts[id]
	if !ok {
		return nil, NewError(KindAccountNotFound, "account not found: "+id)
	}
	return cloneAccount(a), nil
}

func (v *txView) GetTransfer(businessID string) (TransferResult, bool) {
	r, ok := v.s.transfer[businessID]
	if ok {
		r.Entries = append([]Entry(nil), r.Entries...)
	}
	return r, ok
}

func (v *txView) Apply(txn Transaction, res TransferResult, updates map[string]int64) error {
	var debitSum, creditSum int64
	for _, e := range txn.Entries {
		switch e.Direction {
		case Debit:
			debitSum += e.Amount
		case Credit:
			creditSum += e.Amount
		default:
			return NewError(KindInvalidArgument, "unknown entry direction: "+string(e.Direction))
		}
		if e.Amount <= 0 {
			return NewError(KindInvalidArgument, "entry amount must be positive")
		}
	}
	if len(txn.Entries) < 2 || debitSum != creditSum {
		return NewError(KindInvalidArgument, "entries are not balanced double-entry")
	}
	if txn.BusinessID == "" {
		return NewError(KindInvalidArgument, "transaction business id is empty")
	}
	if _, dup := v.s.transfer[txn.BusinessID]; dup {
		// 正常流程下幂等检查在 Apply 之前；走到这里属于编程错误。
		return NewError(KindIdempotencyConflict, "business id already committed: "+txn.BusinessID)
	}
	// 更新必须与分录引用的账户完全一致，且变动净额为 0（资金守恒）。
	if len(updates) != len(accountSet(txn.Entries)) {
		return NewError(KindInvalidArgument, "balance updates do not match entry accounts")
	}
	var net int64
	for id, delta := range updates {
		a, ok := v.s.accounts[id]
		if !ok {
			return NewError(KindAccountNotFound, "account not found: "+id)
		}
		a.Balance += delta
		a.Version++
		net += delta
	}
	if net != 0 {
		// 不会真正写回：Begin 会回滚。
		return NewError(KindInvalidArgument, "balance updates violate conservation (net != 0)")
	}
	txn.Entries = append([]Entry(nil), txn.Entries...)
	if txn.CreatedAt.IsZero() {
		txn.CreatedAt = time.Now()
	}
	v.s.txns = append(v.s.txns, txn)
	res.Entries = append([]Entry(nil), res.Entries...)
	v.s.transfer[txn.BusinessID] = res
	return nil
}

func accountSet(entries []Entry) map[string]struct{} {
	set := map[string]struct{}{}
	for _, e := range entries {
		set[e.AccountID] = struct{}{}
	}
	return set
}

func (v *txView) ApplyStatus(acct *Account) error {
	cur, ok := v.s.accounts[acct.ID]
	if !ok {
		return NewError(KindAccountNotFound, "account not found: "+acct.ID)
	}
	cur.Status = acct.Status
	cur.Version++
	return nil
}
