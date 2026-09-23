package ledger

import "errors"

// ErrorKind 是可区分的拒绝/异常原因分类。
type ErrorKind string

const (
	KindUnknown             ErrorKind = "unknown"
	KindInvalidArgument     ErrorKind = "invalid_argument"
	KindAccountNotFound     ErrorKind = "account_not_found"
	KindAccountClosed       ErrorKind = "account_closed"
	KindAccountFrozen       ErrorKind = "account_frozen"
	KindInsufficientFunds   ErrorKind = "insufficient_funds"
	KindCurrencyMismatch    ErrorKind = "currency_mismatch"
	KindIdempotencyConflict ErrorKind = "idempotency_conflict"
)

// Error 是账本层统一错误，携带可程序化判别的 Kind。
type Error struct {
	Kind    ErrorKind
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

func NewError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Message: msg}
}

func wrapError(kind ErrorKind, msg string, cause error) *Error {
	return &Error{Kind: kind, Message: msg, Cause: cause}
}

// KindOf 返回错误对应的分类，非账本错误归为 KindUnknown。
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var le *Error
	if errors.As(err, &le) {
		return le.Kind
	}
	return KindUnknown
}
