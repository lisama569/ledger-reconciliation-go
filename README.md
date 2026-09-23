# 本地双分录账本与结算对账（Go）

用本地内存存储模拟账户、流水与外部结算回执，实现一套可并发、可幂等、
可解释差异的双分录账本与结算对账流程。所有金额以最小货币单位的 **整数**
表示（如人民币“分”），杜绝浮点误差。

## 目录结构

```
internal/
  ledger/        账本：账户、双分录凭证、事务存储、幂等转账
    model.go     领域类型（Account / Entry / Transaction / TransferRequest…）
    errors.go    统一错误与可程序化判别的错误分类 ErrorKind
    store.go     Store 接口 + MemoryStore（map + 单锁 + 快照回滚事务）
    service.go   Service：开户、转账、状态管理、余额汇总
  recon/         对账：外部回执比对、差异分类、重试幂等
    model.go     Receipt / Discrepancy / Report
    reconciler.go Reconciler：差异判定顺序 + 已处理回执回放
    adapter.go   ledger.Service → recon.LedgerView 适配
```

## 一、账本模型

### 账户（Account）
- `ID`、`Currency`、`Balance`（int64 最小货币单位）、`Status`、`Version`。
- 状态：`active`（正常）/ `frozen`（冻结）/ `closed`（关闭）。

### 双分录（Entry / Transaction）
每笔转账生成一张凭证（`Transaction`），包含恰好两条金额相等的分录：

| 分录 | 账户 | 方向 |
| --- | ---- | ---- |
| 收款方（to） | 资产增加 | **借 debit** |
| 付款方（from） | 资产减少 | **贷 credit** |

存储层在提交前强制校验：
1. 分录数 ≥ 2；借方金额合计 == 贷方金额合计（借贷平衡）；
2. 余额更新只涉及分录引用的账户，全部变动净额 == 0；
3. 每条分录金额必须为正。

因此 **任何一笔入账前后，系统内资金总额守恒**。守恒指标可通过
`Service.TotalBalance(ctx, ccy)` 按币种汇总验证。

### 事务与原子性（无半笔入账）
`MemoryStore.Begin` 持有进程内单一互斥锁：
- 进入事务前对账户表、幂等表、凭证表做深拷贝快照；
- 回调返回任何 error 或 panic → 整体恢复快照（回滚），外部观察不到中间态；
- 回调成功 → 一次提交。

幂等检查与账户读改写在**同一临界区**内完成，从根本上消除
“检查与入账之间被插入”的 TOCTOU 竞争。

## 二、幂等与并发规则

- 幂等键：`TransferRequest.BusinessID`（业务单号）。
- 提交时在事务内先查幂等表：
  - **未命中** → 校验账户/状态/币种/余额，构造双分录并提交；
  - **命中且语义指纹相同** → 不再次入账，直接回放首次结果，
    返回 `TransferResult{Duplicated: true}`，字段与首次完全一致；
  - **命中但语义指纹不同** → 返回 `idempotency_conflict`，
    原记录保留，不覆盖、不入账。
- 语义指纹：`币种 | 转出方 -> 收款方 | 金额`
  （`Reference` 等描述性字段不参与，避免格式差异误判冲突）。
- 并发提交同一单号：锁串行化后恰有 1 次入账，其余全部幂等回放，
  全部调用方拿到一致结果（见 `TestConcurrentSameBusinessID`）。

## 三、错误分类

账本错误统一为 `*ledger.Error`，用 `ledger.KindOf(err)` 程序化判别：

| ErrorKind | 触发条件 |
| --- | --- |
| `invalid_argument` | 单号为空、金额非正、币种为空、收付款方相同/为空 |
| `account_not_found` | 转出或转入账户不存在 |
| `account_closed` | 任一账户已关闭（判定优先于 frozen） |
| `account_frozen` | 任一账户被冻结 |
| `currency_mismatch` | 账户币种与请求币种不一致 |
| `insufficient_funds` | 转出方余额 < 转账金额 |
| `idempotency_conflict` | 同一业务单号复用于不同语义的请求 |

拒绝发生在 `Apply` 之前或事务内部，一律回滚，**不会产生半笔入账**，
拒绝前后总余额保持不变。

### 对账差异分类（recon.DiscrepancyType）

判定顺序（每张回执自上而下，首个命中即结论）：

1. `duplicate_receipt`：`ReceiptID` 历史批次或本批次已处理 →
   回放首次结论（记录里携带首次结论，如 `first conclusion amount_mismatch`）；
2. `unmatched_receipt`：回执的 `BusinessID` 在账本无对应凭证；
3. `currency_mismatch`：凭证币种 != 回执币种；
4. `amount_mismatch`：凭证金额 != 回执金额；
5. `match_ok`：账实一致。

批次内还会反向核对：账本存在但本批无回执、且历史从未匹配成功的凭证，
标记为 `unmatched_entry`（提示结算漏记/部分失败），防止“部分回执丢失”
被静默掩盖。

**重试语义**：Reconciler 本地维护 `processed[receiptID] → 首次结论`。
部分失败后重放整批时，已处理回执只产生一条 duplicate 回放，
不会重复记账、不会重复产生原始差异；新回执正常判定，
补传成功后对应凭证不再出现在 `unmatched_entry` 中。

## 四、本地验证方法

```bash
# 构建
go build ./...

# 静态检查
go vet ./...

# 全部单测
go test ./...

# 并发安全（必跑；并发用例在 race 下验证）
go test -race -count=1 ./...

# 查看带输入 / 幂等判定依据 / 对账差异的详细日志
go test -v -race ./...
```

### 关键测试场景

`internal/ledger/service_test.go`
- `TestTransferConservationAndResult`：双分录结构与资金守恒；
- `TestTransferErrorKinds`：各拒绝原因可区分，拒绝后总额不变；
- `TestAccountStatusRejected`：冻结/关闭账户被拒；
- `TestIdempotentDuplicate`：重复提交只入账一次、结果一致；
- `TestIdempotencyConflict`：同号不同语义判冲突，不覆盖；
- `TestConcurrentSameBusinessID`：64 并发同单号 → 1 次入账 + 63 次回放；
- `TestConcurrentDistinctTransfers`：200 笔并发互转，总额守恒；
- `TestRejectedTransferNoHalfEntry`：余额竞争下只成功 1 笔，无半笔。

`internal/recon/reconciler_test.go`
- `TestReconcileAllMatched`：全部匹配无差异；
- `TestReconcileDiscrepancyKinds`：金额/币种/未匹配/重复四类异常同批可区分；
- `TestReconcileRetryIdempotent`：失败重试回放首次结论、账本不变；
- `TestReconcileMissingEntry`：部分回执丢失 → unmatched_entry，补传后消除。

日志中会打印：请求输入、错误分类、幂等判定依据（单号命中/指纹冲突）、
每条对账差异的类型与明细、转账前后按币种汇总的资金总额。

## 五、边界与说明

- 存储为进程内内存实现（`MemoryStore`），用于本地模拟与验证；
  `Store` / `LedgerView` 为接口，可替换为磁盘或数据库实现，
  替换时需保持“幂等检查与入账同临界区/同事务”的语义。
- 转账限定同币种；跨币种属于换汇业务，需显式汇率与第四条凭证，
  不在本实现范围内。
