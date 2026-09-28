package releasetrain

import (
	"fmt"
	"sort"
	"time"
)

// PromotionState 是一次晋级执行的生命周期状态。
//
//	Running ──全部环境成功──▶ Promoted        （终态）
//	   │ 某环境部署失败，回退完成
//	   ▼
//	Failed ──retry（沿用同一快照）──▶ Running
//	   │
//	   ├──cancel（无在途部署）──────────▶ Cancelled （终态）
//
// 取消发生在部署/回退进行中时进入 Cancelling，回退把环境恢复到执行前版本后
// 才落到 Cancelled——绝不把混合版本状态留在任何中间态里。
type PromotionState string

const (
	PromoRunning    PromotionState = "running"
	PromoFailed     PromotionState = "failed"
	PromoCancelling PromotionState = "cancelling"
	PromoCancelled  PromotionState = "cancelled"
	PromoPromoted   PromotionState = "promoted"
)

// EnvState 是单个环境在本次晋级中的状态。
type EnvState string

const (
	// EnvBlocked：前一环境尚未晋级，本环境不能审批/部署（不得越级）。
	EnvBlocked EnvState = "blocked"
	// EnvAwaitingApproval：本环境已开放，审批门槛尚未满足。
	EnvAwaitingApproval EnvState = "awaiting_approval"
	// EnvReady：审批已齐，等待工作者领取部署。
	EnvReady EnvState = "ready"
	// EnvDeploying：工作者持租约部署中。
	EnvDeploying EnvState = "deploying"
	// EnvRollingBack：部分组件失败/取消打断，正在恢复执行前版本。
	EnvRollingBack EnvState = "rolling_back"
	// EnvPromoted：整列车在本环境部署成功（终态，outbox 已稳定写出一条）。
	EnvPromoted EnvState = "promoted"
	// EnvFailed：本环境尝试失败且回退完成，等待新尝试。
	EnvFailed EnvState = "failed"
	// EnvCancelled：随晋级一并取消。
	EnvCancelled EnvState = "cancelled"
)

// RollbackStatus 是回退任务状态。
type RollbackStatus string

const (
	RollbackPending   RollbackStatus = "pending"
	RollbackSucceeded RollbackStatus = "succeeded"
	RollbackFailed    RollbackStatus = "failed"
)

// EnvPolicy 是一个环境的审批门槛定义（规则 + 有资格人员名单）。
type EnvPolicy struct {
	Name      string         `json:"name"`
	Rules     []ApprovalRule `json:"rules"`
	Approvers []Approver     `json:"approvers"`
}

// PromotionPolicy 定义环境的严格顺序及各自审批门槛。
// 数组顺序即晋级顺序；创建晋级执行时整份策略被冻结复制。
type PromotionPolicy struct {
	Environments []EnvPolicy `json:"environments"`
}

// EnvApproval 记录某环境针对当前尝试的一次审批。
type EnvApproval struct {
	Person string    `json:"person"`
	Role   string    `json:"role"`
	At     time.Time `json:"at"`
}

// Lease 是工作者领取部署/回退时取得的租约。
// Epoch（领取/接管尝试号）随每次接管单调递增；只有 token 与 epoch 都匹配的
// 当前租约可以提交回执，旧租约的迟到回执一律被拒绝。
type Lease struct {
	Token  string    `json:"token"`
	Epoch  int       `json:"epoch"`
	Worker string    `json:"worker"`
	At     time.Time `json:"at"`
}

func (l *Lease) check(token string, epoch int) error {
	if l == nil || l.Token != token || l.Epoch != epoch {
		return fmt.Errorf("%w: receipt carries stale lease (epoch %d); current lease is epoch %d held by %s",
			ErrLease, epoch, epochOf(l), workerOf(l))
	}
	return nil
}

func epochOf(l *Lease) int {
	if l == nil {
		return 0
	}
	return l.Epoch
}

func workerOf(l *Lease) string {
	if l == nil {
		return "<none>"
	}
	return l.Worker
}

// DeployResult 是单个组件的部署回执结果。
// From 为该环境执行前版本：指针为 nil 表示执行前该环境没有此组件。
// Token/Epoch 标识提交该回执的租约：重复回执（同 token+epoch）重放本记录，
// 被接管后的旧租约回执不能覆盖它。
type DeployResult struct {
	Component string    `json:"component"`
	From      *Version  `json:"from_version"`
	To        *Version  `json:"to_version"`
	Success   bool      `json:"success"`
	Message   string    `json:"message,omitempty"`
	Token     string    `json:"lease_token"`
	Epoch     int       `json:"epoch"`
	At        time.Time `json:"at"`
}

