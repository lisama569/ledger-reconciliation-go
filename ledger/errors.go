package ledger

import "fmt"

// 业务可区分的拒绝原因。调用方应使用 errors.Is 判定错误类别。
var (
	// ErrInsufficientBalance 余额不足，无法完成转出。
	ErrInsufficientBalance = newBizError("INSUFFICIENT_BALANCE", "insufficient balance")
	// ErrAccountNotFound 账户不存在。
	ErrAccountNotFound = newBizError("ACCOUNT_NOT_FOUND", "account not found")
	// ErrAccountClosed 账户已关闭，不允许发生账务动作。
	ErrAccountClosed = newBizError("ACCOUNT_CLOSED", "account is closed")
	// ErrAccountFrozen 账户被冻结，不允许发生账务动作。
	ErrAccountFrozen = newBizError("ACCOUNT_FROZEN", "account is frozen")
	// ErrInvalidAmount 金额非法（<=0）。
	ErrInvalidAmount = newBizError("INVALID_AMOUNT", "amount must be positive")
	// ErrCurrencyMismatch 转账双方币种不一致（入账阶段）。
	ErrCurrencyMismatch = newBizError("CURRENCY_MISMATCH", "currency mismatch")
	// ErrSameAccount 收付方为同一账户。
	ErrSameAccount = newBizError("SAME_ACCOUNT", "debit and credit account must differ")
	// ErrIdempotencyConflict 同一业务单号复用但语义不同。
	ErrIdempotencyConflict = newBizError("IDEMPOTENCY_CONFLICT", "request with same idempotency key carries different semantics")
)

// BizError 带可区分错误码的业务错误。
type BizError struct {
	Code    string
	Message string
}

func newBizError(code, msg string) *BizError {
	return &BizError{Code: code, Message: msg}
}

func (e *BizError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Is 支持 errors.Is 比较哨兵错误。
func (e *BizError) Is(target error) bool {
	t, ok := target.(*BizError)
	if !ok {
		return false
	}
	return e.Code == t.Code
}

// ReconcileDiscrepancyType 对账差异类型。
type ReconcileDiscrepancyType string

const (
	// DiscrepancyNone 无差异（匹配成功）。
	DiscrepancyNone ReconcileDiscrepancyType = "NONE"
	// DiscrepancyUnmatched 回执在账本中找不到对应流水。
	DiscrepancyUnmatched ReconcileDiscrepancyType = "UNMATCHED_RECEIPT"
	// DiscrepancyMissingReceipt 账本流水没有对应回执（外部少回）。
	DiscrepancyMissingReceipt ReconcileDiscrepancyType = "MISSING_RECEIPT"
	// DiscrepancyAmountMismatch 金额不符。
	DiscrepancyAmountMismatch ReconcileDiscrepancyType = "AMOUNT_MISMATCH"
	// DiscrepancyCurrencyMismatch 币种不符。
	DiscrepancyCurrencyMismatch ReconcileDiscrepancyType = "CURRENCY_MISMATCH"
	// DiscrepancyDuplicateReceipt 同一流水收到多份回执。
	DiscrepancyDuplicateReceipt ReconcileDiscrepancyType = "DUPLICATE_RECEIPT"
)

// ReconcileError 对账差异错误，携带差异类型与上下文。
type ReconcileError struct {
	Type    ReconcileDiscrepancyType
	Receipt *SettlementReceipt
	EntryID string
	Detail  string
}

func (e *ReconcileError) Error() string {
	rid := ""
	if e.Receipt != nil {
		rid = e.Receipt.ReceiptID
	}
	return fmt.Sprintf("reconcile discrepancy %s: receipt=%s entry=%s detail=%s", e.Type, rid, e.EntryID, e.Detail)
}
