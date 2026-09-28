# go-release-train

发布列车（Release Train）服务：管理组件版本登记、组件间版本约束、候选列车的编辑/冻结/审批/取消/放行全生命周期，保证冻结快照内依赖始终一致；并在**已放行**列车上提供跨环境（开发 → 预发布 → 生产）逐级晋级、失败回退与接管控制。

开发环境：Go 1.23.0，无第三方依赖。

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

## 跨环境逐级晋级（环境 Promotion）

放行（release）是列车内容的**准入结论**：冻结快照通过依赖校验、审批齐备后列车成为不可变的可发布单元。环境晋级则发生在放行**之后**，把同一列已放行列车按严格顺序推过一个个真实环境，是发布流程的下一阶段，二者关系：

```
editing → frozen → released ──创建晋级（冻结快照/环境顺序/审批策略副本）──▶ promotion
                          （train.released outbox）                dev → staging → prod
                                                                   （每环境一条 environment.promoted outbox）
```

- **放行是晋级的前置门槛**：只有 `released` 列车才能创建晋级；未放行返回状态冲突。
- **晋级不改变列车**：晋级持有的是放行时冻结快照的**独立副本**（连同 `snapshot_id`）。即使后续注册表变化，晋级始终部署这一份内容；一次晋级失败后无论重试多少次，都沿用同一快照。
- **事件互补**：列车放行写一条 `train.released`；每个环境整列车晋级成功写一条 `environment.promoted`（payload 含环境、尝试号、执行前后版本表），各自稳定一条、随状态在同一事务落盘。

### 环境顺序与审批门槛

通过 `PUT /promotion-policy` 定义环境的**严格顺序**与每个环境各自的审批门槛（角色规则 + 有资格人员，校验规则与放行策略相同）。创建晋级时整份策略被冻结复制，之后修改策略不影响在途晋级。前一环境未晋级，后一环境始终 `blocked`，审批与部署都不能越级。

环境状态：`blocked → awaiting_approval → ready → deploying → promoted`；失败时 `deploying → rolling_back → failed`；取消时进入 `cancelling`，回退完成才 `cancelled`。

### 整列车部署与失败回退

- 部署以**整列车为单位**：工作者先 `claim` 取得整个快照的目标版本和该环境的**执行前基线**（首次领取时拍下，整个尝试内固定）。每个组件分别回报实际结果；**所有组件都成功**才记为该环境 `promoted` 并写出唯一 outbox。
- 成功回执的版本必须等于快照版本，否则拒绝——不接受混合版本被记为成功。
- 任一组件失败：记录该组件实际结果（含实际版本/原因），立即把环境转入 `rolling_back`，为所有相对基线发生偏离的已更新组件生成回退任务，恢复到**该环境本次执行前的版本**（基线缺失即移除）。全部回退成功后落 `failed`。混版状态绝不会被当成成功，回退完成前下一环境不开放。
- 回退任务本身可失败、可由新回执重试（`pending/failed → succeeded`）。

### 租约、尝试号与接管

审批、部署回执、取消可能同时到达（全部写操作在单个存储事务内串行化）：

- 领取部署得到 `token` + `epoch`（租约/接管代次）+ `attempt`（晋级尝试号）。**只有当前尝试的当前租约能提交结果**：旧尝试回执（`attempt` 不符）、被接管后的旧 token/旧 epoch 回执一律返回 `lease_conflict`，不能覆盖接管后的状态。
- 不同工作者重复领取即**接管**：epoch+1、旧租约即刻失效；同一工作者重领（续租/安全重试）租约不变。
- 回退使用独立租约：进入回退后部署租约失效，需显式 `rollback/claim`。
- 同一租约同一组件的**重复回执幂等**返回已有结果；携带相同 `request_id` 的请求重试重放首次结果，不会重复计数或重复推进。
- 迟到回执不能越过前一环境：只能回报当前开放环境；已晋级环境只接受完成租约的重复回执，其余按陈旧租约拒绝。

### 失败后新尝试

