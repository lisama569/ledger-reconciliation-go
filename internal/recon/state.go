package recon

// bizState 单个业务单跨批次累积的结算状态（Reconciler 持久持有）。
// 所有字段只允许在 Reconciler.mu 临界区内读写。
type bizState struct {
	businessID   string
	currency     string
	ledgerAmount int64

	// currencyMismatch 一旦收到过币种不符的回执即为 true（粘性）。
	currencyMismatch bool

	// settledAmount 已计入“正常结算”的累计金额；
	// 超收多出的部分绝不计入这里。
	settledAmount int64
	// receipts 构成 settledAmount 的逐笔到账流水（按首次到达顺序）。
	receipts []SettledReceipt

	// overflow 超收总额（结清后又到账 / 累计超过账本金额的部分）。
	overflow int64
	// extraIDs 造成超收或结清后多余的流水号（按到达顺序）。
	extraIDs []string
}

// status 依据累积状态推导业务单当前唯一结论。
func (s *bizState) status() BusinessStatus {
	switch {
	case s.overflow > 0:
		// 超收是终态：一旦累计超过账本金额，后续不会回落到结清/部分。
		return StatusOverReceived
	case s.settledAmount == s.ledgerAmount && len(s.receipts) > 0:
		// 结清是终态：结清后再到新流水只增加 overflow，不改这里。
		return StatusSettled
	case s.currencyMismatch:
		return StatusCurrencyMismatch
	case len(s.receipts) > 0:
		return StatusPartial
	default:
		return StatusUnseen
	}
}

// applyValidReceipt 处理一张币种正确的新到账回执，返回该回执的判定。
//
// counted：本笔计入正常结算的金额（超收部分不计）；
// overflow：本笔中超出账本金额的部分；
// discrepancy：该回执应产生的差异类型（MatchOK 表示无差异）。
func (s *bizState) applyValidReceipt(receiptID string, amount int64) (counted, overflowAmt int64, discrepancy DiscrepancyType) {
	current := s.status()

	// 已结清：再来的任何新流水一律按多余的重复结算处理，不二次结清。
	if current == StatusSettled {
		s.overflow += amount
		s.extraIDs = append(s.extraIDs, receiptID)
		return 0, amount, OverReceived
	}
	// 已超收：后续流水同样只进超额区。
	if current == StatusOverReceived {
		s.overflow += amount
		s.extraIDs = append(s.extraIDs, receiptID)
		return 0, amount, OverReceived
	}

	room := s.ledgerAmount - s.settledAmount
	switch {
	case amount <= room:
		// 未超：部分 -> 继续累积；恰好补齐 -> 结清。乱序、跨批次同样适用。
		s.settledAmount += amount
		s.receipts = append(s.receipts, SettledReceipt{ReceiptID: receiptID, Amount: amount})
		return amount, 0, MatchOK
	case len(s.receipts) == 0:
		// 兼容单笔模式：业务单的第一张有效回执就超过账本金额，
		// 回执级结论保留 amount_mismatch（上一轮语义），
		// 业务级结论为 over_received，多出部分不计入正常结算。
		s.settledAmount = s.ledgerAmount
		s.receipts = append(s.receipts, SettledReceipt{ReceiptID: receiptID, Amount: s.ledgerAmount})
		s.overflow = amount - room
		s.extraIDs = append(s.extraIDs, receiptID)
		return room, amount - room, AmountMismatch
	default:
		// 分次累积后超过账本金额：本笔只计入剩余额度，其余识别为超收。
		s.settledAmount += room
		s.receipts = append(s.receipts, SettledReceipt{ReceiptID: receiptID, Amount: room})
		s.overflow = amount - room
		s.extraIDs = append(s.extraIDs, receiptID)
		return room, amount - room, OverReceived
	}
}

// markCurrencyMismatch 记录一张币种不符的回执。
func (s *bizState) markCurrencyMismatch(receiptID string) {
	s.currencyMismatch = true
	s.extraIDs = append(s.extraIDs, receiptID)
}

// toResult 生成业务单结论快照。
func (s *bizState) toResult(detail string) BusinessResult {
	return BusinessResult{
		BusinessID:      s.businessID,
		Status:          s.status(),
		LedgerAmount:    s.ledgerAmount,
		Currency:        s.currency,
		SettledAmount:   s.settledAmount,
		Receipts:        append([]SettledReceipt(nil), s.receipts...),
		OverflowAmount:  s.overflow,
		ExtraReceiptIDs: append([]string(nil), s.extraIDs...),
		Detail:          detail,
	}
}
