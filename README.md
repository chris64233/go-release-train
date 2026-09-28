# go-release-train

发布列车（Release Train）服务：管理组件版本登记、组件间版本约束、候选列车的编辑/冻结/审批/取消/放行全生命周期，保证冻结快照内依赖始终一致；并在**已放行列车**之上提供跨环境（开发 → 预发布 → 生产）逐级晋级、审批门槛、失败回退与可重试的新尝试。

开发环境：Go 1.23.0，无第三方依赖。

## 环境晋级与列车放行的关系

“放行（release）”和“环境晋级（environment promotion）”是两个先后衔接、各自完整的阶段：

- **放行回答“这列车的内容对不对”**：冻结快照通过依赖校验、审批齐备后置为 `released`，写出一条 `train.released`。放行**不**代表它已经部署到任何环境。
- **环境晋级回答“这列已放行的车如何逐环境落地”**：只有 `released` 列车才能创建晋级活动；活动在创建时刻再次**冻结**三样东西——列车快照、环境严格顺序、各环境审批门槛。未放行列车、或快照中版本在登记库中已不完整时，晋级活动不能开始。
- 放行只做一次；晋级活动每列车也只创建一次。一次晋级活动内可有**多次尝试（attempt）**：某环境部署失败并回退完成后，问题修复可创建新尝试，新尝试**继续使用同一列车快照**，已经处于快照版本的环境直接继承、不重复部署、不重复写 outbox。
- 已放行列车的冻结快照不可变，因此后续策略（列车审批策略、环境定义）的修改都不会影响在途的放行与晋级。


## 核心模型与不变量

- **组件版本不可变**：`(component, version)` 一经登记即不可修改。登记时声明它对其他组件的版本约束（`=`、`>`、`>=`、`<`、`<=`，语义化版本比较）。重复登记相同内容幂等；同一版本声明不同约束返回幂等冲突。
- **候选可替换，冻结即快照**：列车在 `editing` 状态可加入、替换、移除候选组件；冻结时把候选复制为不可变快照，此后列车内容不再改变。
- **冻结原子校验**（任何一个组件不兼容，整个冻结失败，不留部分结果）：
  1. 快照内每个版本都必须已登记；
  2. 每个版本声明的每条约束，目标组件必须在快照内，且快照选用的版本满足约束；
  3. 快照内的依赖图必须**无环**（Kahn 拓扑排序检测，自环也算环）。
  所有问题一次性收集返回（422 + `problems` 列表）。
- **策略快照**：冻结时刻把当前审批策略（角色规则 + 有资格人员名单）复制进列车。审批资格只认这份快照；冻结后再改策略不影响已冻结的列车。
- **审批幂等**：同一 `(person, role)` 重复审批不报错、不重复计数；只有冻结时策略快照中有资格的人可以审批。所有规则角色的人数都满足后才能放行。
- **终态互斥**：`released` 与 `cancelled` 互为唯一终态。放行后取消、取消后放行都返回状态冲突。
- **放行幂等 + outbox 唯一**：重复放行（无论是否带相同 request id）返回首次放行的同一个结果；`released` 状态与 outbox 事件在同一事务写入，一列车至多一条 `train.released` 事件。
- **乐观并发**：列车带单调递增的 `version`，写请求可携带 `expected_version`；并发修改时过期条件返回版本冲突（409），绝不丢失更新。
- **请求幂等键**：写请求可携带 `request_id`；相同键 + 相同指纹重放原结果，相同键 + 不同内容返回幂等冲突。

### 状态机

```
                候选编辑
  editing ─────────────────┐
     │  freeze（校验通过）  │ cancel
     ▼                     │
   frozen ────cancel────► cancelled   （终态）
     │  release（审批齐）
     ▼
  released                   （终态，写 outbox 1 条）
```

`editing` 也可直接取消。终态下除"重复放行/重复取消"外的任何写操作都返回状态冲突。

## 错误分类

| 错误类别 | HTTP 状态码 | code | 场景 |
| --- | --- | --- | --- |
| `ErrInvalidArgument` | 400 | `invalid_argument` | 参数缺失、版本号格式错、策略不合法 |
| `ErrNotFound` | 404 | `not_found` | 列车/版本/outbox 事件不存在 |
| `ErrDependency` | 422 | `dependency` | 冻结校验：有环、约束不满足、候选版本未登记/缺失依赖（附 `problems`） |
| `ErrApproval` | 403 | `approval` | 无审批资格、审批角色未齐就放行 |
| `ErrStateConflict` | 409 | `state_conflict` | 冻结后编辑、终态后操作、放行/取消互斥 |
| `ErrVersionConflict` | 409 | `version_conflict` | `expected_version` 与当前版本不一致 |
| `ErrIdempotencyConflict` | 409 | `idempotency_conflict` | 版本被改写、同一 request id 内容不同 |

