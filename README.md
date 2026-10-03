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
  recon/         对账：外部回执比对、差异分类、重试幂等、分次累积结算
    model.go     Receipt / Discrepancy / Report / BusinessResult / BatchSummary
    state.go     bizState：业务单跨批次累积状态与结论演进（结清/部分/超收）
    reconciler.go Reconciler：回执判定 + 分次累积 + 批次留痕与查询
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

### 对账差异分类（recon.DiscrepancyType，回执级）

每张回执自上而下首个命中即回执级结论：

1. `duplicate_receipt`：`ReceiptID` 历史批次或本批次已处理 →
   回放首次结论（记录里携带首次结论，如 `first conclusion match_ok`），
   **重放不改变任何金额累积**；
2. `unmatched_receipt`：回执的 `BusinessID` 在账本无对应凭证；
3. `currency_mismatch`：凭证币种 != 回执币种（该回执不参与金额累积）；
4. 币种正确的回执进入**分次累积**（见下一节），可能产生：
   - `amount_mismatch`：业务单的**第一张有效回执**金额就超过账本金额
     （上一轮单笔模式的兼容结论）；
   - `over_received`：分次累积后超过账本金额，或**已结清之后又到达新流水**
     （多余的重复结算）；
   - 无差异：回执计入正常结算（出现在 `Report.Matched`）。
5. 批次内反向核对：账本有凭证但**从未在任何批次收到过回执**
   → `unmatched_entry`。

### 分次结算：业务单级唯一结论（recon.BusinessStatus）

同一业务单支持**分多次到账**：每笔到账带自己的外部结算流水号
（`ReceiptID`），可以**乱序、跨批次**陆续进入。Reconciler 为每个业务单
维护跨批次累积状态，按流水号逐笔累加（`bizState`）：

| 业务单结论 | 定义 |
| --- | --- |
| `unseen` | 账本有凭证，但任何批次都没收到过回执（对应 `unmatched_entry`） |
| `partial_settled` | 收到过 ≥1 张币种正确的有效回执，累计金额 **<** 账本金额 |
| `settled` | 有效回执累计金额 **恰好 ==** 账本金额（与到账笔数、顺序、批次无关） |
| `over_received` | 单笔或累计金额 **>** 账本金额，或结清后又到达新流水；**多出的部分不计入正常结算** |
| `currency_mismatch` | 收到过币种与凭证不一致的回执（粘性，正常后也保留异常痕迹） |

累积与结论演进规则：

- 一笔到账只在“账本金额 − 已累计金额”的剩余额度内计入正常结算；
  超过部分进入 `OverflowAmount`，同时把该流水记入 `ExtraReceiptIDs`。
- **超收是终态**：一旦超过账本金额，后续流水只增加超额，
  结论不会回落到结清或部分结算。
- **结清是终态**：结清后再收到的任何**新流水号**一律记为
  `over_received`（多余的重复结算），不能二次结清，本金累计保持不变。
- 同一个 `ReceiptID` 的重放/补传/重放旧回执只产生 `duplicate_receipt`，
  不二次累计；结论**只向更完整的方向演进**（未收到 → 部分 → 结清），
  已经结清不会因重复推送变回未结清。
- 一旦某业务单收到过**任何**回执（哪怕金额/币种对不上），
  它就永远不再被当成 `unmatched_entry`；补上之前漏掉的到账后，
  “没有回执”的结论随之消除。
- 同一业务单在单次对账结果（一个 `Report`）里**恰好出现一次**
  （`Report.BusinessResults`），不会既被判异常又被判没收到回执。

`BusinessResult` 里同时给出当前结论、账本金额/币种、累计金额、
逐笔流水明细（`Receipts`：哪些流水号各贡献多少，按实际到达顺序）、
超额金额与多余流水号，可完整解释“金额是怎么累积出来的”。

### 批次留痕与查询

- `ReconcileBatch(ctx, batchID, receipts)`：显式批次号；
  `Reconcile(ctx, receipts)` 自动生成 `batch-N`。
- 每个批次留存一份不可变快照（`Report`）：回执级差异、
  业务单结论列表与汇总 `BatchSummary`
  （结清 / 部分结算 / 超收 / 币种不符 / 无回执各多少）。
- **同一批次号重跑直接回放首次报告**，不重新判定、不重复产生差异记录、
  不污染累积状态。
