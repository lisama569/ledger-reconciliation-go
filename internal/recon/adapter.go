package recon

import (
	"context"

	"github.com/lisama569/ledger-reconciliation-go/internal/ledger"
)

// LedgerServiceView 适配 ledger.Service 到 LedgerView。
type LedgerServiceView struct {
	Svc *ledger.Service
}

// ListTransactions 读取账本全部凭证并转换为对账视图。
func (v LedgerServiceView) ListTransactions(ctx context.Context) ([]LedgerTx, error) {
	txns, err := v.Svc.Transactions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LedgerTx, 0, len(txns))
	for _, t := range txns {
		// 双分录借贷金额相等，取任一分录金额即为单边发生额。
		var amount int64
		var ccy string
		for _, e := range t.Entries {
			if e.Direction == ledger.Debit {
				amount = e.Amount
				ccy = string(e.Currency)
				break
			}
		}
		out = append(out, LedgerTx{
			BusinessID: t.BusinessID,
			Currency:   ccy,
			Amount:     amount,
		})
	}
	return out, nil
}