## 持久化

默认使用单个 JSON 文件（`-state` 指定，默认 `data/state.json`）：

- 所有写操作在一个事务内完成：先在状态副本上执行业务逻辑，任何校验失败都丢弃副本（回滚，无部分结果）；成功后整份状态经 **临时文件 + rename 原子落盘**。
- 进程重启后自动恢复：版本登记、列车（含冻结快照、策略快照、审批记录）、outbox、幂等记录。
- 传 `-state ""` 退化为纯内存存储（测试用）。

## HTTP 接口

启动：

```bash
go run ./cmd/release-train -addr :8080 -state data/state.json
```

### 审批策略

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| PUT | `/policy` | 设置当前策略（只影响之后冻结的列车） |
| GET | `/policy` | 查询当前策略 |

```json
PUT /policy
{
  "rules":     [{"role": "qa", "need": 1}, {"role": "manager", "need": 1}],
  "approvers": [{"person": "alice", "role": "qa"}, {"person": "bob", "role": "manager"}]
}
```

策略会校验：规则角色不重复、need 为正、人员角色必须有对应规则、每个角色有资格人数不少于 need。

### 版本登记

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| PUT | `/components/{component}/versions/{version}` | 登记（或幂等重放）不可变版本 |
| GET | `/components/{component}/versions/{version}` | 查询单个版本 |
| GET | `/versions?component=xx` | 列出版本（不带 component 列全部） |

```json
PUT /components/payment/versions/1.2.0
{
  "constraints": [
    {"component": "order", "op": ">=", "version": "2.0.0"}
  ],
  "request_id": "optional-idempotency-key"
}
```

### 列车

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/trains` | 创建列车 |
| GET | `/trains` | 列出全部列车 |
| GET | `/trains/{id}` | 查询列车详情 |
| PUT | `/trains/{id}/candidates/{component}` | 冻结前设置/替换候选 `{"version":"1.2.0","expected_version":N}` |
| DELETE | `/trains/{id}/candidates/{component}` | 冻结前移除候选（body 可带 `expected_version`） |
| POST | `/trains/{id}/freeze` | 冻结（原子依赖校验 + 保存策略快照） |
| POST | `/trains/{id}/approve` | 审批 `{"person":"alice","role":"qa"}` |
| POST | `/trains/{id}/cancel` | 取消（editing/frozen 可取消，重复取消幂等） |
| POST | `/trains/{id}/release` | 放行（审批齐才成功，重复放行返回原结果） |

动作类请求（freeze/approve/cancel/release）均可带 `expected_version` 与 `request_id`，请求体允许为空 `{}`。

### Outbox

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/outbox` | 列出未派发事件（每列车至多一条 `train.released`；每个环境至多一条 `environment.promoted`） |
| POST | `/outbox/{id}/dispatch` | 标记事件已派发 |

事件 payload 包含 `train_id`、`snapshot_id` 与冻结快照的组件版本表；`environment.promoted` 还包含 `promotion_id`、`attempt_no`、`environment`。

## 跨环境晋级

### 严格顺序与审批门槛

环境默认严格顺序为 `dev → staging → production`，可用 `PUT /environment-policy` 整体替换（环境名即顺序，每个环境各自一份角色规则 + 有资格人员名单；规则为空表示该环境免审批）。环境定义与列车审批策略一样，在**创建晋级活动时复制为快照**，之后修改不影响在途活动。

每个环境的部署领取（claim）同时受两个条件约束：

1. 该环境的审批门槛已满足（角色人数齐备；同一 `(person, role)` 审批幂等）；
2. 严格顺序上前一环境已 `promoted`。

两者任一不满足都无法领取，因此回执也不可能越过尚未完成的前一环境。

### 完整列车为单位 + 失败回退

- 工作者领取部署时拿到 **租约号（lease_no）+ 尝试号（attempt_no）** 作为 fencing token，以及整列车快照目标和“部署前版本表”。
- 必须以**完整列车**为单位提交回执：快照内每个组件都有回执后才收尾。**全部成功**才把环境记为 `promoted`，原子更新现网版本并写出且只写一条 `environment.promoted` 事件。
- 任一组件失败：如实记录每个组件的实际结果（成功/失败、版本、消息），**绝不把混合版本当成功**，立即进入回退——为每个已更新（成功）的组件生成回退单元，恢复目标是首次领取时冻结的“部署前版本”（此前不存在则回退为下线）。回退单元逐个领取/回报，失败的单元可被再次领取重试；全部回退成功后本次尝试落为 `failed`。

### 租约 fencing 与并发