- 查询接口：`r.Batch(id)` 取某批留痕、`r.ListBatches()` 列全部批次、
  `r.Business(ctx, id)` 查业务单跨批次的当前结论与累计明细；
  `report.Business(id)` 查该批视角的结论。

### 与上一轮单笔对账的兼容范围

- 回执级五类结论（含 `duplicate_receipt` 回放、`unmatched_receipt`、
  `currency_mismatch`、`unmatched_entry` 反向核对、`Report.Matched` 与
  `HasDiff()`）语义保持不变，原有四个用例全部继续通过。
- 一笔业务单一张足额回执仍然是 `match_ok` / 业务级 `settled`，
  与旧行为完全一致。
- 唯一的行为扩充点：旧版对“金额对不上”统一打 `amount_mismatch`；
  现在业务级会进一步区分**分次累积语义**——
  - 业务单**第一张有效回执**就超过账本金额：回执级仍保留
    `amount_mismatch`（旧用例依赖），但业务级结论为 `over_received`，
    多出部分不计入正常结算；
  - 多张回执累积后超出，或结清后又来流水：记为新类型 `over_received`；
  - 第一张有效回执金额**小于**账本金额：这是合法的部分到账，
    业务级为 `partial_settled`，不算差异。

### 性能

对账是 map 查找与逐笔累加，单次对账工作量约为
O(账本凭证数 + 本批回执数)，与**历史已处理回执数量无关**
（`processed` / `businesses` 只做 O(1) 命中查询）。
`TestScaleLinearWithBatch` 在 4 万账本凭证、单批 1 万回执的量级下验证：
堆积 3 万历史回执后再跑 1 万张，耗时与冷跑同批基本一致。

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

`internal/recon/reconciler_test.go`（上一轮单笔行为，保持兼容）
- `TestReconcileAllMatched`：全部匹配无差异；
- `TestReconcileDiscrepancyKinds`：金额/币种/未匹配/重复四类异常同批可区分；
- `TestReconcileRetryIdempotent`：失败重试回放首次结论、账本不变；
- `TestReconcileMissingEntry`：部分回执丢失 → unmatched_entry，补传后消除。

`internal/recon/accumulation_test.go`（分次结算新场景）
- `TestAccumulatePartialToSettledCrossBatch`：分两次到账，部分结算 → 跨批结清，
  部分结算不得被当成没有回执；
- `TestAccumulateOutOfOrder`：批内乱序 + 跨批次分次都能正确累积，流水按到达顺序留痕；
- `TestOverReceivedSplitAndSticky`：分次累积超额（多出部分不计）、超收粘性、
  首张即超额的兼容结论；
- `TestSettledThenExtraSerial`：结清后再来新流水按多余重复结算，不二次结清；
- `TestDuplicatePushNotCounted`：批内/跨批重复推送不重复累积、不重复产生差异；
- `TestNoContradictoryConclusion`：同一业务单同一批只有一个结论，
  异常单不会同时被标成没有回执（含后续空批次）；
- `TestBatchTraceAndRerunIdempotent`：批次留痕、按批次/业务单查询、
  同批重放回放首次报告且不污染累积；
- `TestScaleLinearWithBatch`：4 万凭证/单批 1 万回执量级，
  耗时随批规模线性、与历史堆积无关；
- `TestConcurrentAccumulation`：race 下并发分批与重放，最终恰好结清、每笔只计一次。

日志中会打印：请求输入、错误分类、幂等判定依据（单号命中/指纹冲突）、
每条对账差异的类型与明细、转账前后按币种汇总的资金总额；
分次对账日志还会逐张打印输入回执、累积/判重规则、哪些回执计入正常结算、
每个业务单的唯一结论与逐笔流水累计明细、批次汇总计数。

## 五、边界与说明

- 存储为进程内内存实现（`MemoryStore`），用于本地模拟与验证；
  `Store` / `LedgerView` 为接口，可替换为磁盘或数据库实现，
  替换时需保持“幂等检查与入账同临界区/同事务”的语义。
- 对账器的已处理回执表、业务单累积状态与批次留痕同样只保存在进程内存中
  （`Reconciler` 内部 map）；替换为持久化存储时需保持
  “回执首次结论不可变、同批次重跑回放、业务单结论只向更完整演进”的语义。
- 转账限定同币种；跨币种属于换汇业务，需显式汇率与第四条凭证，
  不在本实现范围内。
