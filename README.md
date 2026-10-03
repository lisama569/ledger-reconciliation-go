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
    reconciler.go Reconciler：单笔回执差异判定顺序 + 已处理回执回放
    settlement.go SettlementReconciler：分次到账累积、跨批次/乱序、批次留痕
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

## 三点五、分次结算对账（recon.SettlementReconciler）

单笔对账假设“一行业务 ↔ 一张回执”。实际结算经常**分多次打款、乱序、
跨批次到账**，因此在 Reconciler 之上再加一层累积式对账器（`settlement.go`）。
两层并存，调用方按需选用（兼容范围见本节末尾）。

### 业务单唯一结论（SettlementStatus）

同一业务单在同一次对账结果里**恰好一个结论**，绝不自相矛盾：

| 结论 | 定义 |
| --- | --- |
| `unsettled` | 账本有凭证，但从未收到任何回执（反向核对得出） |
| `partial_settled` | 已收到币种正确的到账，且 **累积金额 < 账本金额** |
| `settled` | 币种正确的到账**累积金额恰好 == 账本金额**（结清，终态） |
| `over_received` | 单笔或累积到账 **> 账本金额**；超出部分（`OverAmount()`）不计入正常结算，不算结清 |
| `currency_mismatch` | 只收到过币种不符的回执，尚无币种正确的到账 |

金额对不上（部分/超收）、币种对不上、查不到业务单是三类互斥的异常，
其中“查无此单”是回执级问题（`unmatched_receipt`），不挂业务单结论；
同一业务单不会既被标异常、又被标成没收到回执。

### 累积与判重规则

- **以外部结算流水号（ReceiptID）判重**：历史批次或同批次出现过的流水号
  只回放，不再次累积。判重先于金额/币种/累积判定。
- **币种正确才累积**，累加与输入顺序无关：乱序到账、跨批次到账结论一致。
- **结论只向更完整的方向演进**：
  `unsettled → currency_mismatch/partial_settled → settled`；
  已收到过回执（哪怕币种不符）后，后续批次不再报 `unsettled`；
  补传漏掉的到账后，原来的“没有回执”消失；
  **已结清是终态**，不会因重复推送退回未结清。
- **结清后再收到新流水号**：记为回执级问题 `redundant_settlement`
  （多余的重复结算），不重新结清、不计入正常金额。
- 每张回执的问题挂在对应业务单的 `Issues` 上（查无此单的进
  `OrphanIssues`），业务单结论本身仍然唯一。

### 批次留痕与查询

- `ReconcileBatch(ctx, batchID, receipts)`：每批产出 `BatchReport`，
  含汇总（结清/部分/超收/币种不符/未到账各多少、各类回执问题多少）与
  每单的本批结论、本批新到账明细。
- `Business(businessID)`：查业务单**当前**结论与金额累积过程
  （每张到账的流水号、所属批次、金额时间，以及历史问题）。
- `Batch(batchID)`：按批次号回查历史留痕；**同一 batchID 重跑返回首次
  留痕**（`Replayed=true`），不重复产生差异记录。

### 规模特性

账本视图只在首次对账（或显式 `SyncLedger`）时全量读取一次建立索引；
反向核对只遍历“从未收到回执”的待结清单（`pending`，收到任意回执即移除），
不随历史数据增长而全量扫描。单批耗时与该批回执规模大致成正比：
3 万张回执约 180ms（见 `TestSettlementScale`），同规模后续批次不随历史变慢。

### 与上一轮单笔对账的兼容范围

- 旧 `Reconciler` 及其类型、测试**原样保留**，行为不变；
  新能力全部在独立的 `SettlementReconciler` 中。
- 单笔即全额到账的旧场景，在新对账器中一张回执直接累积为 `settled`，
  语义等价于旧的 `match_ok`；旧报告用 `DiscrepancyType`，新报告用
  `SettlementStatus` + 回执级 `ReceiptIssueType`，两套结论模型不混用。
- 两者共用 `LedgerView` 接口与 `Receipt` 结构，可接同一份账本数据。

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

`internal/recon/settlement_test.go`（分次结算新场景）
- `TestSettlementPartialAccumulatesToSettled`：分次累积，部分→补齐→结清；
- `TestSettlementOutOfOrderAcrossBatches`：乱序、跨批次累积，结论与顺序无关；
- `TestSettlementOverReceived`：单笔/累积超收，超出部分不计入正常结算；
- `TestSettlementDuplicateSerialNotCounted`：重复流水号不重复累积/计差异，
  同 batchID 重放返回首次留痕；
- `TestSettlementSingleConclusionNoContradiction`：同一业务单同批唯一结论，
  收到过回执后不再被标“没有回执”，补传后结论只向更完整演进；
- `TestSettlementRedundantAfterSettled`：结清后新流水号按多余重复结算处理；
- `TestSettlementUnmatchedReceiptTracked`：查无此单及其重放只作孤儿问题；
- `TestSettlementScale`：3 万回执 ~180ms，同规模批次不随历史累积变慢。

日志中会打印：请求输入、错误分类、幂等判定依据（单号命中/指纹冲突）、
每条对账差异的类型与明细、每批的汇总计数、每个业务单落在哪个结论上、
金额按什么规则累积（每张流水计入多少、累积到多少）、转账前后按币种汇总的
资金总额。

## 五、边界与说明

- 存储为进程内内存实现（`MemoryStore`），用于本地模拟与验证；
  `Store` / `LedgerView` 为接口，可替换为磁盘或数据库实现，
  替换时需保持“幂等检查与入账同临界区/同事务”的语义。
- 转账限定同币种；跨币种属于换汇业务，需显式汇率与第四条凭证，
  不在本实现范围内。