// RollbackTask 是把单个已更新组件恢复到环境执行前版本的任务。
// To 为 nil 表示执行前环境中没有该组件（恢复即移除）。
type RollbackTask struct {
	Component string         `json:"component"`
	To        *Version       `json:"to_version"`
	Status    RollbackStatus `json:"status"`
	Message   string         `json:"message,omitempty"`
	Token     string         `json:"lease_token,omitempty"`
	Epoch     int            `json:"epoch"`
	At        time.Time      `json:"at,omitempty"`
}

// EnvExecution 是一个环境在本次晋级中的执行状态。
type EnvExecution struct {
	Name    string   `json:"name"`
	Order   int      `json:"order"`
	State   EnvState `json:"state"`
	Attempt int      `json:"attempt"` // 该执行所属的晋级尝试号

	// Policy 是创建晋级时冻结的本环境审批门槛快照。
	Policy PolicySnapshot `json:"policy"`

	// Approvals 针对当前尝试；失败后开新尝试会重新审批。
	Approvals []EnvApproval `json:"approvals,omitempty"`

	// Baseline 是领取部署时拍下的“执行前版本”，回退与查询都以它为准。
	Baseline map[string]Version `json:"baseline,omitempty"`

	// Results 是本次租约内每个组件的实际部署结果。
	Results []*DeployResult `json:"results,omitempty"`
	// Rollback 是失败/取消时为已更新组件生成的回退任务。
	Rollback []*RollbackTask `json:"rollback,omitempty"`

	Lease *Lease `json:"lease,omitempty"`

	// EventID 是本环境晋级成功时写出的唯一 outbox 事件，稳定不复用。
	EventID string `json:"event_id,omitempty"`

	PromotedAt time.Time `json:"promoted_at,omitempty"`
	FailedAt   time.Time `json:"failed_at,omitempty"`
	// FailReasons 收集失败组件的实际回执说明。
	FailReasons []string `json:"fail_reasons,omitempty"`
}

// Promotion 是一次跨环境逐级晋级执行（聚合）。
type Promotion struct {
	ID         string `json:"id"`
	TrainID    string `json:"train_id"`
	SnapshotID string `json:"snapshot_id"`

	state   PromotionState
	Attempt int `json:"attempt"`

	// snapshot 是创建时从已放行列车冻结下来的列车快照副本，重试也不更换。
	snapshot map[string]Version

	// envs 按严格顺序排列；前一环境不成功，后一环境始终 blocked。
	envs []*EnvExecution

	// installed 是各环境当前实际安装版本（env -> component -> version），
	// 成功部署/回退回执驱动它变化，供下一次领取拍基线与查询展示“执行后版本”。
	installed map[string]map[string]Version

	createdAt time.Time
	updatedAt time.Time
}

// 访问器。

func (p *Promotion) State() PromotionState        { return p.state }
func (p *Promotion) Snapshot() map[string]Version { return cloneCandidates(p.snapshot) }
func (p *Promotion) CreatedAt() time.Time         { return p.createdAt }
func (p *Promotion) UpdatedAt() time.Time         { return p.updatedAt }
func (p *Promotion) Installed() map[string]map[string]Version {
	out := make(map[string]map[string]Version, len(p.installed))
	for env, m := range p.installed {
		out[env] = cloneCandidates(m)
	}
	return out
}

// Environments 返回各环境执行状态的深拷贝（顺序即严格晋级顺序）。
func (p *Promotion) Environments() []*EnvExecution {
	out := make([]*EnvExecution, len(p.envs))
	for i, e := range p.envs {
		out[i] = cloneEnvExecution(e)
	}
	return out
}

func (p *Promotion) touch(now time.Time) { p.updatedAt = now }

