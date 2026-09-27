# go-release-train

发布列车（Release Train）服务：管理组件版本登记、组件间版本约束、候选列车的编辑/冻结/审批/取消/放行全生命周期，保证冻结快照内依赖始终一致。

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
| GET | `/outbox` | 列出未派发事件（每列车至多一条 `train.released`） |
| POST | `/outbox/{id}/dispatch` | 标记事件已派发 |

事件 payload 包含 `train_id`、`snapshot_id` 与冻结快照的组件版本表。

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
| `aggregate.go` | 聚合行为：编辑、冻结、审批、取消、放行 |
| `errors.go` | 七类领域错误及 `DependencyError`（携带全部问题） |
| `persistence.go` | 事务式 Store 接口、JSON 文件原子落盘、内存存储、序列化 |
| `service.go` | 应用服务：事务编排、乐观版本条件、幂等键、全部用例与查询 |
| `http.go` | JSON HTTP 接口与错误码映射 |
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
