package releasetrain

import (
	"encoding/json"
	"fmt"
	"time"
)

// validatePromotionPolicy 校验环境顺序与各环境审批门槛：
// 至少一个环境、名称非空且不重复；每个环境的审批策略合法。
func validatePromotionPolicy(pol *PromotionPolicy) error {
	if pol == nil || len(pol.Environments) == 0 {
		return fmt.Errorf("%w: promotion policy requires at least one environment", ErrInvalidArgument)
	}
	seen := map[string]struct{}{}
	for _, e := range pol.Environments {
		if e.Name == "" {
			return fmt.Errorf("%w: environment name is required", ErrInvalidArgument)
		}
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate environment %q in promotion policy", ErrInvalidArgument, e.Name)
		}
		seen[e.Name] = struct{}{}
		if err := validatePolicy(e.Rules, e.Approvers); err != nil {
			return fmt.Errorf("%w: environment %q: %v", ErrInvalidArgument, e.Name, err)
		}
	}
	return nil
}

// newPromotion 创建一次晋级执行：冻结列车快照副本、环境顺序与各环境审批策略快照。
// 第一个环境立即进入审批等待（无规则时直接 ready），其余环境 blocked。
func newPromotion(id, trainID, snapshotID string, snapshot map[string]Version, pol PromotionPolicy, now time.Time) *Promotion {
	p := &Promotion{
		ID:         id,
		TrainID:    trainID,
		SnapshotID: snapshotID,
		state:      PromoRunning,
		Attempt:    1,
		snapshot:   cloneCandidates(snapshot),
		installed:  map[string]map[string]Version{},
		createdAt:  now,
		updatedAt:  now,
	}
	for i, ep := range pol.Environments {
		e := &EnvExecution{
			Name:    ep.Name,
			Order:   i,
			State:   EnvBlocked,
			Attempt: 1,
			Policy: PolicySnapshot{
				Rules:     append([]ApprovalRule(nil), ep.Rules...),
				Approvers: append([]Approver(nil), ep.Approvers...),
			},
		}
		if i == 0 {
			e.State = EnvAwaitingApproval
			if len(e.missingApprovals()) == 0 {
				e.State = EnvReady
			}
		}
		p.envs = append(p.envs, e)
	}
	return p
}

// requirePromotionState 断言晋级整体状态。
func (p *Promotion) requirePromotionState(want ...PromotionState) error {
	for _, s := range want {
		if p.state == s {
			return nil
		}
	}
	return fmt.Errorf("%w: promotion %s is %s", ErrStateConflict, p.ID, p.state)
}

// requireOpenEnv 找到环境并断言它是当前唯一开放环境（前序环境都已晋级）。
func (p *Promotion) requireOpenEnv(name string) (*EnvExecution, error) {
	e, err := p.env(name)
	if err != nil {
		return nil, err
	}
	if !p.isOpen(e) {
		var prev string
		if cur := p.currentEnv(); cur != nil {
			prev = fmt.Sprintf("; current open environment is %q", cur.Name)
		}
		return nil, fmt.Errorf("%w: environment %q is not open: previous environment not promoted yet%s",
			ErrStateConflict, name, prev)
	}
	return e, nil
}

// ---- 环境审批 ----

// ApproveEnv 为当前开放环境记录一次审批。资格只认创建晋级时冻结的策略快照；
// 同一 (person, role) 在同一尝试内幂等。新尝试需要重新审批。
func (p *Promotion) ApproveEnv(envName, person, role string, now time.Time) (bool, error) {
	if err := p.requirePromotionState(PromoRunning); err != nil {
		return false, err
	}
	e, err := p.requireOpenEnv(envName)
	if err != nil {
		return false, err
	}
	if e.State != EnvAwaitingApproval && e.State != EnvReady {
		return false, fmt.Errorf("%w: environment %q is %s, not accepting approvals",
			ErrStateConflict, envName, e.State)
	}
	if !e.Policy.eligibleRole(person, role) {
		return false, fmt.Errorf("%w: %s is not an approver with role %q for environment %q",
			ErrApproval, person, role, envName)
	}
	if e.hasApproval(person, role) {
		return false, nil
	}
	e.Approvals = append(e.Approvals, EnvApproval{Person: person, Role: role, At: now})
	if e.State == EnvAwaitingApproval && len(e.missingApprovals()) == 0 {
		e.State = EnvReady // 审批门槛满足，等待领取部署
	}
	p.touch(now)
	return true, nil
}

