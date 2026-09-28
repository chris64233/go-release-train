package releasetrain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// 环境晋级（Environment Promotion）
//
// 放行（release）回答“列车内容是否正确”；环境晋级回答“这列已放行的列车是否已
// 逐环境落地”。一列 released 列车可以创建一次晋级活动（Promotion），活动包含：
//
//   - 创建时冻结的列车快照（之后所有尝试共用，绝不重新取版本）；
//   - 严格有序的环境列表（默认 dev → staging → production）；
//   - 每个环境各自的审批门槛快照。
//
// 一次活动可有多次尝试（Attempt）：部署失败并回退完成后，问题修复可创建新尝试，
// 新尝试跳过已经处于快照版本的环境，其余环境重新审批、重新部署。
//
// 每个环境必须以“完整列车”为单位部署：全部组件成功才算该环境晋级，写且只写一条
// environment.promoted outbox；任一组件失败即如实记录结果并回退该环境中已更新的
// 组件，绝不把混合版本状态记为成功。

// PromotionState 是晋级活动的状态。
type PromotionState string

const (
	// PromActive：当前尝试仍在推进（或失败后等待创建新尝试）。
	PromActive PromotionState = "active"
	// PromSucceeded：所有环境均已晋级，终态。
	PromSucceeded PromotionState = "succeeded"
	// PromCancelRequested：收到取消但某环境尚有部署在途，正在回退；回退完成后转 cancelled。
	PromCancelRequested PromotionState = "cancel_requested"
	// PromCancelled：终态。
	PromCancelled PromotionState = "cancelled"
)

// AttemptStatus 是单次晋级尝试的状态。
type AttemptStatus string

const (
	AttemptRunning     AttemptStatus = "running"      // 仍有环境待部署
	AttemptRollingBack AttemptStatus = "rolling_back" // 某环境部署失败/取消触发回退中
	AttemptFailed      AttemptStatus = "failed"       // 回退完成，本次尝试失败（可创建新尝试）
	AttemptSucceeded   AttemptStatus = "succeeded"    // 全部环境晋级
	AttemptCancelled   AttemptStatus = "cancelled"    // 活动被取消
)

// EnvStatus 是单个环境在一次尝试中的执行状态。
type EnvStatus string

const (
	EnvWaitingApproval EnvStatus = "waiting_approval" // 审批未齐或前序环境未完成
	EnvReady           EnvStatus = "ready"            // 审批已齐且前序环境已晋级，等待工作者领取
	EnvDeploying       EnvStatus = "deploying"        // 已有工作者持租约部署中
	EnvPromoted        EnvStatus = "promoted"         // 全部组件部署成功
	EnvRollingBack     EnvStatus = "rolling_back"     // 部分组件失败，回退已更新组件中
	EnvFailed          EnvStatus = "failed"           // 回退完成（仅用于历史尝试展示）
)

// RollbackUnitState 是单个组件回退单元的状态。
type RollbackUnitState string

const (
	RollbackPending   RollbackUnitState = "pending"
	RollbackLeased    RollbackUnitState = "leased"
	RollbackSucceeded RollbackUnitState = "succeeded"
	RollbackFailed    RollbackUnitState = "failed" // 回执失败，可被再次领取重试
)

// EnvPolicy 是一个环境的审批门槛：角色规则 + 有资格人员名单（与列车审批策略同构）。
// Rules 为空表示该环境无需审批，审批门槛自动敞开。
type EnvPolicy struct {
	Name      string         `json:"name"`
	Rules     []ApprovalRule `json:"rules"`
	Approvers []Approver     `json:"approvers"`
}

// EnvironmentPolicy 定义环境的严格顺序及各自审批门槛。
// Environments 的切片顺序即晋级顺序。
type EnvironmentPolicy struct {
	Environments []EnvPolicy `json:"environments"`
}

// defaultEnvironmentPolicy 是内置的严格顺序 dev → staging → production，
// 默认三个环境均无审批门槛；可通过 PutEnvironmentPolicy 整体替换（含收紧门槛）。
func defaultEnvironmentPolicy() *EnvironmentPolicy {
	names := []string{"dev", "staging", "production"}
	envs := make([]EnvPolicy, 0, len(names))
	for _, n := range names {
		envs = append(envs, EnvPolicy{Name: n})
	}
	return &EnvironmentPolicy{Environments: envs}
}