// env 按名称找环境执行；顺序非法名称返回 NotFound。
func (p *Promotion) env(name string) (*EnvExecution, error) {
	for _, e := range p.envs {
		if e.Name == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("%w: environment %q is not part of promotion %s", ErrNotFound, name, p.ID)
}

// currentEnv 返回第一个尚未晋级的环境（即当前唯一允许审批/部署的环境）。
func (p *Promotion) currentEnv() *EnvExecution {
	for _, e := range p.envs {
		if e.State != EnvPromoted {
			return e
		}
	}
	return nil
}

// isOpen 判断 env 是否是当前开放环境（前面的环境都已晋级）。
func (p *Promotion) isOpen(env *EnvExecution) bool {
	cur := p.currentEnv()
	return cur != nil && cur == env
}

// missingApprovals 按环境策略快照统计尚缺的审批角色（同一人同角色只计一次）。
func (e *EnvExecution) missingApprovals() []string {
	counts := map[string]map[string]struct{}{}
	for _, a := range e.Approvals {
		if counts[a.Role] == nil {
			counts[a.Role] = map[string]struct{}{}
		}
		counts[a.Role][a.Person] = struct{}{}
	}
	var missing []string
	for _, rule := range e.Policy.Rules {
		if got := len(counts[rule.Role]); got < rule.Need {
			missing = append(missing, fmt.Sprintf("role %q needs %d approval(s), got %d", rule.Role, rule.Need, got))
		}
	}
	sort.Strings(missing)
	return missing
}

func (e *EnvExecution) hasApproval(person, role string) bool {
	for _, a := range e.Approvals {
		if a.Person == person && a.Role == role {
			return true
		}
	}
	return false
}

func (e *EnvExecution) findResult(component string) *DeployResult {
	if i := e.indexResult(component); i >= 0 {
		return e.Results[i]
	}
	return nil
}

func (e *EnvExecution) indexResult(component string) int {
	for i, r := range e.Results {
		if r.Component == component {
			return i
		}
	}
	return -1
}

func (e *EnvExecution) findRollbackTask(component string) *RollbackTask {
	for _, t := range e.Rollback {
		if t.Component == component {
			return t
		}
	}
	return nil
}

// BlockingReason 返回当前阻断原因（无阻断时返回空串）。
func (p *Promotion) BlockingReason() string {
	switch p.state {
	case PromoPromoted:
		return ""
	case PromoCancelled:
		return "promotion cancelled"
	case PromoCancelling:
		if cur := p.currentEnv(); cur != nil {
			done, total := rollbackProgress(cur)
			return fmt.Sprintf("cancelling: rollback of environment %q in progress (%d/%d components restored)",
				cur.Name, done, total)
		}
		return "cancelling"
	case PromoFailed:
		cur := p.currentEnv()
		if cur == nil {
			return ""
		}
		return fmt.Sprintf("attempt %d failed at environment %q: %v; fix the issue and start a new attempt (retry reuses snapshot %s)",
			p.Attempt, cur.Name, cur.FailReasons, p.SnapshotID)
	}

	// running
	cur := p.currentEnv()
	if cur == nil {
		return ""
	}
	switch cur.State {
	case EnvBlocked, EnvAwaitingApproval:
		return fmt.Sprintf("environment %q is waiting for approvals: %v", cur.Name, cur.missingApprovals())
	case EnvReady:
		return fmt.Sprintf("environment %q approval gate met, waiting for a worker to claim deployment", cur.Name)
	case EnvDeploying:
		return fmt.Sprintf("environment %q deployment in progress: %d/%d components reported, lease epoch %d held by %q",
			cur.Name, len(cur.Results), len(p.snapshot), cur.Lease.Epoch, cur.Lease.Worker)
	case EnvRollingBack:
		done, total := rollbackProgress(cur)
		failed := []string{}
		for _, t := range cur.Rollback {
			if t.Status == RollbackFailed {
				failed = append(failed, t.Component)
			}
		}
		sort.Strings(failed)
		if len(failed) > 0 {
			return fmt.Sprintf("environment %q rollback in progress (%d/%d restored); failed rollback components: %v",
				cur.Name, done, total, failed)
		}
		return fmt.Sprintf("environment %q rollback in progress: %d/%d components restored", cur.Name, done, total)
	case EnvFailed:
		return fmt.Sprintf("environment %q failed, waiting for rollback to finish", cur.Name)
	}
	return ""
}

// rollbackProgress 返回回退任务 (已成功数, 总数)。
func rollbackProgress(e *EnvExecution) (int, int) {
	done := 0
	for _, t := range e.Rollback {
		if t.Status == RollbackSucceeded {
			done++
		}
	}
	return done, len(e.Rollback)
}

func cloneEnvExecution(e *EnvExecution) *EnvExecution {
	cp := *e
	cp.Policy = PolicySnapshot{
		Rules:     append([]ApprovalRule(nil), e.Policy.Rules...),
		Approvers: append([]Approver(nil), e.Policy.Approvers...),
	}
	if e.Approvals != nil {
		cp.Approvals = append([]EnvApproval(nil), e.Approvals...)
	}
	cp.Baseline = cloneCandidates(e.Baseline)
	if e.Results != nil {
		cp.Results = make([]*DeployResult, len(e.Results))
		for i, r := range e.Results {
			rc := *r
			if r.From != nil {
				v := *r.From
				rc.From = &v
			}
			if r.To != nil {
				v := *r.To
				rc.To = &v
			}
			cp.Results[i] = &rc
		}
	}
	if e.Rollback != nil {
		cp.Rollback = make([]*RollbackTask, len(e.Rollback))
		for i, t := range e.Rollback {
			tc := *t
			if t.To != nil {
				v := *t.To
				tc.To = &v
			}
			cp.Rollback[i] = &tc
		}
	}
	if e.Lease != nil {
		l := *e.Lease
		cp.Lease = &l
	}
	if e.FailReasons != nil {
		cp.FailReasons = append([]string(nil), e.FailReasons...)
	}
	return &cp
}