// ---- 部署领取与回执 ----

// DeployLease 是领取部署的返回：租约 token、尝试号、接管代次与本次部署目标。
type DeployLease struct {
	Token      string            `json:"token"`
	Attempt    int               `json:"attempt"`
	Epoch      int               `json:"epoch"`
	Worker     string            `json:"worker"`
	Tookover   bool              `json:"tookover"`
	Components map[string]string `json:"components"` // 整列车目标版本（component -> version）
	Baseline   map[string]string `json:"baseline"`   // 该环境执行前版本（缺失表示执行前未安装）
}

// ClaimDeploy 工作者领取当前开放环境的整列车部署。
// ready -> 首次发放（epoch=1，拍下执行前基线）；deploying -> 接管：
// epoch+1、旧租约即刻失效，旧尝试/旧代次的回执不能再写入。
// 基线在整个尝试内固定不变，保证回退始终恢复到“本次尝试执行前”的版本。
func (p *Promotion) ClaimDeploy(envName, worker string, now time.Time) (*DeployLease, error) {
	if err := p.requirePromotionState(PromoRunning); err != nil {
		return nil, err
	}
	e, err := p.requireOpenEnv(envName)
	if err != nil {
		return nil, err
	}
	switch e.State {
	case EnvReady:
		e.State = EnvDeploying
		e.Attempt = p.Attempt
		e.Lease = &Lease{Token: "lease_" + newID(), Epoch: 1, Worker: worker, At: now}
		e.Baseline = cloneCandidates(p.installed[e.Name])
		e.Results = nil
	case EnvDeploying:
		if e.Lease.Worker == worker {
			// 同一工作者重领（安全重试/续租）：原租约继续有效，代次不变。
			break
		}
		// 不同工作者接管：代次递增、旧租约即刻失效；基线保持尝试开始时版本不动。
		e.Lease = &Lease{Token: "lease_" + newID(), Epoch: e.Lease.Epoch + 1, Worker: worker, At: now}
	default:
		return nil, fmt.Errorf("%w: environment %q is %s, cannot claim deployment",
			ErrStateConflict, envName, e.State)
	}
	p.touch(now)
	return p.deployLeaseView(e, worker), nil
}

func (p *Promotion) deployLeaseView(e *EnvExecution, worker string) *DeployLease {
	comps := make(map[string]string, len(p.snapshot))
	for c, v := range p.snapshot {
		comps[c] = v.String()
	}
	base := map[string]string{}
	for c, v := range e.Baseline {
		base[c] = v.String()
	}
	return &DeployLease{
		Token: e.Lease.Token, Attempt: p.Attempt, Epoch: e.Lease.Epoch, Worker: worker,
		Tookover: e.Lease.Epoch > 1, Components: comps, Baseline: base,
	}
}

// DeployReceiptResult 是部署回执的处理结果。
type DeployReceiptResult struct {
	Recorded      *DeployResult `json:"recorded"`
	Idempotent    bool          `json:"idempotent"`               // 重复回执：返回已有记录，未改状态
	EnvPromoted   bool          `json:"env_promoted"`             // 本次（或历史）回执对应环境已晋级
	PromotionDone bool          `json:"promotion_done,omitempty"` // 所有环境完成
	EventID       string        `json:"event_id,omitempty"`       // 该环境唯一 outbox 事件
}