一次晋级失败（`failed`）后，问题修复可 `retry` 创建新尝试：尝试号 +1，审批、部署结果、回退、租约全部重置，**但快照不变**（仍是原 `snapshot_id` 与同一份组件版本），已成功晋级的环境保持 `promoted` 不重做。

### 取消

- 审批等待/就绪期取消：没有任何组件被更新，直接落 `cancelled`。
- 部署/回退进行中取消：进入 `cancelling`，按基线把已更新组件恢复完才落 `cancelled`；若领取后尚无组件实际更新（零偏离）则直接取消，不留空回退。
- `promoted` 不可取消；重复取消幂等。

### 查询

`GET /promotions/{id}` 返回每个环境的：执行前版本（`before_versions`，领取时基线）、目标版本、当前实际版本（`current_versions`）、逐组件部署结果（含 from/to/成功否/原因）、回退任务与进度（`rollback_done/total`）、审批策略快照与已审批人、尚缺审批，以及顶层 `blocking_reason`（等待审批/等待领取/部署中/回退进度/失败原因/取消中等当前阻断原因）。

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
| `ErrLease` | 409 | `lease_conflict` | 旧尝试/被接管租约的迟到部署或回退回执 |

## 持久化

默认使用单个 JSON 文件（`-state` 指定，默认 `data/state.json`）：

- 所有写操作在一个事务内完成：先在状态副本上执行业务逻辑，任何校验失败都丢弃副本（回滚，无部分结果）；成功后整份状态经 **临时文件 + rename 原子落盘**。
- 进程重启后自动恢复：版本登记、列车（含冻结快照、策略快照、审批记录）、跨环境晋级（快照副本、各环境状态/审批/结果/回退/租约）、outbox、幂等记录。
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
| GET | `/outbox` | 列出未派发事件（每列车至多一条 `train.released`） |
| POST | `/outbox/{id}/dispatch` | 标记事件已派发 |

事件 payload 包含 `train_id`、`snapshot_id` 与冻结快照的组件版本表。

### 跨环境晋级

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| PUT | `/promotion-policy` | 设置环境严格顺序与各环境审批门槛 |
| GET | `/promotion-policy` | 查询当前晋级策略 |
| POST | `/promotions` | 对已放行列车创建晋级 `{"train_id":"...","request_id":"..."}` |
| GET | `/promotions` | 列出全部晋级 |
| GET | `/promotions/{pid}` | 查询晋级详情（前后版本/结果/回退/审批依据/阻断原因） |
| POST | `/promotions/{pid}/environments/{env}/approve` | 环境审批 `{"person","role"}` |
| POST | `/promotions/{pid}/environments/{env}/deploy/claim` | 领取/接管部署 `{"worker"}`，返回 token/attempt/epoch/目标/基线 |
| POST | `/promotions/{pid}/environments/{env}/deploy/report` | 单组件部署回执（见下） |
| POST | `/promotions/{pid}/environments/{env}/rollback/claim` | 领取/接管回退，返回未完成任务 |
| POST | `/promotions/{pid}/environments/{env}/rollback/report` | 单组件回退回执 |
| POST | `/promotions/{pid}/cancel` | 取消（进行中先回退） |
| POST | `/promotions/{pid}/retry` | 失败后开启新尝试（沿用原快照） |

部署回执请求体：

```json
{
  "token": "lease_...", "attempt": 1, "epoch": 1,
  "component": "payment",
  "success": true,
  "version": "1.2.0",
  "message": "optional detail",
  "request_id": "optional-idempotency-key"
}
```

回退回执把 `success/version` 换为 `"status": "succeeded" | "failed"`。写接口均可带 `request_id`；回执成功且为最后一个组件时，响应含 `env_promoted`、`promotion_done`、`event_id`。

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

### 跨环境晋级试用

