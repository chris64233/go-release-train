package releasetrain

import "time"

// 列车状态机：
//
//	open ──Freeze──▶ frozen ──Release──▶ released（终态）
//	  │                 │
//	  └────Cancel───────┴──Cancel──▶ cancelled（终态）
//
// released 与 cancelled 互斥，且均为唯一终态。
const (
	StateOpen      = "open"      // 候选可替换
	StateFrozen    = "frozen"    // 已冻结为不可变快照，等待审批/放行
	StateReleased  = "released"  // 终态：已放行
	StateCancelled = "cancelled" // 终态：已取消
)

// Component 是一个可发布组件的登记信息。
type Component struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Dependency 声明某版本对另一个组件的版本约束。
type Dependency struct {
	Component  string `json:"component"`
	Constraint string `json:"constraint"` // 例如 "^1.2.0"，多条件用逗号分隔表示 AND
}

// ComponentVersion 是组件的一个不可变版本：一旦登记，版本号与依赖约束均不可修改。
type ComponentVersion struct {
	Component    string       `json:"component"`
	Version      string       `json:"version"`
	Dependencies []Dependency `json:"dependencies"`
	CreatedAt    time.Time    `json:"created_at"`
}

// key 返回版本登记表中的唯一键。
func (v ComponentVersion) key() string { return v.Component + "@" + v.Version }

// ApprovalRule 是一条审批角色要求：角色名、具备该角色资格的人员名单、
// 以及至少需要几名该角色人员通过。
type ApprovalRule struct {
	Role      string   `json:"role"`
	Members   []string `json:"members"`   // 具备该角色审批资格的人员
	Threshold int      `json:"threshold"` // 需要的不同审批人数，至少 1
}

// Candidate 是列车中的一个候选组件版本。
type Candidate struct {
	Component string `json:"component"`
	Version   string `json:"version"`
}

func (c Candidate) key() string { return c.Component + "@" + c.Version }

// Approval 记录一次审批，按（人员, 角色）幂等。
type Approval struct {
	Person    string    `json:"person"`
	Role      string    `json:"role"`
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id"`
}

// OutboxEvent 是发布放行时写入 outbox 的事件。一次放行只产生一条。
type OutboxEvent struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"` // 目前固定 "train.released"
	TrainID   string    `json:"train_id"`
	Revision  int64     `json:"revision"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
	Published bool      `json:"published"`
}

// Train 是一趟发布列车及其不可变快照。
type Train struct {
	ID         string      `json:"id"`
	Revision   int64       `json:"revision"` // 乐观并发版本号，每次状态/内容修改 +1
	State      string      `json:"state"`
	Candidates []Candidate `json:"candidates"`

	// 冻结时保存的不可变快照；冻结前为空。
	FrozenAt       time.Time      `json:"frozen_at,omitempty"`
	PolicySnapshot []ApprovalRule `json:"policy_snapshot,omitempty"`

	Approvals []Approval `json:"approvals,omitempty"`

	ReleasedAt   time.Time `json:"released_at,omitempty"`
	CancelledAt  time.Time `json:"cancelled_at,omitempty"`
	CancelReason string    `json:"cancel_reason,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// ReleaseResult 缓存放行结果，保证重复放行返回同一份原结果。
	ReleaseResult *ReleaseResult `json:"release_result,omitempty"`
}

// ReleaseResult 是放行的返回结果，随列车持久化以实现“重复放行返回原结果”。
type ReleaseResult struct {
	TrainID    string      `json:"train_id"`
	Revision   int64       `json:"revision"`
	OutboxID   int64       `json:"outbox_id"`
	ReleasedAt time.Time   `json:"released_at"`
	Snapshot   []Candidate `json:"snapshot"`
}

// snapshotComponents 返回快照中的组件名集合。
func (t *Train) candidateSet() map[string]Candidate {
	m := make(map[string]Candidate, len(t.Candidates))
	for _, c := range t.Candidates {
		m[c.Component] = c
	}
	return m
}
