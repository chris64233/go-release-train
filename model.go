package releasetrain

import (
	"encoding/json"
	"sort"
	"time"
)

// TrainState 是发布列车生命周期状态。
//
//	Editing ──freeze──▶ Frozen ──release──▶ Released   （终态）
//	                    │
//	                    └──────cancel──▶ Cancelled       （终态）
//
// Editing 状态下也可以直接取消；Released 与 Cancelled 互为互斥的唯一终态。
type TrainState string

const (
	StateEditing   TrainState = "editing"
	StateFrozen    TrainState = "frozen"
	StateReleased  TrainState = "released"
	StateCancelled TrainState = "cancelled"
)

// ComponentVersion 是组件的一个已发布版本。版本发布后不可修改：
// 登记接口对相同 (component, version) 的重复请求做幂等校验，
// 约束集合不一致即返回 ErrIdempotencyConflict。
type ComponentVersion struct {
	Component   string       `json:"component"`
	Version     Version      `json:"version"`
	Constraints []Constraint `json:"constraints"`
	CreatedAt   time.Time    `json:"created_at"`
}

// ApprovalRule 是审批策略中的一条规则：角色 role 至少需要 need 名有资格人员通过。
type ApprovalRule struct {
	Role string `json:"role"`
	Need int    `json:"need"`
}

// Approver 是一名有审批资格的人员及其角色。
type Approver struct {
	Person string `json:"person"`
	Role   string `json:"role"`
}

// PolicySnapshot 是冻结时刻保存的审批策略快照。
// 放行只认这份快照；冻结之后策略如何修改都不影响本列车。
type PolicySnapshot struct {
	Rules     []ApprovalRule `json:"rules"`
	Approvers []Approver     `json:"approvers"`
}

// Approval 记录一次审批。同一 (person, role) 幂等：重复提交不报错、不重复计数。
type Approval struct {
	Person    string    `json:"person"`
	Role      string    `json:"role"`
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
}

// OutboxEvent 是放行事务内写入的发布事件。
// Release 在单个事务中同时落定 Released 状态与 outbox 事件，
// 因此一列车最多产生一条（重复放行直接返回原结果）。
type OutboxEvent struct {
	ID         string          `json:"id"`
	TrainID    string          `json:"train_id"`
	Type       string          `json:"type"` // 固定 "train.released"
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
	Dispatched bool            `json:"dispatched"`
}

// ReleaseResult 是放行结果；重复放行返回首次放行产生的同一个结果。
type ReleaseResult struct {
	TrainID    string    `json:"train_id"`
	ReleasedAt time.Time `json:"released_at"`
	EventID    string    `json:"event_id"`
	SnapshotID string    `json:"snapshot_id"`
	Idempotent bool      `json:"idempotent,omitempty"` // true 表示本次调用命中了首次放行的结果
}

// Train 是发布列车聚合。
type Train struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	state   TrainState
	Version int `json:"version"` // 聚合版本号，每次成功修改 +1，用于乐观并发条件

	// candidates 为候选组件 component -> 版本字符串。冻结前可替换，冻结后冻结进快照。
	candidates map[string]Version

	// frozenSnapshot 是冻结时不可变的候选快照（component -> Version）。
	frozenSnapshot map[string]Version
	snapshotID     string
	frozenAt       time.Time

	// policy 是冻结时保存的策略快照，终态前一直作为审批资格依据。
	policy *PolicySnapshot

	approvals []Approval

	releasedAt     time.Time
	cancelledAt    time.Time
	releaseEventID string

	createdAt time.Time
	updatedAt time.Time
}

// 访问器（model 包内测试/服务使用；持久化通过影子结构访问私有字段）。

func (t *Train) State() TrainState              { return t.state }
func (t *Train) Candidates() map[string]Version { return cloneCandidates(t.candidates) }
func (t *Train) Snapshot() map[string]Version   { return cloneCandidates(t.frozenSnapshot) }
func (t *Train) SnapshotID() string             { return t.snapshotID }
func (t *Train) Policy() *PolicySnapshot        { return t.policy }
func (t *Train) Approvals() []Approval          { return append([]Approval(nil), t.approvals...) }
func (t *Train) ReleasedAt() time.Time          { return t.releasedAt }
func (t *Train) FrozenAt() time.Time            { return t.frozenAt }
func (t *Train) CancelledAt() time.Time         { return t.cancelledAt }
func (t *Train) ReleaseEventID() string         { return t.releaseEventID }
func (t *Train) CreatedAt() time.Time           { return t.createdAt }
func (t *Train) UpdatedAt() time.Time           { return t.updatedAt }

func cloneCandidates(in map[string]Version) map[string]Version {
	if in == nil {
		return nil
	}
	out := make(map[string]Version, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// snapshotComponents 返回快照内组件名的有序列表。
func snapshotComponents(snap map[string]Version) []string {
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