// validateEnvironmentPolicy 校验环境顺序与各环境审批门槛：
// 至少一个环境、名称非空且不重复；每个环境的规则/人员满足策略校验。
func validateEnvironmentPolicy(p *EnvironmentPolicy) error {
	if p == nil || len(p.Environments) == 0 {
		return fmt.Errorf("%w: at least one environment is required", ErrInvalidArgument)
	}
	seen := map[string]struct{}{}
	for _, e := range p.Environments {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return fmt.Errorf("%w: environment name is required", ErrInvalidArgument)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("%w: environment %q listed twice", ErrInvalidArgument, name)
		}
		seen[name] = struct{}{}
		if err := validatePolicy(e.Rules, e.Approvers); err != nil {
			return fmt.Errorf("%w: environment %q: %v", ErrInvalidArgument, name, err)
		}
	}
	return nil
}

// EnvApproval 记录某环境的一次审批。
type EnvApproval struct {
	Person    string    `json:"person"`
	Role      string    `json:"role"`
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id,omitempty"`
}

// ComponentDeployResult 是单个组件的部署回执（实际结果，失败也如实保留）。
type ComponentDeployResult struct {
	Component       string    `json:"component"`
	Success         bool      `json:"success"`
	DeployedVersion *Version  `json:"deployed_version,omitempty"` // 工作者回报的实际版本；失败时可为空
	Message         string    `json:"message,omitempty"`
	LeaseNo         int       `json:"lease_no"` // 接收该回执的租约号（接管 fencing 依据）
	At              time.Time `json:"at"`
	RequestID       string    `json:"request_id,omitempty"`
}

// RollbackUnit 是一个待回退组件：把该组件恢复到本次环境部署前的版本 ToVersion。
// ToVersion 为 nil 表示部署前该环境没有此组件（回退即下线）。
type RollbackUnit struct {
	Component   string            `json:"component"`
	ToVersion   *Version          `json:"to_version,omitempty"`
	State       RollbackUnitState `json:"state"`
	LeaseNo     int               `json:"lease_no"`
	Worker      string            `json:"worker,omitempty"`
	LeasedAt    time.Time         `json:"leased_at,omitempty"`
	Attempts    int               `json:"attempts"` // 被领取次数（含失败重试）
	LastMessage string            `json:"last_message,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at,omitempty"`
	SucceededAt time.Time         `json:"succeeded_at,omitempty"`
}

// EnvExecution 是一次尝试中一个环境的执行记录。
type EnvExecution struct {
	Name   string    `json:"name"`
	Status EnvStatus `json:"status"`

	// policy 是活动创建时冻结的该环境审批门槛快照。
	policy EnvPolicy
	// approvals 是本次尝试中该环境收到的审批（新尝试重新计票）。
	approvals []EnvApproval

	// beforeVersions 是首次被领取部署时该环境各组件的现存版本（component -> *Version）。
	// nil 指针表示该环境此前没有该组件；它是回退的恢复目标。
	beforeVersions map[string]*Version

	results []ComponentDeployResult

	// 部署租约：每次领取（含接管）单调递增 leaseNo，只有当前租约可提交回执。
	leaseNo     int
	leaseWorker string
	leasedAt    time.Time

	promotedAt time.Time
	failedAt   time.Time
	eventID    string // 该环境成功时写出的唯一 outbox 事件 ID

	// carriedFromAttempt > 0 表示本环境在新尝试中被识别为“上一次尝试已晋级、
	// 现网版本与快照一致”，直接继承而不重复部署、不重复写 outbox。
	carriedFromAttempt int

	rollback []*RollbackUnit
}

// Attempt 是一次晋级尝试。同一活动的多次尝试共用 Promotion 上的快照与环境定义。
type Attempt struct {
	No         int           `json:"no"`
	Status     AttemptStatus `json:"status"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at,omitempty"`
	envs       []*EnvExecution
}

// Promotion 是环境晋级活动聚合。
type Promotion struct {
	ID      string `json:"id"`
	TrainID string `json:"train_id"`
	state   PromotionState
	Version int `json:"version"`

	// snapshot / snapshotID 在创建时从已放行列车复制，之后所有尝试共用，不再变化。
	snapshot   map[string]Version
	snapshotID string

	// environments 为严格顺序；envPolicies 为创建时冻结的各环境审批门槛。
	environments []string
	envPolicies  map[string]EnvPolicy

	attempts []*Attempt

	createdAt   time.Time
	updatedAt   time.Time
	succeededAt time.Time
	cancelledAt time.Time
}

// 访问器。

