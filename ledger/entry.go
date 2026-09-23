package ledger

import "sync"

// Direction 分录方向。
type Direction string

const (
	// Debit 借方：转出方，资产减少。
	Debit Direction = "DEBIT"
	// Credit 贷方：转入方，资产增加。
	Credit Direction = "CREDIT"
)

// Entry 双分录中的一条明细（借/贷）。
type Entry struct {
	ID        string
	TxnID     string
	AccountID string
	Direction Direction
	Money     Money
}

// Transaction 一笔双分录转账事务，恰好包含一借一贷两条分录，
// 两条分录金额相等、币种相同，保证资金守恒。
type Transaction struct {
	ID       string
	BizID    string // 业务单号（幂等键）
	FromAcct string
	ToAcct   string
	Money    Money
	Entries  []Entry
}

// EntryStore 流水存储：按事务 ID 与业务单号双向索引。
type EntryStore struct {
	mu      sync.RWMutex
	byID    map[string]*Transaction
	byBizID map[string]*Transaction
}

// NewEntryStore 创建空流水存储。
func NewEntryStore() *EntryStore {
	return &EntryStore{
		byID:    make(map[string]*Transaction),
		byBizID: make(map[string]*Transaction),
	}
}

// put 落账一笔事务。重复 BizID 返回 false（由调用方通过幂等层避免）。
func (s *EntryStore) put(txn *Transaction) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byBizID[txn.BizID]; exists {
		return false
	}
	s.byID[txn.ID] = txn
	s.byBizID[txn.BizID] = txn
	return true
}

// GetByID 按事务 ID 查询。
func (s *EntryStore) GetByID(id string) (*Transaction, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byID[id]
	return t, ok
}

// GetByBizID 按业务单号查询。
func (s *EntryStore) GetByBizID(bizID string) (*Transaction, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.byBizID[bizID]
	return t, ok
}

// All 返回全部事务快照。
func (s *EntryStore) All() []*Transaction {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Transaction, 0, len(s.byID))
	for _, t := range s.byID {
		out = append(out, t)
	}
	return out
}