// ReportDeploy 提交一个组件的部署回执。
//   - attempt/token/epoch 必须全部等于当前尝试与当前租约，否则 ErrLease：
//     旧尝试、被接管的旧代次、伪造 token 的回执都不能覆盖状态；
//   - 成功回执的版本必须等于列车快照版本（整列车单位，不接受混版成功）；
//   - 同一租约同一组件的重复回执幂等返回已有结果；
//   - 任一组件失败：记录实际结果并立即把环境转入回退，为所有已偏离基线的
//     组件生成回退任务；混版状态绝不记为成功。
func (p *Promotion) ReportDeploy(envName, token string, attempt, epoch int, component string,
	success bool, to *Version, message string, now time.Time) (*DeployReceiptResult, error) {

	if err := p.requirePromotionState(PromoRunning); err != nil {
		return nil, err
	}
	e, err := p.env(envName)
	if err != nil {
		return nil, err
	}
	// 已晋级环境即使不再是“当前开放环境”，仍要识别其迟到回执：
	// 完成租约的重复回执幂等重放，其余一律按陈旧租约拒绝。
	if e.State == EnvPromoted {
		if r := e.findResult(component); r != nil && r.Token == token && r.Epoch == epoch && e.Attempt == attempt {
			return &DeployReceiptResult{Recorded: r, Idempotent: true, EnvPromoted: true, EventID: e.EventID}, nil
		}
		return nil, fmt.Errorf("%w: environment %q already promoted at attempt %d; stale receipt for %s rejected",
			ErrLease, envName, e.Attempt, component)
	}
	if !p.isOpen(e) {
		return nil, fmt.Errorf("%w: environment %q is not open: previous environment not promoted yet",
			ErrStateConflict, envName)
	}

	if e.State != EnvDeploying {
		// 回退进行中：未记录过的新回执不能改变回退进程；同一租约的已记录回执允许幂等重放。
		if r := e.findResult(component); r != nil && r.Token == token && r.Epoch == epoch && e.Attempt == attempt {
			return &DeployReceiptResult{Recorded: r, Idempotent: true}, nil
		}
		return nil, fmt.Errorf("%w: environment %q is %s, deployment is closed", ErrStateConflict, envName, e.State)
	}

	if attempt != p.Attempt || e.Attempt != attempt {
		return nil, fmt.Errorf("%w: receipt attempt %d is not current (attempt %d)", ErrLease, attempt, p.Attempt)
	}
	if err := e.Lease.check(token, epoch); err != nil {
		return nil, err
	}
	target, ok := p.snapshot[component]
	if !ok {
		return nil, fmt.Errorf("%w: component %s is not in the promotion snapshot", ErrInvalidArgument, component)
	}
	if success {
		if to == nil || *to != target {
			got := "<none>"
			if to != nil {
				got = to.String()
			}
			return nil, fmt.Errorf("%w: successful deployment of %s must report snapshot version %s, got %s",
				ErrInvalidArgument, component, target, got)
		}
	}

	// 同一租约的重复回执：原样返回，不重复计数、不重复推进。
	if existing := e.findResult(component); existing != nil && existing.Token == token && existing.Epoch == epoch {
		return &DeployReceiptResult{Recorded: existing, Idempotent: true}, nil
	}

	var from *Version
	if b, ok := e.Baseline[component]; ok {
		v := b
		from = &v
	}
	r := &DeployResult{
		Component: component, From: from, To: cloneVersionPtr(to), Success: success,
		Message: message, Token: token, Epoch: epoch, At: now,
	}
	// 旧租约残留记录：当前租约接管后重报该组件则就地替换，
	// 保证每个组件在当前租约里只有一条结果。
	if idx := e.indexResult(component); idx >= 0 {
		e.Results[idx] = r
	} else {
		e.Results = append(e.Results, r)
	}

	if success {
		p.setInstalled(e.Name, component, target)
	} else {
		if to != nil {
			p.setInstalled(e.Name, component, *to) // 记录失败时环境的实际版本
		}
		e.FailReasons = append(e.FailReasons, fmt.Sprintf("%s: %s", component, failureMessage(message)))
		// 记录实际结果后启动回退：有组件偏离基线则等待恢复；
		// 尚无任何组件被更新（零偏离）则直接失败，不留无任务的空回退。
		e.State = EnvRollingBack
		if pending := p.prepareRollback(e, now); pending == 0 {
			e.State = EnvFailed
			e.FailedAt = now
			p.state = PromoFailed
		}
		p.touch(now)
		return &DeployReceiptResult{Recorded: r}, nil
	}

	// 全部组件都成功才算环境晋级；失败分支已转入回退，因此这里只可能全成功。
	if len(e.Results) == len(p.snapshot) && allSucceeded(e.Results) {
		event := p.completeEnv(e, now)
		p.touch(now)
		return &DeployReceiptResult{
			Recorded: r, EnvPromoted: true, PromotionDone: p.state == PromoPromoted, EventID: event.ID,
		}, nil
	}
	p.touch(now)
	return &DeployReceiptResult{Recorded: r}, nil
}

