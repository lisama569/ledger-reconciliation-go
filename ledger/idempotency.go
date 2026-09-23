package ledger

import (
	"fmt"
	"sync"
)

// IdempotentRequest 以业务单号为幂等键的转账请求。
type IdempotentRequest struct {
	// BizID 业务单号，幂等键。
	BizID string
	// 以下字段构成语义指纹：重复提交必须完全一致，否则判定冲突。
	FromAcct string
	ToAcct   string
	Amount   int64
	Currency string
}

// fingerprint 返回语义指纹。判定依据是业务单号之外的全部语义字段：
// 收付方账户、金额、币种，任何一项不同即视为冲突，绝不静默覆盖。
func (r IdempotentRequest) fingerprint() string {
	return fmt.Sprintf("%s|%s|%d|%s", r.FromAcct, r.ToAcct, r.Amount, r.Currency)
}

// IdempotencyDecision 幂等判定结果。
type IdempotencyDecision string

const (
	// IdemNew 首次提交。
	IdemNew IdempotencyDecision = "NEW"
	// IdemReplay 重复提交且语义一致，重放首次结果。
	IdemReplay IdempotencyDecision = "REPLAY"
	// IdemConflict 同一 BizID 语义不同。
	IdemConflict IdempotencyDecision = "CONFLICT"
)

// IdempotencyRecord 幂等记录：键 -> 首次请求的语义指纹与事务 ID。
type IdempotencyRecord struct {
	BizID       string
	Fingerprint string
	TxnID       string
}

// IdempotencyStore 幂等记录存储。
type IdempotencyStore struct {
	mu      sync.Mutex
	records map[string]*IdempotencyRecord
}

// NewIdempotencyStore 创建空幂等存储。
func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{records: make(map[string]*IdempotencyRecord)}
}

// Decide 对请求做幂等判定。
func (s *IdempotencyStore) Decide(req IdempotentRequest) (IdempotencyDecision, *IdempotencyRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, exists := s.records[req.BizID]
	if !exists {
		return IdemNew, nil
	}
	if rec.Fingerprint != req.fingerprint() {
		return IdemConflict, rec
	}
	return IdemReplay, rec
}

// Save 记录首次成功提交的处理结果。
func (s *IdempotencyStore) Save(bizID, fingerprint, txnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[bizID] = &IdempotencyRecord{
		BizID:       bizID,
		Fingerprint: fingerprint,
		TxnID:       txnID,
	}
}

// Get 按业务单号读取幂等记录。
func (s *IdempotencyStore) Get(bizID string) (*IdempotencyRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[bizID]
	return rec, ok
}