func (p *Promotion) State() PromotionState        { return p.state }
func (p *Promotion) Snapshot() map[string]Version { return cloneCandidates(p.snapshot) }
func (p *Promotion) SnapshotID() string           { return p.snapshotID }
func (p *Promotion) Environments() []string       { return append([]string(nil), p.environments...) }
func (p *Promotion) EnvPolicy(name string) (EnvPolicy, bool) {
	pol, ok := p.envPolicies[name]
	return pol, ok
}
func (p *Promotion) Attempts() []*Attempt       { return append([]*Attempt(nil), p.attempts...) }
func (p *Promotion) CurrentAttempt() *Attempt   { return p.currentAttempt() }
func (p *Promotion) CreatedAt() time.Time       { return p.createdAt }
func (p *Promotion) UpdatedAt() time.Time       { return p.updatedAt }
func (p *Promotion) SucceededAtTime() time.Time { return p.succeededAt }
func (p *Promotion) CancelledAtTime() time.Time { return p.cancelledAt }

func (a *Attempt) Envs() []*EnvExecution { return append([]*EnvExecution(nil), a.envs...) }

func (e *EnvExecution) Policy() EnvPolicy        { return e.policy }
func (e *EnvExecution) Approvals() []EnvApproval { return append([]EnvApproval(nil), e.approvals...) }
func (e *EnvExecution) BeforeVersions() map[string]*Version {
	return cloneVersionPtrs(e.beforeVersions)
}
func (e *EnvExecution) Results() []ComponentDeployResult {
	return append([]ComponentDeployResult(nil), e.results...)
}
func (e *EnvExecution) RollbackUnits() []*RollbackUnit {
	return append([]*RollbackUnit(nil), e.rollback...)
}
func (e *EnvExecution) LeaseNo() int            { return e.leaseNo }
func (e *EnvExecution) LeaseWorker() string     { return e.leaseWorker }
func (e *EnvExecution) LeasedAt() time.Time     { return e.leasedAt }
func (e *EnvExecution) PromotedAt() time.Time   { return e.promotedAt }
func (e *EnvExecution) FailedAt() time.Time     { return e.failedAt }
func (e *EnvExecution) EventID() string         { return e.eventID }
func (e *EnvExecution) CarriedFromAttempt() int { return e.carriedFromAttempt }

func (u *RollbackUnit) ToVersionPtr() *Version { return u.ToVersion }

func cloneVersionPtrs(in map[string]*Version) map[string]*Version {
	if in == nil {
		return nil
	}
	out := make(map[string]*Version, len(in))
	for k, v := range in {
		if v != nil {
			cp := *v
			out[k] = &cp
		} else {
			out[k] = nil
		}
	}
	return out
}

// ---- 聚合行为 ----

func newPromotion(id, trainID string, snap map[string]Version, snapshotID string,
	policy *EnvironmentPolicy, now time.Time) *Promotion {
	p := &Promotion{
		ID:          id,
		TrainID:     trainID,
		state:       PromActive,
		Version:     1,
		snapshot:    cloneCandidates(snap),
		snapshotID:  snapshotID,
		envPolicies: map[string]EnvPolicy{},
		createdAt:   now,
		updatedAt:   now,
	}
	for _, e := range policy.Environments {
		p.environments = append(p.environments, e.Name)
		// 深拷贝门槛快照。
		p.envPolicies[e.Name] = EnvPolicy{
			Name:      e.Name,
			Rules:     append([]ApprovalRule(nil), e.Rules...),
			Approvers: append([]Approver(nil), e.Approvers...),
		}
	}
	p.attempts = []*Attempt{p.newAttempt(1, now, nil, nil)}
	return p
}

// newAttempt 构建一次尝试。live 为当前各环境的实际部署版本表：
// 已经完整处于快照版本的环境直接继承为 promoted（不重复部署、不重复出 outbox）。
func (p *Promotion) newAttempt(no int, now time.Time,
	live map[string]map[string]Version, carriedEventID map[string]prevPromotion) *Attempt {
	a := &Attempt{No: no, Status: AttemptRunning, StartedAt: now}
	for _, name := range p.environments {
		e := &EnvExecution{Name: name, Status: EnvWaitingApproval, policy: p.envPolicies[name]}
		if live != nil && p.envAtSnapshot(name, live) {
			if prev, ok := carriedEventID[name]; ok {
				e.Status = EnvPromoted
				e.eventID = prev.eventID
				e.promotedAt = prev.promotedAt
				e.carriedFromAttempt = prev.attemptNo
			}
		}
		a.envs = append(a.envs, e)
	}
	p.recomputeReadiness(a)
	return a
}

// prevPromotion 记录某环境此前晋级的事件与时间，供新尝试继承。
type prevPromotion struct {
	attemptNo  int
	eventID    string
	promotedAt time.Time
}

// envAtSnapshot 判断某环境当前所有组件是否都已是快照版本。
func (p *Promotion) envAtSnapshot(env string, live map[string]map[string]Version) bool {
	cur := live[env]
	if len(cur) != len(p.snapshot) {
		return false
	}
	for comp, want := range p.snapshot {
		got, ok := cur[comp]
		if !ok || got.Compare(want) != 0 {
			return false
		}
	}
	return true
}

