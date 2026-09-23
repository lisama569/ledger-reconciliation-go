package ledger

import (
	"context"
	"strconv"
	"time"
)

// Service 账本服务：账户开户与双分录转账。
type Service struct {
	store Store
}

// NewService 创建账本服务。
func NewService(store Store) *Service { return &Service{store: store} }

// OpenAccount 开立账户。id/币种不可为空，初始余额不可为负。
func (s *Service) OpenAccount(ctx context.Context, id string, ccy Currency, initial int64) (*Account, error) {
	return s.store.CreateAccount(ctx, Account{
		ID:        id,
		Currency:  ccy,
		Balance:   initial,
		Status:    StatusActive,
		CreatedAt: time.Now(),
	})
}

// Transfer 执行一笔双分录转账（按 BusinessID 幂等）。
//
// 拒绝原因（Kind）按以下固定优先级判定，保证并发/重试下可预期：
//  1. invalid_argument     单号/账户为空、金额非正
//  2. account_not_found     任一账户不存在
//  3. account_closed        任一账户已关闭（关闭优先级高于冻结）
//  4. account_frozen        任一账户被冻结
//  5. currency_mismatch     账户币种与请求币种不一致
//  6. insufficient_funds    借方（转出方）余额不足
//  7. idempotency_conflict  同一 BusinessID 但业务语义指纹不同
//
// 同一 BusinessID + 同一语义：无论重复或并发提交多少次，只入账一次，
// 后续调用返回首次结果（Duplicated=true）。
func (s *Service) Transfer(ctx context.Context, req TransferRequest) (*TransferResult, error) {
	if req.BusinessID == "" {
		return nil, NewError(KindInvalidArgument, "business id is empty")
	}
	if req.Amount <= 0 {
		return nil, NewError(KindInvalidArgument, "transfer amount must be positive")
	}
	if req.Currency == "" {
		return nil, NewError(KindInvalidArgument, "currency is empty")
	}
	if req.FromID == "" || req.ToID == "" {
		return nil, NewError(KindInvalidArgument, "from/to account id is empty")
	}
	if req.FromID == req.ToID {
		return nil, NewError(KindInvalidArgument, "cannot transfer to the same account")
	}

	result := &TransferResult{}
	err := s.store.Begin(ctx, func(tx Tx) error {
		// 1) 幂等判定：检查必须与入账在同一临界区内，
		//    并发提交时只有一个请求能越过这一步。
		if prev, ok := tx.GetTransfer(req.BusinessID); ok {
			want := reqFingerprint(req)
			if prev.fingerprint() != want {
				return NewError(KindIdempotencyConflict,
					"business id "+req.BusinessID+" reused with different request: "+
						"stored=["+prev.fingerprint()+"] incoming=["+want+"]")
			}
			prev.Duplicated = true
			*result = prev
			return nil
		}

		// 2) 账户存在性
		from, err := tx.GetAccount(req.FromID)
		if err != nil {
			return err
		}
		to, err := tx.GetAccount(req.ToID)
		if err != nil {
			return err
		}

		// 3) 账户状态：closed 优先于 frozen，双方都检查
		if err := checkStatus(req.FromID, from.Status); err != nil {
			return err
		}
		if err := checkStatus(req.ToID, to.Status); err != nil {
			return err
		}

		// 4) 币种一致
		if from.Currency != req.Currency {
			return NewError(KindCurrencyMismatch,
				"debit account "+req.FromID+" currency "+string(from.Currency)+
					" does not match request "+string(req.Currency))
		}
		if to.Currency != req.Currency {
			return NewError(KindCurrencyMismatch,
				"credit account "+req.ToID+" currency "+string(to.Currency)+
					" does not match request "+string(req.Currency))
		}

		// 5) 余额充足
		if from.Balance < req.Amount {
			return NewError(KindInsufficientFunds,
				"account "+req.FromID+" balance "+strconv.FormatInt(from.Balance, 10)+
					" < amount "+strconv.FormatInt(req.Amount, 10))
		}

		// 6) 构造借贷平衡的双分录（金额以最小货币单位的整数计）
		entries := []Entry{
			{AccountID: req.ToID, Currency: req.Currency, Amount: req.Amount, Direction: Debit},
			{AccountID: req.FromID, Currency: req.Currency, Amount: req.Amount, Direction: Credit},
		}
		txn := Transaction{
			BusinessID: req.BusinessID,
			Entries:    entries,
			CreatedAt:  time.Now(),
		}
		committed := TransferResult{
			BusinessID: req.BusinessID,
			FromID:     req.FromID,
			ToID:       req.ToID,
			Currency:   req.Currency,
			Amount:     req.Amount,
			Entries:    entries,
		}
		updates := map[string]int64{
			req.FromID: -req.Amount,
			req.ToID:   +req.Amount,
		}
		if err := tx.Apply(txn, committed, updates); err != nil {
			return err
		}
		*result = committed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func checkStatus(id string, st AccountStatus) error {
	switch st {
	case StatusClosed:
		return NewError(KindAccountClosed, "account "+id+" is closed")
	case StatusFrozen:
		return NewError(KindAccountFrozen, "account "+id+" is frozen")
	case StatusActive:
		return nil
	default:
		return NewError(KindAccountClosed, "account "+id+" has invalid status "+string(st))
	}
}

// Account 查询账户快照。
func (s *Service) Account(ctx context.Context, id string) (*Account, error) {
	return s.store.GetAccount(ctx, id)
}

// SetStatus 修改账户状态（冻结/关闭/恢复）。账户不存在返回 KindAccountNotFound。
func (s *Service) SetStatus(ctx context.Context, id string, status AccountStatus) error {
	switch status {
	case StatusActive, StatusFrozen, StatusClosed:
	default:
		return NewError(KindInvalidArgument, "invalid account status "+string(status))
	}
	return s.store.Begin(ctx, func(tx Tx) error {
		a, err := tx.GetAccount(id)
		if err != nil {
			return err
		}
		a.Status = status
		return tx.ApplyStatus(a)
	})
}

// Accounts 返回全部账户（守恒校验用）。
func (s *Service) Accounts(ctx context.Context) ([]Account, error) {
	return s.store.ListAccounts(ctx)
}

// Transactions 返回全部已入账凭证（适配器/诊断用）。
func (s *Service) Transactions(ctx context.Context) ([]Transaction, error) {
	return s.store.ListTransactions(ctx)
}

// TotalBalance 按币种汇总全部账户余额（对账守恒指标）。
func (s *Service) TotalBalance(ctx context.Context, ccy Currency) (int64, error) {
	accts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, a := range accts {
		if a.Currency == ccy {
			total += a.Balance
		}
	}
	return total, nil
}