- 同一工作者重复领取其活动租约返回原租约；其他工作者领取视为**接管**：`lease_no` 单调 +1，旧租约的回执一律拒绝（不能覆盖接管后的状态），接管按整列车重新部署。
- 回执必须携带当前 `attempt_no` 与 `lease_no`：旧尝试、被接管的旧租约都返回状态冲突。
- 取消与审批/回执并发：若已有组件更新，取消进入 `cancel_requested` 并触发回退，回退完成才 `cancelled`，不遗留混合版本、不记成功；若无在途更新则立即 `cancelled`。重复取消幂等；`succeeded` 后取消报冲突。
- 部署成功、回退、取消均与现网版本表、outbox 在同一事务内落盘。

### 尝试（attempt）

```
创建活动(尝试1 running)
  env: waiting_approval ──审批齐/前序done──▶ ready ──claim──▶ deploying
   deploying ──全部组件成功──▶ promoted（写 1 条 environment.promoted）
   deploying ──任一组件失败──▶ rolling_back ──回退完成──▶ failed
                                                            │ 修复后
                                                            ▼
                                                    创建尝试2（沿用同一快照，
                                                    已在快照版本的环境继承）
  所有环境 promoted ──▶ succeeded（终态）
  取消（有在途更新先回退）──▶ cancel_requested ──▶ cancelled（终态）
```

### 环境晋级接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| PUT | `/environment-policy` | 设置环境顺序与各环境门槛（只影响之后创建的活动） |
| GET | `/environment-policy` | 查询当前环境定义 |
| POST | `/trains/{id}/promotions` | 在已放行列车上创建晋级活动（冻结快照/顺序/门槛，可带 `request_id`） |
| GET | `/promotions` | 列出全部晋级活动 |
| GET | `/promotions/{id}` | 详情视图（见下） |
| POST | `/promotions/{id}/approve` | 环境审批 `{"environment","person","role"}` |
| POST | `/promotions/{id}/cancel` | 取消（有在途更新则回退，可带 `request_id`） |
| POST | `/promotions/{id}/attempts` | 最近尝试失败回退后创建新尝试（沿用原快照，可带 `request_id`） |
| POST | `/promotions/{id}/environments/{env}/claim` | 领取部署 `{"worker":"w1"}`，返回 `attempt_no/lease_no/targets/before` |
| POST | `/promotions/{id}/environments/{env}/receipts` | 提交组件回执（带 `attempt_no`、`lease_no`、`receipts[]`，可带 `request_id`） |
| POST | `/promotions/{id}/rollback/claim` | 领取下一个待回退单元 |
| POST | `/promotions/{id}/rollback/report` | 回报回退单元结果 `{"attempt_no","lease_no","environment","component","success"}` |

`GET /promotions/{id}` 每次尝试、每个环境返回：执行前版本（`before_versions`，空串表示此前无该组件）、目标版本（`target_versions`，即列车快照）、现网实际版本（`current_versions`）、各组件部署回执（`results`）、回退单元进度（`rollback`）、审批门槛与已获审批及缺口（`approval_policy/approvals/missing_approvals`）、当前租约（`lease`）、晋级时间与事件 ID，以及当前不能继续的原因（`blocking_reason`：审批未齐 / 等待前一环境 / 回退中 / 等待新尝试等）。

### 环境晋级快速试用

```bash
# 前置：版本登记、装候选、冻结、审批、放行（见“快速试用”），得到已放行列车 $TID
PID=$(curl -sXPOST localhost:8080/trains/$TID/promotions -d '{}' | jq -r .id)

# 1. 领取 dev 部署（默认环境免审批），得到 attempt_no / lease_no
curl -sXPOST localhost:8080/promotions/$PID/environments/dev/claim -d '{"worker":"w1"}'

# 2. 提交整列车回执（全部成功 → promoted，写一条 environment.promoted）
curl -sXPOST localhost:8080/promotions/$PID/environments/dev/receipts -d '{
  "attempt_no":1,"lease_no":1,
  "receipts":[
    {"component":"order","success":true},
    {"component":"payment","success":true}
  ]}'

# 若有组件失败（success:false）→ 自动进入回退：
#   POST /promotions/$PID/rollback/claim  领取单元
#   POST /promotions/$PID/rollback/report 回报成功（恢复到部署前版本）
# 全部回退完成后：
curl -sXPOST localhost:8080/promotions/$PID/attempts -d '{}'   # 新尝试，沿用同一快照
# 然后依次 staging、production 重复 claim + receipts
```

## 快速试用

