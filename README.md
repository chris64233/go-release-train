# go-release-train

发布列车（Release Train）服务：管理组件版本及其依赖约束，把若干候选组件冻结为
**不可变快照**，经策略审批后放行；冻结校验依赖图无环且所有版本约束在快照内自洽。

开发环境：Go 1.23.0，无第三方依赖。

## 核心模型与不变量

- **版本不可变**：`组件@版本` 一经登记，其依赖约束不可修改。重复登记相同内容幂等返回，
  内容不同则返回 `state` 冲突。
- **依赖约束**：每个版本声明对其他组件的 semver 约束，支持 `= != > >= < <=`、
  `^`（兼容版本）、`~`（次版本兼容），逗号分隔表示 AND（如 `>=1.0.0,<2.0.0`）。
- **列车状态机**：

  ```
  open ──Freeze──▶ frozen ──Release──▶ released （终态）
    │                 │
    └────Cancel───────┴──Cancel──▶ cancelled（终态）
  ```

  `released` 与 `cancelled` 互斥，且都是唯一终态。
- **冻结是一次原子校验**：冻结时验证
  1. 每个依赖的目标组件都在候选快照内；
  2. 目标版本满足声明的全部约束；
  3. 候选构成的依赖图无环（DFS 三色标记）。

  任一不满足则**整个冻结失败**，列车保持 `open`、修订号不变，不留下任何部分结果。
- **审批策略快照**：冻结瞬间把当前策略（角色、成员名单、阈值）复制进列车。之后全局
  策略如何修改都不影响本列车——审批资格只认快照。审批按 **(人员, 角色)** 幂等；
  每个角色要求若干**不同**人员达到阈值，全部角色满足后才可放行。
- **乐观并发（版本条件）**：列车带单调递增的 `revision`。候选修改、冻结、审批、取消
  都必须携带期望修订号；过期写入返回 `412 version conflict`（响应含当前修订号），
  杜绝并发覆盖丢状态。
- **放行幂等 + 单一 outbox**：重复放行（即使携带过期修订号）返回首次放行的同一份
  结果；一次放行只向发布 outbox 写入一条 `train.released` 事件。已放行不可取消，
  已取消不可放行。

## 持久化与并发

- 所有状态（组件、版本、列车、策略快照、审批、outbox、幂等记录）持久化到单个 JSON
  文件；写入采用「临时文件 + rename」原子替换，崩溃只会看到旧版或新版，不会半截。
- 进程内用读写锁串行化所有变更：校验、状态迁移、落盘在同一个临界区完成，因此
  并发放行/取消恰好一方胜出，绝不会出现 outbox 与 cancelled 并存。
- 多实例部署时应换用带事务与条件更新（CAS/`WHERE revision=?`）的数据库存储，
  接口层与领域逻辑无需改动。

## HTTP 接口

所有写接口支持 `Idempotency-Key` 请求头：同键 + 同请求体重放首次响应（响应头
`Idempotency-Replayed: true`）；同键不同体返回 `409 idempotency conflict`。