func cloneVersionPtr(v *Version) *Version {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func failureMessage(m string) string {
	if m == "" {
		return "deployment failed"
	}
	return m
}

func allSucceeded(rs []*DeployResult) bool {
	for _, r := range rs {
		if !r.Success {
			return false
		}
	}
	return true
}

// completeEnv 把环境记为晋级成功，写出该环境唯一一条稳定 outbox 事件，并开放下一环境。
func (p *Promotion) completeEnv(e *EnvExecution, now time.Time) *OutboxEvent {
	e.State = EnvPromoted
	e.PromotedAt = now
	e.FailedAt = time.Time{}
	eventID := "evt_" + newID()
	e.EventID = eventID

	from := map[string]string{}
	for c, v := range e.Baseline {
		from[c] = v.String()
	}
	to := map[string]string{}
	for c, v := range p.snapshot {
		to[c] = v.String()
	}
	payload, _ := json.Marshal(map[string]any{
		"promotion_id": p.ID,
		"train_id":     p.TrainID,
		"snapshot_id":  p.SnapshotID,
		"environment":  e.Name,
		"attempt":      p.Attempt,
		"from":         from,
		"to":           to,
	})
	event := &OutboxEvent{
		ID:        eventID,
		TrainID:   p.TrainID,
		Type:      "environment.promoted",
		Payload:   json.RawMessage(payload),
		CreatedAt: now,
	}

	// 开放下一环境（同一次尝试），并重新评估其审批门槛。
	if e.Order+1 < len(p.envs) {
		next := p.envs[e.Order+1]
		next.Attempt = p.Attempt
		next.State = EnvAwaitingApproval
		if len(next.missingApprovals()) == 0 {
			next.State = EnvReady
		}
	} else {
		p.state = PromoPromoted
	}
	return event
}

// ---- 回退 ----

// RollbackLease 是回退领取返回：待恢复组件及其目标版本（nil 目标表示移除）。
type RollbackLease struct {
	Token   string          `json:"token"`
	Attempt int             `json:"attempt"`
	Epoch   int             `json:"epoch"`
	Worker  string          `json:"worker"`
	Tasks   []*RollbackTask `json:"tasks"`
}

// ClaimRollback 领取（或接管）当前开放环境的回退工作。
// 重新领取即接管：epoch+1，旧回退回执作废；已成功任务保留，
// 返回所有尚未成功（pending/failed）的任务。
func (p *Promotion) ClaimRollback(envName, worker string, now time.Time) (*RollbackLease, error) {
	if err := p.requirePromotionState(PromoRunning, PromoCancelling); err != nil {
		return nil, err
	}
	e, err := p.requireOpenEnv(envName)
	if err != nil {
		return nil, err
	}
	if e.State != EnvRollingBack {
		return nil, fmt.Errorf("%w: environment %q is %s, no rollback to claim", ErrStateConflict, envName, e.State)
	}
	if e.Lease.Worker != worker {
		// 不同工作者接管：epoch+1，旧回退回执作废；同工作者重领（查看剩余任务/续租）
		// 不换租约。已成功任务始终保留。
		prev := epochOf(e.Lease)
		e.Lease = &Lease{Token: "lease_" + newID(), Epoch: prev + 1, Worker: worker, At: now}
	}
	p.touch(now)

	tasks := make([]*RollbackTask, 0)
	for _, t := range e.Rollback {
		if t.Status != RollbackSucceeded {
			tasks = append(tasks, cloneRollbackTask(t))
		}
	}
	return &RollbackLease{Token: e.Lease.Token, Attempt: p.Attempt, Epoch: e.Lease.Epoch, Worker: worker, Tasks: tasks}, nil
}

// RollbackReceiptResult 是回退回执处理结果。
type RollbackReceiptResult struct {
	Task           *RollbackTask  `json:"task"`
	Idempotent     bool           `json:"idempotent"`
	RollbackDone   bool           `json:"rollback_done"`
	PromotionState PromotionState `json:"promotion_state"`
}

// ReportRollback 提交单个组件的回退结果。
// 失败的任务可在同一租约/新接管租约中重试；全部成功后：
// 部署失败路径落 failed（可创建新尝试），取消路径落 cancelled（终态）。
func (p *Promotion) ReportRollback(envName, token string, attempt, epoch int, component string,
	status RollbackStatus, message string, now time.Time) (*RollbackReceiptResult, error) {

	// 已终结（failed/cancelled）：只有完成最后任务的同一租约的重复回执幂等重放。
	if e, err := p.env(envName); err == nil && (p.state == PromoFailed || p.state == PromoCancelled) {
		if e.State == EnvFailed || e.State == EnvCancelled {
			if t := e.findRollbackTask(component); t != nil && t.Token == token && t.Epoch == epoch {
				return &RollbackReceiptResult{Task: cloneRollbackTask(t), Idempotent: true, RollbackDone: true, PromotionState: p.state}, nil
			}
		}
	}
	if err := p.requirePromotionState(PromoRunning, PromoCancelling); err != nil {
		return nil, err
	}
	e, err := p.requireOpenEnv(envName)
	if err != nil {
		return nil, err
	}
	if e.State != EnvRollingBack {
		if t := e.findRollbackTask(component); t != nil && t.Token == token && t.Epoch == epoch && e.Attempt == attempt {
			return &RollbackReceiptResult{Task: cloneRollbackTask(t), Idempotent: true, PromotionState: p.state}, nil
		}
		return nil, fmt.Errorf("%w: environment %q is %s", ErrStateConflict, envName, e.State)
	}
	if attempt != p.Attempt || e.Attempt != attempt {
		return nil, fmt.Errorf("%w: rollback receipt attempt %d is not current (attempt %d)", ErrLease, attempt, p.Attempt)
	}
	if err := e.Lease.check(token, epoch); err != nil {
		return nil, err
	}
	task := e.findRollbackTask(component)
	if task == nil {
		return nil, fmt.Errorf("%w: no rollback task for component %s in environment %q",
			ErrInvalidArgument, component, envName)
	}
	switch status {
	case RollbackSucceeded, RollbackFailed:
	default:
		return nil, fmt.Errorf("%w: rollback status must be %q or %q",
			ErrInvalidArgument, RollbackSucceeded, RollbackFailed)
	}
	// 同状态重复回执：幂等。已成功任务不允许再报失败。
	if task.Status == status && task.Epoch == epoch {
		return &RollbackReceiptResult{Task: cloneRollbackTask(task), Idempotent: true, PromotionState: p.state}, nil
	}
	if task.Status == RollbackSucceeded && status == RollbackFailed {
		return nil, fmt.Errorf("%w: component %s already rolled back successfully", ErrStateConflict, component)
	}

	task.Status = status
	task.Message = message
	task.Token = token
	task.Epoch = epoch
	task.At = now
	if status == RollbackSucceeded {
		if task.To == nil {
			p.removeInstalled(e.Name, component)
		} else {
			p.setInstalled(e.Name, component, *task.To)
		}
	}

	if !rollbackAllDone(e) {
		p.touch(now)
		return &RollbackReceiptResult{Task: cloneRollbackTask(task), PromotionState: p.state}, nil
	}

	// 全部恢复完成：按触发来源落终态。
	e.Lease = nil
	done := &RollbackReceiptResult{Task: cloneRollbackTask(task), RollbackDone: true}
	if p.state == PromoCancelling {
		e.State = EnvCancelled
		p.state = PromoCancelled
		for _, other := range p.envs {
			if other.State != EnvPromoted && other != e {
				other.State = EnvCancelled
			}
		}
	} else {
		e.State = EnvFailed
		e.FailedAt = now
		p.state = PromoFailed
	}
	done.PromotionState = p.state
	p.touch(now)
	return done, nil
}

func rollbackAllDone(e *EnvExecution) bool {
	for _, t := range e.Rollback {
		if t.Status != RollbackSucceeded {
			return false
		}
	}
	return true
}

// prepareRollback 为所有相对基线发生偏离的组件补齐回退任务并关闭部署租约，
// 返回待恢复任务数。任务数为 0 时没有任何混版状态需要恢复，调用方直接落终态。
func (p *Promotion) prepareRollback(e *EnvExecution, now time.Time) int {
	existing := map[string]struct{}{}
	for _, t := range e.Rollback {
		existing[t.Component] = struct{}{}
	}
	cur := p.installed[e.Name]
	for component := range p.snapshot {
		var base *Version
		if b, ok := e.Baseline[component]; ok {
			v := b
			base = &v
		}
		installed, hasInstalled := cur[component]
		switch {
		case base == nil && !hasInstalled:
			// 执行前没有、现在也没有：无需恢复。
		case base != nil && hasInstalled && *base == installed:
			// 与基线一致：无需恢复。
		default:
			if _, dup := existing[component]; dup {
				continue
			}
			e.Rollback = append(e.Rollback, &RollbackTask{
				Component: component, To: base, Status: RollbackPending,
			})
		}
	}
	sortRollbackTasks(e.Rollback)
	pending := 0
	for _, t := range e.Rollback {
		if t.Status != RollbackSucceeded {
			pending++
		}
	}
	// 租约立刻失效：部署回执通道关闭，回退工作者需显式领取回退租约。
	prev := epochOf(e.Lease)
	e.Lease = &Lease{Token: "lease_" + newID(), Epoch: prev + 1, Worker: "", At: now}
	return pending
}

// finalizeCancel 把晋级与所有未晋级环境落为取消终态。
func (p *Promotion) finalizeCancel() {
	p.state = PromoCancelled
	for _, o := range p.envs {
		if o.State != EnvPromoted {
			o.State = EnvCancelled
		}
	}
}

// ---- 取消 ----

// Cancel 取消晋级执行，可与审批/部署回执并发发生（由存储事务串行化）：
//   - 尚无部署动作：直接落 cancelled；
//   - 部署进行中：进入 cancelling 并立即按基线生成回退，回退完成才 cancelled；
//   - 已失败（等待新尝试）：直接取消；
//   - 已取消幂等，已晋级不可取消。
func (p *Promotion) Cancel(now time.Time) (bool, error) {
	switch p.state {
	case PromoCancelled:
		return false, nil
	case PromoPromoted:
		return false, fmt.Errorf("%w: promotion %s already promoted", ErrStateConflict, p.ID)
	case PromoCancelling:
		return false, nil
	case PromoFailed:
		p.state = PromoCancelled
		for _, e := range p.envs {
			if e.State != EnvPromoted {
				e.State = EnvCancelled
			}
		}
		p.touch(now)
		return true, nil
	}

	cur := p.currentEnv()
	if cur != nil && (cur.State == EnvDeploying || cur.State == EnvRollingBack) {
		if cur.State == EnvRollingBack {
			// 回退进行中收到取消：标记意图，等全部回退任务完成后落 cancelled。
			p.state = PromoCancelling
			p.touch(now)
			return true, nil
		}
		// 部署中被取消：按基线生成回退；若领取后尚无组件更新（零偏离），
		// 没有混版状态需要恢复，直接取消，避免空回退永久卡住。
		cur.State = EnvRollingBack
		if pending := p.prepareRollback(cur, now); pending == 0 {
			p.finalizeCancel()
			p.touch(now)
			return true, nil
		}
		p.state = PromoCancelling
		p.touch(now)
		return true, nil
	}
	// 审批等待/就绪/blocked：没有任何组件被更新，直接取消。
	p.state = PromoCancelled
	for _, e := range p.envs {
		if e.State != EnvPromoted {
			e.State = EnvCancelled
		}
	}
	p.touch(now)
	return true, nil
}

// ---- 失败后新尝试 ----

// Retry 在一次晋级失败后开启新尝试。问题修复后允许重试，
// 但必须沿用原列车快照（p.snapshot 永不替换）：审批、部署结果、回退、
// 租约全部重置，尝试号 +1。
func (p *Promotion) Retry(now time.Time) error {
	if err := p.requirePromotionState(PromoFailed); err != nil {
		return err
	}
	p.Attempt++
	p.state = PromoRunning
	for _, e := range p.envs {
		if e.State == EnvPromoted {
			continue // 已晋级环境不重做
		}
		e.resetForAttempt(p.Attempt)
	}
	// 第一个未晋级环境重新开放。
	if cur := p.currentEnv(); cur != nil {
		cur.State = EnvAwaitingApproval
		if len(cur.missingApprovals()) == 0 {
			cur.State = EnvReady
		}
	}
	p.touch(now)
	return nil
}

func (e *EnvExecution) resetForAttempt(attempt int) {
	e.Attempt = attempt
	e.State = EnvBlocked
	e.Approvals = nil
	e.Baseline = nil
	e.Results = nil
	e.Rollback = nil
	e.Lease = nil
	e.EventID = ""
	e.PromotedAt = time.Time{}
	e.FailedAt = time.Time{}
	e.FailReasons = nil
}

// ---- 安装版本维护 ----

func (p *Promotion) setInstalled(env, component string, ver Version) {
	if p.installed[env] == nil {
		p.installed[env] = map[string]Version{}
	}
	p.installed[env][component] = ver
}

func (p *Promotion) removeInstalled(env, component string) {
	if m := p.installed[env]; m != nil {
		delete(m, component)
		if len(m) == 0 {
			delete(p.installed, env)
		}
	}
}

func cloneRollbackTask(t *RollbackTask) *RollbackTask {
	cp := *t
	if t.To != nil {
		v := *t.To
		cp.To = &v
	}
	return &cp
}

func sortRollbackTasks(ts []*RollbackTask) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && ts[j-1].Component > ts[j].Component; j-- {
			ts[j-1], ts[j] = ts[j], ts[j-1]
		}
	}
}