```bash
# 1. 策略
curl -sXPUT localhost:8080/policy -d '{
  "rules":[{"role":"qa","need":1}],
  "approvers":[{"person":"alice","role":"qa"}]}'

# 2. 登记组件版本
curl -sXPUT localhost:8080/components/order/versions/2.1.0 -d '{"constraints":[]}'
curl -sXPUT localhost:8080/components/payment/versions/1.2.0 \
  -d '{"constraints":[{"component":"order","op":">=","version":"2.0.0"}]}'

# 3. 建车、装候选
TID=$(curl -sXPOST localhost:8080/trains -d '{"name":"R1"}' | jq -r .id)
curl -sXPUT  localhost:8080/trains/$TID/candidates/order   -d '{"version":"2.1.0"}'
curl -sXPUT  localhost:8080/trains/$TID/candidates/payment -d '{"version":"1.2.0"}'

# 4. 冻结（候选不兼容时这里会返回 422 和 problems）
curl -sXPOST localhost:8080/trains/$TID/freeze -d '{}'

# 5. 审批、放行
curl -sXPOST localhost:8080/trains/$TID/approve -d '{"person":"alice","role":"qa"}'
curl -sXPOST localhost:8080/trains/$TID/release -d '{}'
curl -s localhost:8080/outbox
```

## 代码结构

| 文件 | 内容 |
| --- | --- |
| `version.go` | 语义版本解析/比较，版本约束与满足判断 |
| `model.go` | 组件版本、策略快照、审批、outbox 事件、列车聚合与状态机字段 |
| `validation.go` | 快照依赖校验：约束满足 + Kahn 无环检测，问题一次性收集 |
| `aggregate.go` | 列车聚合行为：编辑、冻结、审批、取消、放行 |
| `promotion.go` | 环境晋级聚合：环境顺序/门槛快照、尝试、部署租约与回执 fencing、回退、取消、新尝试 |
| `promotion_service.go` | 晋级应用服务：事务编排、幂等键、outbox 唯一、现网版本表、查询视图与阻断原因 |
| `errors.go` | 七类领域错误及 `DependencyError`（携带全部问题） |
| `persistence.go` | 事务式 Store 接口、JSON 文件原子落盘、内存存储、列车与晋级活动序列化 |
| `service.go` | 应用服务：事务编排、乐观版本条件、幂等键、全部用例与查询 |
| `http.go` / `http_promotion.go` | JSON HTTP 接口与错误码映射（含全部晋级端点） |
| `cmd/release-train/main.go` | 服务入口 |

## 测试

```bash
go test -race ./...
```

覆盖范围：

- 版本解析/比较、五种约束算子；
- 冻结校验：兼容通过、约束不满足、依赖缺失、版本未登记、环与自环、多问题同时返回；
- 冻结失败原子性（状态/版本/快照均无变化），修复后可重新冻结；
- 冻结快照不可变；审批资格、同人员同角色幂等、审批不齐拒绝放行；
- 冻结后修改策略不影响在途列车（策略快照隔离）；
- 放行/取消互斥、重复放行返回原结果且 outbox 仅一条；
- 乐观版本条件（过期版本号冲突，无丢失更新）；
- 并发：20 路同时放行仅一条 outbox、放行/取消竞争恰一个终态、并发编辑一胜一冲突；
- 版本不可变冲突、request id 重放与冲突；
- 文件持久化重启恢复（快照/策略/审批/outbox/不可变性）；
- HTTP 全流程、422 problems、409 版本冲突、404/400。

环境晋级：

- 创建门槛：仅 `released` 可晋级、快照依赖不完整被拒（422 problems）、一列车一次活动；
- 环境顺序/门槛快照冻结，创建后改定义不影响在途活动；审批资格与幂等、未齐/越序领取被拒；
- 完整列车部署：全部成功才 promoted 且每环境一条稳定 outbox；
- 部分组件失败如实记录结果、不把混合版本当成功，按“部署前版本”回退已更新组件（失败单元可重试）；
- 回退完成后新尝试沿用同一快照；已晋级环境继承、不重复部署/出事件；
- 租约/尝试号 fencing：同工人重领不变号、接管租约 +1、旧租约与旧尝试回执被拒、旧结果不覆盖接管后状态；
- 并发 10 路领取租约号单调、仅最高租约可提交；
- 回执幂等（分批、request_id 重试、晋级后重放）且 outbox 不重复；
- 取消并发：有在途更新先 `cancel_requested` 回退再 `cancelled`、无更新立即取消、成功后取消冲突、重复取消幂等；
- 查询视图：执行前后/目标版本、组件结果、回退进度、审批依据与缺口、当前阻断原因；
- 晋级活动重启恢复后可继续回退且旧租约仍被 fencing；
- HTTP 全流程（门槛/越序 409、无资格 403、失败回退新尝试、outbox 计数、404/400）。