func (p *Promotion) currentAttempt() *Attempt {
	if len(p.attempts) == 0 {
		return nil
	}
	return p.attempts[len(p.attempts)-1]
}

func (p *Promotion) attempt(no int) (*Attempt, error) {
	for _, a := range p.attempts {
		if a.No == no {
			return a, nil
		}
	}
	return nil, fmt.Errorf("%w: promotion %s has no attempt %d", ErrNotFound, p.ID, no)
}

func (a *Attempt) env(name string) (*EnvExecution, error) {
	for _, e := range a.envs {
		if e.Name == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("%w: environment %q is not part of promotion", ErrNotFound, name)
}

func (p *Promotion) touch(now time.Time) {
	p.Version++
	p.updatedAt = now
}

// requireActiveRunning 断言活动仍 active 且当前尝试可推进。
func (p *Promotion) requireActiveRunning() (*Attempt, error) {
	if p.state != PromActive {
		return nil, fmt.Errorf("%w: promotion %s is %s", ErrStateConflict, p.ID, p.state)
	}
	a := p.currentAttempt()
	if a == nil || a.Status != AttemptRunning {
		got := AttemptStatus("")
		if a != nil {
			got = a.Status
		}
		return nil, fmt.Errorf("%w: promotion %s current attempt is %q", ErrStateConflict, p.ID, got)
	}
	return a, nil
}

// recomputeReadiness 依据“审批门槛 + 严格环境顺序”重算各环境是否可领取部署。
// 只有前一环境已 promoted 的环境才可能 ready；deploying/terminal 状态不受影响。
func (p *Promotion) recomputeReadiness(a *Attempt) {
	if p.state != PromActive || a == nil || a.Status != AttemptRunning {
		return
	}
	prevPromoted := true // 第一个环境没有前序
	for _, e := range a.envs {
		switch e.Status {
		case EnvPromoted:
			prevPromoted = true
		case EnvWaitingApproval, EnvReady:
			if prevPromoted && e.gateMet() {
				e.Status = EnvReady
			} else {
				e.Status = EnvWaitingApproval
			}
			prevPromoted = false
		default:
			// deploying / rolling_back / failed：后续环境一律阻断。
			prevPromoted = false
		}
	}
}

// eligible 判断某人在该环境门槛快照中是否拥有指定角色。
func (e *EnvExecution) eligible(person, role string) bool {
	for _, a := range e.policy.Approvers {
		if a.Person == person && a.Role == role {
			return true
		}
	}
	return false
}

func (e *EnvExecution) hasApproval(person, role string) bool {
	for _, a := range e.approvals {
		if a.Person == person && a.Role == role {
			return true
		}
	}
	return false
}

// gateMet 按门槛快照统计每个角色的去重人数，返回尚未满足的角色描述；空切片表示门槛已满足。
func (e *EnvExecution) missingApprovals() []string {
	counts := map[string]map[string]struct{}{}
	for _, a := range e.approvals {
		if counts[a.Role] == nil {
			counts[a.Role] = map[string]struct{}{}
		}
		counts[a.Role][a.Person] = struct{}{}
	}
	var missing []string
	for _, rule := range e.policy.Rules {
		if got := len(counts[rule.Role]); got < rule.Need {
			missing = append(missing, fmt.Sprintf("role %q needs %d approval(s), got %d",
				rule.Role, rule.Need, got))
		}
	}
	sort.Strings(missing)
	return missing
}

func (e *EnvExecution) gateMet() bool { return len(e.missingApprovals()) == 0 }

// Approve 为当前尝试的某环境记录审批。资格只认活动创建时的门槛快照；
// 同一 (person, role) 幂等。只有仍在等待/就绪的环境可审批。
func (p *Promotion) Approve(envName, person, role, requestID string, now time.Time) (bool, error) {
	a, err := p.requireActiveRunning()
	if err != nil {
		return false, err
	}
	e, err := a.env(envName)
	if err != nil {
		return false, err
	}
	switch e.Status {
	case EnvWaitingApproval, EnvReady:
	default:
		return false, fmt.Errorf("%w: environment %s is %s, approval no longer accepted",
			ErrStateConflict, envName, e.Status)
	}
	if !e.eligible(person, role) {
		return false, fmt.Errorf("%w: %s is not an approver with role %q for environment %q in promotion %s",
			ErrApproval, person, role, envName, p.ID)
	}
	if e.hasApproval(person, role) {
		return false, nil // 幂等
	}
	e.approvals = append(e.approvals, EnvApproval{Person: person, Role: role, At: now, RequestID: requestID})
	p.touch(now)
	p.recomputeReadiness(a)
	return true, nil
}

// DeploymentLease 是工作者领取部署得到的租约。
type DeploymentLease struct {
	AttemptNo   int
	LeaseNo     int
	Worker      string
	Environment string
	Targets     map[string]Version // 完整列车快照：必须以整列车为单位部署
	Before      map[string]*Version
	TakenOver   bool // 是否接管了已有租约
}

// ClaimDeployment 领取某环境的部署租约。
// 仅当该环境审批门槛已满足、前一环境已晋级且本环境尚未完成时可领取；
// 已有活动租约时允许接管：租约号 +1，旧租约的回执将被拒绝（fencing）。
func (p *Promotion) ClaimDeployment(envName, worker string,
	live map[string]map[string]Version, now time.Time) (*DeploymentLease, error) {
	a, err := p.requireActiveRunning()
	if err != nil {
		return nil, err
	}
	e, err := a.env(envName)
	if err != nil {
		return nil, err
	}
	takenOver := false
	switch e.Status {
	case EnvReady:
		e.leaseNo = 1
		// 首次领取：冻结“部署前版本”，作为回退恢复目标。
		e.beforeVersions = map[string]*Version{}
		for comp := range p.snapshot {
			if cur, ok := live[envName][comp]; ok {
				v := cur
				e.beforeVersions[comp] = &v
			} else {
				e.beforeVersions[comp] = nil
			}
		}
	case EnvDeploying:
		// 显式接管（如原工作者疑似宕机）：租约号递增作 fencing token。
		// 旧租约下记录的部分回执不再可信，按新租约重新部署完整列车；
		// beforeVersions 保持首次领取时冻结的值，仍是回退恢复目标。
		e.leaseNo++
		kept := e.results[:0]
		for _, r := range e.results {
			if r.LeaseNo >= e.leaseNo {
				kept = append(kept, r)
			}
		}
		e.results = kept
		takenOver = true
	default:
		return nil, fmt.Errorf("%w: environment %s is %s, deployment is not claimable",
			ErrStateConflict, envName, e.Status)
	}
	e.Status = EnvDeploying
	e.leaseWorker = worker
	e.leasedAt = now
	p.touch(now)
	return &DeploymentLease{
		AttemptNo:   a.No,
		LeaseNo:     e.leaseNo,
		Worker:      worker,
		Environment: envName,
		Targets:     cloneCandidates(p.snapshot),
		Before:      cloneVersionPtrs(e.beforeVersions),
		TakenOver:   takenOver,
	}, nil
}

// ReceiptInput 是一个组件的部署回执。
type ReceiptInput struct {
	Component       string
	Success         bool
	DeployedVersion *Version // 可选；成功且省略时取快照目标版本
	Message         string
	RequestID       string
}

// SubmitReceipts 提交一批组件部署回执。
// 只有“当前尝试 + 当前租约”可以提交：旧尝试或被接管旧租约的回执一律拒绝，
// 不能覆盖接管后的状态，也不能越过尚未完成的前一环境（领取阶段已强制顺序）。
// 同一租约对同一组件的重复回执幂等，返回既有结果。
//
// 当全部组件都有回执时收尾：全部成功 → 环境 promoted 并写唯一 outbox（由 service 层
// 追加，promotedNow 返回 true）；任一失败 → 如实记录并启动该环境回退。
//
// live 是“环境 → 组件 → 当前实际部署版本”的现网版本表，与状态迁移在同一事务内更新。
func (p *Promotion) SubmitReceipts(attemptNo, leaseNo int, envName string,
	receipts []ReceiptInput, live map[string]map[string]Version, now time.Time) (promotedNow bool, err error) {
	a, err := p.requireActiveRunning()
	if err != nil {
		return false, err
	}
	if a.No != attemptNo {
		return false, fmt.Errorf("%w: receipt is for attempt %d, current attempt is %d",
			ErrStateConflict, attemptNo, a.No)
	}
	e, err := a.env(envName)
	if err != nil {
		return false, err
	}
	if e.Status != EnvDeploying {
		// 环境已 promoted/rolling_back 等：若整批回执都已在同一租约记录过，按幂等重放。
		if p.allReceiptsAlreadyRecorded(e, leaseNo, receipts) {
			return e.Status == EnvPromoted && e.eventID != "", nil
		}
		return false, fmt.Errorf("%w: environment %s is %s, receipts are no longer accepted",
			ErrStateConflict, envName, e.Status)
	}
	if e.leaseNo != leaseNo {
		// 含“旧租约在接管后到达”与“新租约号伪造”两种情况。
		return false, fmt.Errorf("%w: stale lease %d for environment %s, current lease is %d",
			ErrStateConflict, leaseNo, envName, e.leaseNo)
	}
	if len(receipts) == 0 {
		return false, fmt.Errorf("%w: at least one deployment receipt is required", ErrInvalidArgument)
	}

	changed := false
	for _, r := range receipts {
		target, ok := p.snapshot[r.Component]
		if !ok {
			return false, fmt.Errorf("%w: component %s is not part of train snapshot %s",
				ErrInvalidArgument, r.Component, p.snapshotID)
		}
		// 成功回执若显式回报了版本，必须就是租约下发的快照目标版本；
		// 部署成别的版本不能算成功（否则会把混合版本状态误记为晋级）。
		if r.Success && r.DeployedVersion != nil && r.DeployedVersion.Compare(target) != 0 {
			return false, fmt.Errorf("%w: component %s reported success at %s, but snapshot target is %s",
				ErrInvalidArgument, r.Component, r.DeployedVersion, target)
		}
		if existing := e.findResult(r.Component); existing != nil {
			if existing.LeaseNo != leaseNo {
				// 接管后新租约不能替旧租约改结果，旧租约也不能再写该组件。
				return false, fmt.Errorf("%w: receipt for component %s was already recorded under lease %d",
					ErrStateConflict, r.Component, existing.LeaseNo)
			}
			continue // 同一租约重复回执：幂等保留首个结果
		}
		res := ComponentDeployResult{
			Component: r.Component,
			Success:   r.Success,
			Message:   r.Message,
			LeaseNo:   leaseNo,
			At:        now,
			RequestID: r.RequestID,
		}
		if r.DeployedVersion != nil {
			v := *r.DeployedVersion
			res.DeployedVersion = &v
		} else if r.Success {
			v := p.snapshot[r.Component]
			res.DeployedVersion = &v
		}
		e.results = append(e.results, res)
		changed = true
	}
	if changed {
		p.touch(now)
	}

	// 全部组件都有回执才允许收尾——绝不允许把缺组件的混合状态当成功。
	if len(e.results) < len(p.snapshot) {
		return false, nil
	}
	sort.Slice(e.results, func(i, j int) bool { return e.results[i].Component < e.results[j].Component })

	allOK := true
	for _, r := range e.results {
		if !r.Success {
			allOK = false
			break
		}
	}
	if allOK {
		return p.finalizePromotion(a, e, live, now)
	}
	p.startRollback(a, e, now)
	return false, nil
}

func (p *Promotion) allReceiptsAlreadyRecorded(e *EnvExecution, leaseNo int, receipts []ReceiptInput) bool {
	if len(receipts) == 0 {
		return false
	}
	for _, r := range receipts {
		existing := e.findResult(r.Component)
		if existing == nil || existing.LeaseNo != leaseNo {
			return false
		}
	}
	return true
}

func (e *EnvExecution) findResult(component string) *ComponentDeployResult {
	for i := range e.results {
		if e.results[i].Component == component {
			return &e.results[i]
		}
	}
	return nil
}

// finalizePromotion 把环境记为晋级、原子更新现网版本，并在最后一个环境时收尾活动。
// 返回 promotedNow=true 且设置 e.eventID，service 层负责据此追加 outbox 事件。
func (p *Promotion) finalizePromotion(a *Attempt, e *EnvExecution,
	live map[string]map[string]Version, now time.Time) (bool, error) {
	if e.Status == EnvPromoted {
		return false, nil
	}
	e.Status = EnvPromoted
	e.promotedAt = now
	e.eventID = "evt_" + newID()
	e.leaseWorker = ""
	if live[e.Name] == nil {
		live[e.Name] = map[string]Version{}
	}
	for comp, ver := range p.snapshot {
		live[e.Name][comp] = ver
	}
	p.touch(now)
	p.recomputeReadiness(a)

	// 严格顺序下最后一个环境晋级 => 尝试与活动成功。
	allPromoted := true
	for _, other := range a.envs {
		if other.Status != EnvPromoted {
			allPromoted = false
			break
		}
	}
	if allPromoted {
		a.Status = AttemptSucceeded
		a.FinishedAt = now
		p.state = PromSucceeded
		p.succeededAt = now
		p.touch(now)
	}
	return true, nil
}

// startRollback 如实记录失败，并为“已更新（部署成功）”的组件创建回退单元，
// 恢复目标是该环境执行前的版本。没有任何成功组件时回退立即完成。
func (p *Promotion) startRollback(a *Attempt, e *EnvExecution, now time.Time) {
	e.Status = EnvRollingBack
	e.failedAt = now
	a.Status = AttemptRollingBack
	for _, r := range e.results {
		if !r.Success {
			continue
		}
		var to *Version
		if before := e.beforeVersions[r.Component]; before != nil {
			v := *before
			to = &v
		}
		e.rollback = append(e.rollback, &RollbackUnit{
			Component: r.Component,
			ToVersion: to,
			State:     RollbackPending,
		})
	}
	sort.Slice(e.rollback, func(i, j int) bool { return e.rollback[i].Component < e.rollback[j].Component })
	p.touch(now)
	if len(e.rollback) == 0 {
		p.finishRollbackIfComplete(a, now)
	}
}

// RollbackLease 是工作者领取回退单元得到的租约。
type RollbackLease struct {
	AttemptNo   int
	Environment string
	Component   string
	LeaseNo     int
	Worker      string
	FromVersion *Version // 当前（错误地）部署的版本
	ToVersion   *Version // 要恢复到的部署前版本；nil 表示下线
}

// ClaimRollback 领取下一个待回退（或上次回报失败可重试）的单元。
// 每个单元有独立租约号；只有持当前租约可提交回退回执。
// 没有任何待回退单元（回退从未开始或已全部完成）时返回 ErrNotFound。
func (p *Promotion) ClaimRollback(worker string, now time.Time) (*RollbackLease, error) {
	a := p.currentAttempt()
	if a == nil {
		return nil, fmt.Errorf("%w: promotion %s has no attempts", ErrNotFound, p.ID)
	}
	for _, e := range a.envs {
		if e.Status != EnvRollingBack {
			continue
		}
		for _, u := range e.rollback {
			if u.State != RollbackPending && u.State != RollbackFailed {
				continue
			}
			u.State = RollbackLeased
			u.LeaseNo++
			u.Worker = worker
			u.LeasedAt = now
			u.Attempts++
			u.UpdatedAt = now
			p.touch(now)
			from := p.snapshot[u.Component]
			return &RollbackLease{
				AttemptNo:   a.No,
				Environment: e.Name,
				Component:   u.Component,
				LeaseNo:     u.LeaseNo,
				Worker:      worker,
				FromVersion: &from,
				ToVersion:   u.ToVersion,
			}, nil
		}
	}
	return nil, fmt.Errorf("%w: no pending rollback unit in promotion %s", ErrNotFound, p.ID)
}

// ReportRollback 提交单个回退单元的结果。成功即把现网表恢复到部署前版本；
// 失败的单元保持 failed 可被重新领取。全部单元成功后本次尝试落为 failed
// （或取消流程落为 cancelled）。
func (p *Promotion) ReportRollback(attemptNo, leaseNo int, envName, component string,
	success bool, message string, live map[string]map[string]Version, now time.Time) (bool, error) {
	a := p.currentAttempt()
	if a == nil {
		return false, fmt.Errorf("%w: promotion %s has no attempts", ErrStateConflict, p.ID)
	}
	if a.No != attemptNo {
		return false, fmt.Errorf("%w: rollback receipt is for attempt %d, current attempt is %d",
			ErrStateConflict, attemptNo, a.No)
	}
	e, err := a.env(envName)
	if err != nil {
		return false, err
	}
	if e.Status != EnvRollingBack {
		return false, fmt.Errorf("%w: environment %s is not rolling back", ErrStateConflict, envName)
	}
	var unit *RollbackUnit
	for _, u := range e.rollback {
		if u.Component == component {
			unit = u
			break
		}
	}
	if unit == nil {
		return false, fmt.Errorf("%w: no rollback unit for component %s in environment %s",
			ErrNotFound, component, envName)
	}
	if unit.State != RollbackLeased || unit.LeaseNo != leaseNo {
		return false, fmt.Errorf("%w: stale rollback lease %d for %s/%s, current is %d (%s)",
			ErrStateConflict, leaseNo, envName, component, unit.LeaseNo, unit.State)
	}
	unit.LastMessage = message
	unit.UpdatedAt = now
	if !success {
		unit.State = RollbackFailed
		unit.Worker = ""
		p.touch(now)
		return false, nil
	}
	unit.State = RollbackSucceeded
	unit.SucceededAt = now
	// 把该组件在现网表中恢复到部署前版本；nil 表示此前不存在，回退即移除。
	if live[envName] == nil {
		live[envName] = map[string]Version{}
	}
	if unit.ToVersion != nil {
		live[envName][component] = *unit.ToVersion
	} else {
		delete(live[envName], component)
	}
	p.touch(now)
	p.finishRollbackIfComplete(a, now)
	return e.Status != EnvRollingBack, nil
}

// finishRollbackIfComplete 在所有回退单元成功后落定尝试/活动状态。
func (p *Promotion) finishRollbackIfComplete(a *Attempt, now time.Time) {
	for _, e := range a.envs {
		if e.Status != EnvRollingBack {
			continue
		}
		// 没有任何回退单元（全部组件部署失败、无组件被更新）视为已恢复完成。
		done := true
		for _, u := range e.rollback {
			if u.State != RollbackSucceeded {
				done = false
				break
			}
		}
		if !done {
			continue
		}
		e.Status = EnvFailed
	}
	// 严格顺序下同一时刻至多一个环境在回退；检查是否全部结束。
	allDone := true
	for _, e := range a.envs {
		if e.Status == EnvRollingBack {
			allDone = false
			break
		}
	}
	if !allDone || a.Status != AttemptRollingBack {
		return
	}
	a.FinishedAt = now
	if p.state == PromCancelRequested {
		a.Status = AttemptCancelled
		p.state = PromCancelled
		p.cancelledAt = now
	} else {
		a.Status = AttemptFailed
	}
	p.touch(now)
}

// Cancel 取消晋级活动。
//   - 已终态（succeeded/cancelled）：succeeded 取消报冲突，重复取消幂等；
//   - 没有部署中的环境：立即 cancelled；
//   - 有环境部署中且已有组件更新：转入 cancel_requested 并对该环境启动回退，
//     回退完成后才 cancelled，绝不遗留混合版本且不记为成功。
func (p *Promotion) Cancel(now time.Time) (rollbackStarted bool, err error) {
	switch p.state {
	case PromCancelled, PromCancelRequested:
		return false, nil // 幂等
	case PromSucceeded:
		return false, fmt.Errorf("%w: promotion %s already succeeded", ErrStateConflict, p.ID)
	}
	a := p.currentAttempt()
	// 部署失败触发的回退仍在进行：取消在回退完成时生效（finishRollbackIfComplete
	// 看到 cancel_requested 会把尝试落为 cancelled 而不是 failed）。
	if a != nil && a.Status == AttemptRollingBack {
		p.state = PromCancelRequested
		p.touch(now)
		return true, nil
	}
	var deploying *EnvExecution
	if a != nil && a.Status == AttemptRunning {
		for _, e := range a.envs {
			if e.Status == EnvDeploying {
				deploying = e
				break
			}
		}
	}
	if deploying != nil && hasSuccessfulResult(deploying) {
		// 有组件已经更新：必须回退，取消在回退完成后才生效。
		p.state = PromCancelRequested
		p.startRollback(a, deploying, now)
		// startRollback 可能因无成功单元而立即把活动落为 cancelled/failed；
		// 此分支 hasSuccessfulResult 为真，至少一个单元，cancel_requested 得以保留。
		if p.state == PromCancelRequested {
			return true, nil
		}
	}
	// 无在途更新（含租约已发但零成功回执）：租约即刻失效，直接取消。
	a.Status = AttemptCancelled
	a.FinishedAt = now
	p.state = PromCancelled
	p.cancelledAt = now
	p.touch(now)
	return false, nil
}

func hasSuccessfulResult(e *EnvExecution) bool {
	for _, r := range e.results {
		if r.Success {
			return true
		}
	}
	return false
}

// StartNewAttempt 在最近一次尝试失败（回退完成）后创建新尝试。
// 新尝试必须继续使用原列车快照；已处于快照版本的环境直接继承。
// 返回新尝试号。
func (p *Promotion) StartNewAttempt(live map[string]map[string]Version, now time.Time) (int, error) {
	if p.state != PromActive {
		return 0, fmt.Errorf("%w: promotion %s is %s", ErrStateConflict, p.ID, p.state)
	}
	a := p.currentAttempt()
	if a == nil || a.Status != AttemptFailed {
		got := AttemptStatus("")
		if a != nil {
			got = a.Status
		}
		return 0, fmt.Errorf("%w: promotion %s latest attempt is %q, retry only after a failed attempt",
			ErrStateConflict, p.ID, got)
	}

	// 收集各环境最近一次成功晋级的事件，供新尝试继承（不重复写 outbox）。
	carried := map[string]prevPromotion{}
	for _, old := range p.attempts {
		for _, e := range old.envs {
			if e.Status == EnvPromoted && e.eventID != "" {
				carried[e.Name] = prevPromotion{
					attemptNo:  old.No,
					eventID:    e.eventID,
					promotedAt: e.promotedAt,
				}
			}
		}
	}
	next := p.newAttempt(len(p.attempts)+1, now, live, carried)
	p.attempts = append(p.attempts, next)
	p.touch(now)
	return next.No, nil
}