版本条件通过 `X-Expected-Revision: <n>`（或 `If-Match`、查询参数
`?expected_revision=n`、请求体 `expected_revision`）传递。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/components` | 登记组件（重名幂等） |
| GET | `/v1/components` / `/v1/components/{name}` | 查询组件 |
| POST | `/v1/components/{name}/versions` | 登记不可变版本及依赖 |
| GET | `/v1/components/{name}/versions` | 列出组件版本（按 semver 排序） |
| PUT/GET | `/v1/policy` | 设置/查询当前审批策略（不影响已冻结列车） |
| POST | `/v1/trains` | 创建 open 列车与候选清单 |
| GET | `/v1/trains` / `/v1/trains/{id}` | 查询列车 |
| PUT | `/v1/trains/{id}/candidates` | 替换候选（仅 open，需修订号） |
| POST | `/v1/trains/{id}/freeze` | 冻结（依赖图 + 约束原子校验，需修订号） |
| POST | `/v1/trains/{id}/approvals` | 提交审批（仅 frozen，资格取策略快照，需修订号） |
| GET | `/v1/trains/{id}/approvals` | 审批满足情况 |
| POST | `/v1/trains/{id}/cancel` | 取消（open/frozen，需修订号） |
| POST | `/v1/trains/{id}/release` | 放行（仅 frozen 且审批齐备，重复调用返回原结果） |
| GET | `/v1/outbox` | 查看发布事件 |
| POST | `/v1/outbox/{id}/publish` | 投递方确认事件已发布 |

### 错误分类

响应体统一为 `{"error":{"kind","message","current_revision"?}}`，类别与状态码：

| kind | 含义 | HTTP 状态码 |
|---|---|---|
| `validation` | 参数/版本号/约束格式非法 | 400 |
| `not_found` | 组件、版本或列车不存在 | 404 |
| `dependency` | 冻结时约束不满足、缺依赖或依赖有环 | 422 |
| `approval` | 无审批资格、角色不存在或审批未齐 | 403 |
| `state` | 状态机非法迁移（冻结后改候选、终态互斥等） | 409 |
| `version` | 修订号过期（乐观并发冲突），响应带当前修订号 | 412 |
| `idempotency` | 幂等键被不同请求体复用 | 409 |

## 快速开始

```bash
# 启动（默认 :8080，数据文件 data/release-train.json）
go run ./cmd/releasetrain -addr :8080 -store data/release-train.json

# 1. 登记组件与不可变版本
curl -s -X POST localhost:8080/v1/components -d '{"name":"api"}'
curl -s -X POST localhost:8080/v1/components -d '{"name":"lib"}'
curl -s -X POST localhost:8080/v1/components/lib/versions \
  -d '{"version":"1.0.0","dependencies":[]}'
curl -s -X POST localhost:8080/v1/components/api/versions \
  -d '{"version":"1.0.0","dependencies":[{"component":"lib","constraint":"^1.0.0"}]}'

# 2. 设置审批策略：qa 角色至少 1 人（alice 或 erin）通过
curl -s -X PUT localhost:8080/v1/policy -d '{
  "rules":[{"role":"qa","members":["alice","erin"],"threshold":1}]}'

# 3. 建车 → 冻结（带期望修订号）
curl -s -X POST localhost:8080/v1/trains -d '{
  "id":"T1",
  "candidates":[{"component":"api","version":"1.0.0"},
                {"component":"lib","version":"1.0.0"}]}'
curl -s -X POST localhost:8080/v1/trains/T1/freeze \
  -H 'X-Expected-Revision: 1'

# 4. 审批（资格取自冻结时的策略快照）→ 放行
curl -s -X POST localhost:8080/v1/trains/T1/approvals \
  -d '{"person":"alice","role":"qa","expected_revision":2}'
curl -s -X POST localhost:8080/v1/trains/T1/release \
  -H 'X-Expected-Revision: 3'
```

冻结失败示例（快照内 `lib` 只提供 1.0.0，无法满足 `^2.0.0`）返回 `422`：

```json
{"error":{"kind":"dependency","message":"component api requires lib ^2.0.0, but snapshot provides 1.0.0"}}
```

## 代码结构

| 文件 | 内容 |
|---|---|
| `semver.go` | 语义化版本与约束（`^`/`~`/范围/AND 列表）解析与判定 |
| `errors.go` | 结构化错误与七类冲突分类 |
| `models.go` | 组件、版本、列车、策略快照、审批、outbox 等领域模型 |
| `store.go` | JSON 文件原子持久化 + 进程内读写锁 |
| `service.go` | 应用服务：登记/编辑/冻结校验/审批/取消/放行/幂等 |
| `api.go`、`http_helpers.go` | HTTP 路由、版本条件、幂等键中间件、错误映射 |
| `cmd/releasetrain/main.go` | 服务入口 |

## 测试

```bash
go test -race ./...          # 全部测试（含竞态检测）
go test -cover ./...         # 覆盖率
```

测试覆盖：semver 比较与约束、版本不可变、冻结成功/约束不满足/缺依赖/有环的
原子失败、冻结后候选不可改、策略快照隔离、审批幂等与阈值、放行幂等与单条 outbox、
放行/取消 50 轮并发竞态（终态唯一且 outbox 不串）、20 路并发候选编辑（恰好一次成功）、
持久化重载后状态完整、以及 HTTP 端到端与错误码映射。