```bash
# 1. 定义环境顺序与门槛：dev(qa) → staging(qa+manager) → prod(director)
curl -sXPUT localhost:8080/promotion-policy -d '{
  "environments": [
    {"name":"dev","rules":[{"role":"qa","need":1}],
     "approvers":[{"person":"alice","role":"qa"}]},
    {"name":"staging","rules":[{"role":"qa","need":1},{"role":"manager","need":1}],
     "approvers":[{"person":"alice","role":"qa"},{"person":"bob","role":"manager"}]},
    {"name":"prod","rules":[{"role":"director","need":1}],
     "approvers":[{"person":"carol","role":"director"}]}
  ]}'

# 2. 对已放行列车创建晋级（未放行列车会 409）
PID=$(curl -sXPOST localhost:8080/promotions -d "{\"train_id\":\"$TID\"}" | jq -r .id)

# 3. dev 审批、领取部署（拿到 token/attempt/epoch/基线）
curl -sXPOST localhost:8080/promotions/$PID/environments/dev/approve \
  -d '{"person":"alice","role":"qa"}'
LEASE=$(curl -sXPOST localhost:8080/promotions/$PID/environments/dev/deploy/claim -d '{"worker":"w1"}')
TOKEN=$(echo $LEASE | jq -r .token)

# 4. 逐组件回报；任一失败会自动进入回退
curl -sXPOST localhost:8080/promotions/$PID/environments/dev/deploy/report -d "{
  \"token\":\"$TOKEN\",\"attempt\":1,\"epoch\":1,
  \"component\":\"order\",\"success\":true,\"version\":\"2.1.0\"}"

# 5. 查询：前后版本、组件结果、回退进度、审批依据、blocking_reason
curl -s localhost:8080/promotions/$PID | jq

# 失败修复后沿用同一快照开新尝试：
curl -sXPOST localhost:8080/promotions/$PID/retry -d '{}'
```

## 代码结构

| 文件 | 内容 |
| --- | --- |
| `version.go` | 语义版本解析/比较，版本约束与满足判断 |
| `model.go` | 组件版本、策略快照、审批、outbox 事件、列车聚合与状态机字段 |
| `validation.go` | 快照依赖校验：约束满足 + Kahn 无环检测，问题一次性收集 |
| `aggregate.go` | 聚合行为：编辑、冻结、审批、取消、放行 |
| `errors.go` | 七类领域错误及 `DependencyError`（携带全部问题） |
| `persistence.go` | 事务式 Store 接口、JSON 文件原子落盘、内存存储、序列化 |
| `service.go` | 应用服务：事务编排、乐观版本条件、幂等键、全部用例与查询 |
| `promotion.go` | 跨环境晋级模型：晋级/环境/租约状态、部署结果与回退任务、阻断原因推导 |
| `promotion_aggregate.go` | 晋级聚合行为：环境审批、领取/接管、部署回执、回退、取消、失败重试 |
| `promotion_service.go` | 晋级用例：创建校验、事务编排、幂等重放、outbox 同事务落定、查询 |
| `promotion_persistence.go` | 晋级聚合的 JSON 线格式与深拷贝 |
| `http.go` | JSON HTTP 接口与错误码映射（含晋级路由） |
| `promotion_http.go` | 晋级 HTTP 处理器与晋级查询 DTO |
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

跨环境晋级覆盖：

- 策略校验（空/重名/门槛人数不足）；未放行列车不能创建晋级；
- 严格顺序：后序环境 blocked、越级审批/领取被拒；各环境审批门槛与资格；
- 整列车部署：全成功才晋级且每环境仅一条 `environment.promoted`；成功回执版本必须等于快照；
- 部分失败：记录实际结果、按基线为已更新组件回退、失败任务可重试、回退完成落 failed，混版不算成功；
- 租约：旧尝试回执、被接管旧 token/旧 epoch 回执、伪造 token 全部拒绝；接管替换残留结果；重复回执幂等；
- 零偏离取消/失败不留空回退；取消与审批/回执并发结果自洽（无混版残留）；
- 失败后 retry 沿用同一快照、尝试号 +1、审批/结果/回退重置、已晋级环境保留；
- 幂等：创建/回执的 request id 重放、并发重复回执只记一条；
- 查询视图：前后版本、组件结果、回退进度、审批依据与 blocking_reason；
- 晋级状态文件重启恢复；晋级 HTTP 全流程（审批/领取/失败/回退/重试/逐级晋级/outbox）。
