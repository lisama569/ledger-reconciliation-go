package ledger

import (
	"errors"
	"sync"
)

// AccountStatus 账户状态。
type AccountStatus string

const (
	// StatusActive 正常，允许借贷。
	StatusActive AccountStatus = "ACTIVE"
	// StatusFrozen 冻结，拒绝一切账务动作。
	StatusFrozen AccountStatus = "FROZEN"
	// StatusClosed 已关闭，拒绝一切账务动作。
	StatusClosed AccountStatus = "CLOSED"
)

// Account 本地账户。余额为各币种分别累计的最小货币单位整数。
// 账户自身的 mu 保护余额与状态读写；store 的 mu 只保护账户映射。
type Account struct {
	ID       string
	Name     string
	Status   AccountStatus
	Balances map[string]int64 // currency -> 余额
	mu       sync.Mutex
}

// BalanceOf 读取某币种当前余额（缺视为 0）。
func (a *Account) BalanceOf(currency string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Balances[currency]
}

// StatusOf 读取账户状态。
func (a *Account) StatusOf() AccountStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Status
}

// setStatus 修改账户状态。
func (a *Account) setStatus(s AccountStatus) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Status = s
}

// credit 贷记（转入）：余额增加。调用方必须已完成业务校验。
func (a *Account) credit(m Money) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Balances[m.Currency] += m.Amount
}

// debit 借记（转出）：余额减少，余额不足返回 ErrInsufficientBalance，
// 此时余额保持不变，不会产生半笔变动。
func (a *Account) debit(m Money) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Balances[m.Currency] < m.Amount {
		return ErrInsufficientBalance
	}
	a.Balances[m.Currency] -= m.Amount
	return nil
}

// HasCurrency 判断账户是否有该币种的余额轨迹。
func (a *Account) HasCurrency(currency string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.Balances[currency]
	return ok
}

// snapshotBalances 返回余额快照（守恒校验/日志使用）。
func (a *Account) snapshotBalances() map[string]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make(map[string]int64, len(a.Balances))
	for k, v := range a.Balances {
		cp[k] = v
	}
	return cp
}

// AccountStore 账户存储（内存实现）。
type AccountStore struct {
	mu       sync.RWMutex
	accounts map[string]*Account
}

// NewAccountStore 创建空账户存储。
func NewAccountStore() *AccountStore {
	return &AccountStore{accounts: make(map[string]*Account)}
}

// Create 新增账户并写入各币种初始余额（可为 0 或正）。
// 账户 ID 重复时返回错误。
func (s *AccountStore) Create(id, name string, opening map[string]int64) (*Account, error) {
	if id == "" {
		return nil, errors.New("account id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.accounts[id]; exists {
		return nil, errors.New("account already exists: " + id)
	}
	balances := make(map[string]int64, len(opening))
	for ccy, amt := range opening {
		if amt < 0 {
			return nil, errors.New("opening balance must not be negative")
		}
		balances[ccy] = amt
	}
	a := &Account{
		ID:       id,
		Name:     name,
		Status:   StatusActive,
		Balances: balances,
	}
	s.accounts[id] = a
	return a, nil
}

// Get 查询账户。
func (s *AccountStore) Get(id string) (*Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	return a, ok
}

// All 返回全部账户快照。
func (s *AccountStore) All() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	return out
}

// SetStatus 修改账户状态。
func (s *AccountStore) SetStatus(id string, status AccountStatus) error {
	s.mu.RLock()
	a, ok := s.accounts[id]
	s.mu.RUnlock()
	if !ok {
		return ErrAccountNotFound
	}
	a.setStatus(status)
	return nil
}
