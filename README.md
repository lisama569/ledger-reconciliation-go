# 本地双分录账本与结算对账（Go）

一个纯本地内存实现的小型账本：模拟账户、双分录流水、以业务单号为键的
幂等提交，以及外部结算回执的对账与差异分类。目标是让**并发入账、重复
提交、结算差异**下的账务状态可预期、可解释。

无任何第三方依赖，仅用标准库；Go >= 1.25。

## 目录结构

```
ledger/
  money.go        金额值对象（整数最小货币单位 + 币种）
  errors.go       业务错误码与对账差异类型
  account.go      账户、账户状态、账户存储（余额借贷原语）
  entry.go        分录/事务模型与流水存储
  idempotency.go  幂等请求、语义指纹、幂等记录存储
  ledger.go       账本核心：开户、原子幂等转账、守恒视图
  reconcile.go    回执存储、对账器与差异分类
  *_test.go       覆盖并发/重复/冲突/异常回执的测试
```

## 账本模型

- `Account`：账户，按币种分别保存余额（`map[currency]int64`，最小货币单位
  整数，杜绝浮点误差）。账户有三种状态：`ACTIVE / FROZEN / CLOSED`。
- `Transaction`：一笔转账事务，恰好包含两条 `Entry`：
  - 转出方一条 `DEBIT`（借，余额减少）；
  - 转入方一条 `CREDIT`（贷，余额增加）；
  - 两条分录金额相等、币种相同，同成同败。
- **资金守恒**：转账只在账户间搬运资金，系统内每个币种的余额总额
  （`Ledger.TotalFunds`）在任意入账前后不变。测试在每轮操作前后断言这一点。
- 落账顺序为“先借后贷”：先做转出方余额检查与扣减，余额不足立即整体放弃，
  再做转入方加记，因此**不会出现半笔入账**。

## 幂等与并发规则

- 幂等键是**业务单号 `BizID`**。`IdempotencyStore` 记录每个 BizID 首次请求的
  **语义指纹**：`fromAccount | toAccount | amount | currency`。
- 每次 `Transfer` 先查幂等表，判定结果有三种（`Decision` 会随结果返回并在测试日志中打印）：
  | 判定 | 含义 | 行为 |
  | --- | --- | --- |
  | `NEW` | 首次见此 BizID | 校验并落账，写入幂等记录 |
  | `REPLAY` | 同 BizID 且指纹相同 | 不重复记账，原样返回首次的 `TxnID` 与结果 |
  | `CONFLICT` | 同 BizID 但指纹不同 | 返回 `IDEMPOTENCY_CONFLICT`，**不静默覆盖**，不产生任何账务效果 |
- 并发：账本写操作由单一互斥锁串行化，幂等判定、余额校验、双分录落账、
  写流水与幂等记录在同一个临界区内完成。因此并发提交：
  - 不同 BizID：全部成功，无丢失、无重复；
  - 同一 BizID：恰好入账一次，所有并发调用拿到同一个 `TxnID`。
- 拒绝类错误在幂等判定之后、落账之前返回，余额与流水均不变；
  只有成功入账才写幂等记录（失败可安全重试）。

## 错误分类（可区分的拒绝原因）

使用哨兵错误 + 错误码，调用方用 `errors.Is` 判断：

| 错误 | 错误码 | 场景 |
| --- | --- | --- |
| `ErrInsufficientBalance` | `INSUFFICIENT_BALANCE` | 转出余额不足，整笔放弃 |
| `ErrInvalidAmount` | `INVALID_AMOUNT` | 金额 <= 0 |
| `ErrSameAccount` | `SAME_ACCOUNT` | 收付账户相同 |
| `ErrAccountNotFound` | `ACCOUNT_NOT_FOUND` | 账户不存在 |
| `ErrAccountFrozen` | `ACCOUNT_FROZEN` | 账户被冻结 |
| `ErrAccountClosed` | `ACCOUNT_CLOSED` | 账户已关闭 |
| `ErrCurrencyMismatch` | `CURRENCY_MISMATCH` | 账户无该币种轨迹（入账阶段币种校验） |
| `ErrIdempotencyConflict` | `IDEMPOTENCY_CONFLICT` | 同 BizID 复用但语义不同 |

## 结算对账与差异分类

`Reconciler` 以账本流水为准，与 `ReceiptStore` 中模拟的外部回执逐笔核对。
对账对账本**只读**：任何回执异常都不改动余额与流水，因此重试或部分失败
不会导致账本重复记账或漏记。

差异类型（`ReconcileError.Type`，测试日志逐条打印输入回执与差异说明）：

| 差异类型 | 含义 |
| --- | --- |
| `UNMATCHED_RECEIPT` | 回执 BizID 在账本中找不到流水 |
| `AMOUNT_MISMATCH` | 回执金额与流水不一致 |
| `CURRENCY_MISMATCH` | 回执币种与流水不一致 |
| `DUPLICATE_RECEIPT` | 同一回执重投，或同一 BizID 出现第二份回执 |
| `MISSING_RECEIPT` | 账本有流水但没有任何回执（外部漏回） |

消费规则：

- 只有 BizID、金额、币种三者完全一致的回执才标记为已消费；
- 异常回执不写入任何消费状态，修正后重投会在下一轮被正常消费，可安全重试；
- 已消费回执再次送达时，每轮对账仍会如实地标记为 `DUPLICATE_RECEIPT`，
  而不是静默丢弃；
- 有异常回执（金额/币种不符）的流水不再重复报 `MISSING_RECEIPT`，
  保证一笔业务只有一个归因。

## 本地验证方法

```bash
# 构建与静态检查
go build ./...
go vet ./...

# 全量单测
go test -v ./...

# 并发场景建议带竞态检测
go test -race -v ./...

# 只跑某一类场景
go test -run TestConcurrentTransfer -v ./...
go test -run TestReconcile -v ./...
```

测试日志中会打印：

- **输入**：每笔转账/回执的业务单号、收付账户、金额、币种；
- **幂等判定依据**：`NEW / REPLAY / CONFLICT`、指纹比对结果与 `TxnID`；
- **对账差异**：差异类型、关联回执、流水 ID 与具体不一致字段。

覆盖场景：

1. 正常双分录转账与资金总额守恒；
2. 余额不足/非法金额/同账户/账户不存在/冻结/关闭等可区分拒绝，且无半笔入账；
3. 同单据串行与并发重复提交：只入账一次、返回结果一致；
4. 同 BizID 语义冲突：返回冲突错误且不覆盖原记录；
5. 30 个 goroutine 并发独立单据 + 20 个 goroutine 并发撞同一单据；
6. 对账：正常匹配、未匹配、金额不符、币种不符、重复回执、缺回执；
7. 对账部分失败后修正重投、再重跑：账本始终不重记、不漏记。
