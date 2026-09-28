package releasetrain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---- 环境策略 ----

// PutEnvironmentPolicy 设置环境严格顺序与各环境审批门槛。
// 只影响之后创建的晋级活动；已创建活动使用创建时的快照。
// 传 nil 恢复为内置默认 dev → staging → production（均无审批门槛）。
func (s *Service) PutEnvironmentPolicy(p *EnvironmentPolicy) error {
	if p == nil {
		p = defaultEnvironmentPolicy()
	}
	if err := validateEnvironmentPolicy(p); err != nil {
		return err
	}
	frozen := cloneEnvironmentPolicy(p)
	return s.store.Update(func(st *storedState) error {
		st.envPolicy = frozen
		return nil
	})
}

// GetEnvironmentPolicy 返回当前环境定义；从未设置时返回内置默认。
func (s *Service) GetEnvironmentPolicy() *EnvironmentPolicy {
	out := defaultEnvironmentPolicy()
	_ = s.store.View(func(st *storedState) error {
		if st.envPolicy != nil {
			out = cloneEnvironmentPolicy(st.envPolicy)
		}
		return nil
	})
	return out
}

func (s *Service) effectiveEnvPolicy(st *storedState) *EnvironmentPolicy {
	if st.envPolicy != nil {
		return st.envPolicy
	}
	return defaultEnvironmentPolicy()
}

// ---- 创建晋级活动 ----

// CreatePromotion 在一列已冻结并放行（released）的列车上创建环境晋级活动。
// 创建时刻冻结：列车快照、环境严格顺序、各环境审批门槛。
// 未放行列车、或快照中版本在登记库中已不完整时不能开始。
func (s *Service) CreatePromotion(trainID, requestID string) (*Promotion, error) {
	fp := fingerprint(struct {
		TrainID string `json:"train_id"`
		Kind    string `json:"kind"`
	}{trainID, "create_promotion"})

	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "create_promotion", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		t, err := requireTrain(st, trainID)
		if err != nil {
			return err
		}
		if t.State() != StateReleased {
			return fmt.Errorf("%w: train %s is %s, only a released train can start environment promotion",
				ErrStateConflict, trainID, t.State())
		}
		if len(t.Snapshot()) == 0 || t.SnapshotID() == "" {
			return fmt.Errorf("%w: train %s has no frozen snapshot", ErrInvalidArgument, trainID)
		}
		// 依赖快照完整性：快照中每个版本都必须仍在登记库中。
		reg := registryAdapter{st.versions}
		var missing []string
		for comp, ver := range t.Snapshot() {
			if _, ok := reg.constraintsOf(comp, ver); !ok {
				missing = append(missing, fmt.Sprintf("component %s version %s is no longer registered", comp, ver))
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return &DependencyError{Problems: missing}
		}
		// 一列车至多一次晋级活动；失败后用“新尝试”而不是新建活动。
		for _, existing := range st.promotions {
			if existing.TrainID == trainID {
				return fmt.Errorf("%w: train %s already has promotion %s",
					ErrStateConflict, trainID, existing.ID)
			}
		}

		p := newPromotion("promo_"+newID(), trainID, t.Snapshot(), t.SnapshotID(),
			s.effectiveEnvPolicy(st), s.now().UTC())
		st.promotions[p.ID] = p
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "create_promotion", p.ID, fp, promotionToWire(p))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func decodePromotion(raw json.RawMessage) *Promotion {
	var w promotionWire
	if err := json.Unmarshal(raw, &w); err != nil {
		panic(err)
	}
	return promotionFromWire(&w)
}

func requirePromotion(st *storedState, id string) (*Promotion, error) {
	p, ok := st.promotions[id]
	if !ok {
		return nil, fmt.Errorf("%w: promotion %s", ErrNotFound, id)
	}
	return p, nil
}

// ---- 环境审批 ----

// PromotionApproveInput 是环境审批请求。
type PromotionApproveInput struct {
	PromotionID string
	Environment string
	Person      string
	Role        string
	RequestID   string
}

// ApprovePromotion 为当前尝试的某环境记录审批。资格只认活动创建时的门槛快照；
// 同一 (person, role) 幂等；新尝试重新计票。
func (s *Service) ApprovePromotion(in PromotionApproveInput) (*Promotion, error) {
	if strings.TrimSpace(in.Person) == "" || strings.TrimSpace(in.Role) == "" ||
		strings.TrimSpace(in.Environment) == "" {
		return nil, fmt.Errorf("%w: environment, person and role are required", ErrInvalidArgument)
	}
	fp := fingerprint(struct {
		PromoID string `json:"promotion_id"`
		Env     string `json:"environment"`
		Person  string `json:"person"`
		Role    string `json:"role"`
	}{in.PromotionID, in.Environment, in.Person, in.Role})

	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, in.RequestID, "promotion_approve", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		p, err := requirePromotion(st, in.PromotionID)
		if err != nil {
			return err
		}
		if _, err := p.Approve(in.Environment, in.Person, in.Role, in.RequestID, s.now().UTC()); err != nil {
			return err
		}
		out = clonePromotion(p)
		return saveIdem(st.idem, in.RequestID, "promotion_approve", p.ID, fp, promotionToWire(p))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 部署领取与回执 ----

// ClaimDeployment 领取某环境的部署工作，返回租约号与尝试号（fencing token）。
// 同一工作者对其仍持有的活动租约重复领取返回原租约；其他工作者领取视为接管，
// 租约号 +1，旧租约随后提交的回执将被拒绝。
func (s *Service) ClaimDeployment(promotionID, environment, worker string) (*DeploymentLease, error) {
	if strings.TrimSpace(environment) == "" || strings.TrimSpace(worker) == "" {
		return nil, fmt.Errorf("%w: environment and worker are required", ErrInvalidArgument)
	}
	var out *DeploymentLease
	err := s.store.Update(func(st *storedState) error {
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		a := p.CurrentAttempt()
		// 同一工作者重领其活动租约：返回原租约，不递增租约号。
		if cur := currentDeployLease(p, a, environment, worker); cur != nil {
			out = cur
			return nil
		}
		lease, err := p.ClaimDeployment(environment, worker, st.live, s.now().UTC())
		if err != nil {
			return err
		}
		out = lease
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// currentDeployLease 在该环境正由同一 worker 部署时返回其现有租约，否则 nil。
func currentDeployLease(p *Promotion, a *Attempt, environment, worker string) *DeploymentLease {
	if a == nil {
		return nil
	}
	for _, e := range a.Envs() {
		if e.Name != environment || e.Status != EnvDeploying {
			continue
		}
		if e.LeaseWorker() == worker {
			return &DeploymentLease{
				AttemptNo:   a.No,
				LeaseNo:     e.LeaseNo(),
				Worker:      worker,
				Environment: environment,
				Targets:     p.Snapshot(),
				Before:      e.BeforeVersions(),
			}
		}
	}
	return nil
}

// ReceiptEntry 是单组件部署回执输入。
type ReceiptEntry struct {
	Component       string
	Success         bool
	DeployedVersion *Version
	Message         string
}

// ReceiptOutcome 是一批回执处理后的结果。
type ReceiptOutcome struct {
	PromotionID string `json:"promotion_id"`
	AttemptNo   int    `json:"attempt_no"`
	Environment string `json:"environment"`
	Status      string `json:"status"`
	Promoted    bool   `json:"promoted"`
	EventID     string `json:"event_id,omitempty"`
	Idempotent  bool   `json:"idempotent,omitempty"`
}

// SubmitReceipts 提交部署回执。只有当前尝试的当前租约可提交；旧尝试/旧租约/被接管
// 租约的回执一律拒绝。重复回执与带 request_id 的请求重试返回已有结果。
// 全部组件成功才把环境记为晋级并写唯一 outbox；任一失败即启动回退。
func (s *Service) SubmitReceipts(promotionID string, attemptNo, leaseNo int, environment string,
	entries []ReceiptEntry, requestID string) (*ReceiptOutcome, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: at least one deployment receipt is required", ErrInvalidArgument)
	}
	fpInputs := make([]struct {
		Component string `json:"component"`
		Success   bool   `json:"success"`
		Version   string `json:"version,omitempty"`
		Message   string `json:"message,omitempty"`
	}, 0, len(entries))
	for _, e := range entries {
		v := ""
		if e.DeployedVersion != nil {
			v = e.DeployedVersion.String()
		}
		fpInputs = append(fpInputs, struct {
			Component string `json:"component"`
			Success   bool   `json:"success"`
			Version   string `json:"version,omitempty"`
			Message   string `json:"message,omitempty"`
		}{e.Component, e.Success, v, e.Message})
	}
	fp := fingerprint(struct {
		PromoID  string `json:"promotion_id"`
		Attempt  int    `json:"attempt_no"`
		Lease    int    `json:"lease_no"`
		Env      string `json:"environment"`
		Receipts any    `json:"receipts"`
	}{promotionID, attemptNo, leaseNo, environment, fpInputs})

	var out *ReceiptOutcome
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "promotion_receipts", fp); err != nil {
			return err
		} else if replayed {
			var o ReceiptOutcome
			if err := json.Unmarshal(raw, &o); err != nil {
				return err
			}
			o.Idempotent = true
			out = &o
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		receipts := make([]ReceiptInput, 0, len(entries))
		for _, e := range entries {
			receipts = append(receipts, ReceiptInput{
				Component:       e.Component,
				Success:         e.Success,
				DeployedVersion: e.DeployedVersion,
				Message:         e.Message,
				RequestID:       requestID,
			})
		}
		promotedNow, err := p.SubmitReceipts(attemptNo, leaseNo, environment, receipts, st.live, s.now().UTC())
		if err != nil {
			return err
		}
		outcome := &ReceiptOutcome{PromotionID: promotionID, AttemptNo: attemptNo, Environment: environment}
		a := p.CurrentAttempt()
		env, _ := a.env(environment)
		outcome.Status = string(env.Status)
		if promotedNow {
			outcome.Promoted = true
			outcome.EventID = env.EventID()
			// 每个环境成功只写出一条稳定 outbox：幂等重放/请求重试时事件已存在则跳过。
			exists := false
			for _, ev := range st.outbox {
				if ev.ID == env.EventID() {
					exists = true
					break
				}
			}
			if !exists {
				payload, _ := json.Marshal(map[string]any{
					"promotion_id": p.ID,
					"train_id":     p.TrainID,
					"snapshot_id":  p.SnapshotID(),
					"attempt_no":   a.No,
					"environment":  environment,
					"components":   p.Snapshot(),
				})
				st.outbox = append(st.outbox, OutboxEvent{
					ID:        env.EventID(),
					TrainID:   p.TrainID,
					Type:      "environment.promoted",
					Payload:   json.RawMessage(payload),
					CreatedAt: env.PromotedAt(),
				})
			}
		}
		out = outcome
		return saveIdem(st.idem, requestID, "promotion_receipts", p.ID, fp, outcome)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 回退 ----

// ClaimRollback 领取下一个待回退组件单元（含失败可重试的单元）。
// 无待回退单元时返回 ErrNotFound。
func (s *Service) ClaimRollback(promotionID, worker string) (*RollbackLease, error) {
	if strings.TrimSpace(worker) == "" {
		return nil, fmt.Errorf("%w: worker is required", ErrInvalidArgument)
	}
	var out *RollbackLease
	err := s.store.Update(func(st *storedState) error {
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		lease, err := p.ClaimRollback(worker, s.now().UTC())
		if err != nil {
			return err
		}
		out = lease
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RollbackOutcome 是单个回退单元的处理结果。
type RollbackOutcome struct {
	PromotionID   string `json:"promotion_id"`
	AttemptNo     int    `json:"attempt_no"`
	Environment   string `json:"environment"`
	Component     string `json:"component"`
	State         string `json:"state"`
	RollbackDone  bool   `json:"rollback_done"`  // 本环境回退是否已全部完成
	AttemptFailed bool   `json:"attempt_failed"` // 本次尝试是否已落为 failed（可创建新尝试）
}

// ReportRollback 提交单个回退单元结果，并在全部单元完成时落定尝试状态。
func (s *Service) ReportRollback(promotionID string, attemptNo, leaseNo int,
	environment, component string, success bool, message string) (*RollbackOutcome, error) {
	var out *RollbackOutcome
	err := s.store.Update(func(st *storedState) error {
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		done, err := p.ReportRollback(attemptNo, leaseNo, environment, component,
			success, message, st.live, s.now().UTC())
		if err != nil {
			return err
		}
		a := p.CurrentAttempt()
		env, _ := a.env(environment)
		out = &RollbackOutcome{
			PromotionID:   promotionID,
			AttemptNo:     attemptNo,
			Environment:   environment,
			Component:     component,
			State:         string(unitState(env, component)),
			RollbackDone:  done,
			AttemptFailed: a.Status == AttemptFailed,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func unitState(e *EnvExecution, component string) RollbackUnitState {
	for _, u := range e.RollbackUnits() {
		if u.Component == component {
			return u.State
		}
	}
	return ""
}

// ---- 取消 / 新尝试 ----

// CancelPromotion 取消晋级活动。有组件已更新时转入 cancel_requested 并回退，
// 回退完成后才 cancelled；重复取消幂等。返回取消后的活动与是否启动了回退。
func (s *Service) CancelPromotion(promotionID, requestID string) (*Promotion, bool, error) {
	fp := fingerprint(struct {
		PromoID string `json:"promotion_id"`
		Kind    string `json:"kind"`
	}{promotionID, "cancel_promotion"})

	var out *Promotion
	var rollbackStarted bool
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "cancel_promotion", fp); err != nil {
			return err
		} else if replayed {
			out = decodePromotion(raw)
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		started, err := p.Cancel(s.now().UTC())
		if err != nil {
			return err
		}
		rollbackStarted = started
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "cancel_promotion", p.ID, fp, promotionToWire(p))
	})
	if err != nil {
		return nil, false, err
	}
	return out, rollbackStarted, nil
}

// NewAttemptResult 是创建新尝试的结果。
type NewAttemptResult struct {
	PromotionID string `json:"promotion_id"`
	AttemptNo   int    `json:"attempt_no"`
}

// StartNewAttempt 在最近一次尝试失败（回退完成）后开启新尝试。
// 新尝试继续使用原列车快照；现网已处于快照版本的环境直接继承、不重复部署、不重复出 outbox。
func (s *Service) StartNewAttempt(promotionID, requestID string) (*NewAttemptResult, *Promotion, error) {
	fp := fingerprint(struct {
		PromoID string `json:"promotion_id"`
		Kind    string `json:"kind"`
	}{promotionID, "new_attempt"})

	var res *NewAttemptResult
	var out *Promotion
	err := s.store.Update(func(st *storedState) error {
		if replayed, raw, err := checkIdem(st.idem, requestID, "new_attempt", fp); err != nil {
			return err
		} else if replayed {
			var r NewAttemptResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			p, _ := requirePromotion(st, r.PromotionID)
			res = &r
			out = clonePromotion(p)
			return nil
		}
		p, err := requirePromotion(st, promotionID)
		if err != nil {
			return err
		}
		no, err := p.StartNewAttempt(st.live, s.now().UTC())
		if err != nil {
			return err
		}
		res = &NewAttemptResult{PromotionID: promotionID, AttemptNo: no}
		out = clonePromotion(p)
		return saveIdem(st.idem, requestID, "new_attempt", p.ID, fp, res)
	})
	if err != nil {
		return nil, nil, err
	}
	return res, out, nil
}

// ---- 查询 ----

// GetPromotion 返回晋级活动当前状态（克隆）。
func (s *Service) GetPromotion(id string) (*Promotion, error) {
	var out *Promotion
	err := s.store.View(func(st *storedState) error {
		p, err := requirePromotion(st, id)
		if err != nil {
			return err
		}
		out = clonePromotion(p)
		return nil
	})
	return out, err
}

// ListPromotions 返回全部晋级活动（按 ID 排序）。
func (s *Service) ListPromotions() ([]*Promotion, error) {
	var out []*Promotion
	err := s.store.View(func(st *storedState) error {
		ids := make([]string, 0, len(st.promotions))
		for id := range st.promotions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out = make([]*Promotion, 0, len(ids))
		for _, id := range ids {
			out = append(out, clonePromotion(st.promotions[id]))
		}
		return nil
	})
	return out, err
}

// EnvView 是单个环境的查询视图：执行前后版本、组件结果、回退进度、审批依据与阻断原因。
type EnvView struct {
	Name               string                  `json:"name"`
	Status             string                  `json:"status"`
	AttemptNo          int                     `json:"attempt_no"`
	BeforeVersions     map[string]string       `json:"before_versions"`  // 执行前版本；null 表示此前无该组件
	TargetVersions     map[string]string       `json:"target_versions"`  // 本次部署目标（列车快照）
	CurrentVersions    map[string]string       `json:"current_versions"` // 现网实际版本
	Results            []ComponentDeployResult `json:"results"`
	Rollback           []RollbackUnit          `json:"rollback"`
	ApprovalPolicy     EnvPolicy               `json:"approval_policy"`
	Approvals          []EnvApproval           `json:"approvals"`
	MissingApprovals   []string                `json:"missing_approvals"`
	Lease              *LeaseView              `json:"lease,omitempty"`
	BlockingReason     string                  `json:"blocking_reason,omitempty"`
	PromotedAt         *time.Time              `json:"promoted_at,omitempty"`
	EventID            string                  `json:"event_id,omitempty"`
	CarriedFromAttempt int                     `json:"carried_from_attempt,omitempty"`
}

// LeaseView 展示当前部署租约。
type LeaseView struct {
	AttemptNo int    `json:"attempt_no"`
	LeaseNo   int    `json:"lease_no"`
	Worker    string `json:"worker"`
}

// AttemptView 是一次尝试的查询视图。
type AttemptView struct {
	No           int       `json:"no"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	Environments []EnvView `json:"environments"`
}

// PromotionView 是晋级活动的完整查询视图。
type PromotionView struct {
	ID             string            `json:"id"`
	TrainID        string            `json:"train_id"`
	State          string            `json:"state"`
	Version        int               `json:"version"`
	SnapshotID     string            `json:"snapshot_id"`
	Snapshot       map[string]string `json:"snapshot"`
	Environments   []string          `json:"environments"`
	Attempts       []AttemptView     `json:"attempts"`
	BlockingReason string            `json:"blocking_reason,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

// GetPromotionView 组装带现网版本、审批依据、阻断原因的查询视图。
func (s *Service) GetPromotionView(id string) (*PromotionView, error) {
	var out *PromotionView
	err := s.store.View(func(st *storedState) error {
		p, err := requirePromotion(st, id)
		if err != nil {
			return err
		}
		pc := clonePromotion(p)
		view := &PromotionView{
			ID:           pc.ID,
			TrainID:      pc.TrainID,
			State:        string(pc.state),
			Version:      pc.Version,
			SnapshotID:   pc.snapshotID,
			Snapshot:     versionsToStrings(pc.snapshot),
			Environments: pc.environments,
			CreatedAt:    pc.createdAt,
			UpdatedAt:    pc.updatedAt,
		}
		isCurrent := make(map[int]bool)
		if cur := pc.currentAttempt(); cur != nil {
			isCurrent[cur.No] = true
		}
		for _, a := range pc.attempts {
			av := AttemptView{No: a.No, Status: string(a.Status), StartedAt: a.StartedAt, FinishedAt: a.FinishedAt}
			prevDone := true
			for idx, e := range a.envs {
				ev := buildEnvView(pc, a, e, st.live)
				if isCurrent[a.No] {
					ev.BlockingReason = blockingReason(pc, a, e, idx, prevDone)
				}
				if e.Status == EnvPromoted {
					prevDone = true
				} else {
					prevDone = false
				}
				av.Environments = append(av.Environments, ev)
			}
			view.Attempts = append(view.Attempts, av)
		}
		if cur := pc.currentAttempt(); cur != nil && pc.state == PromActive && cur.Status == AttemptRunning {
			view.BlockingReason = overallBlocking(pc, cur)
		}
		out = view
		return nil
	})
	return out, err
}

func buildEnvView(p *Promotion, a *Attempt, e *EnvExecution,
	live map[string]map[string]Version) EnvView {
	ev := EnvView{
		Name:               e.Name,
		Status:             string(e.Status),
		AttemptNo:          a.No,
		TargetVersions:     versionsToStrings(p.snapshot),
		ApprovalPolicy:     e.policy,
		Approvals:          append([]EnvApproval(nil), e.approvals...),
		MissingApprovals:   e.missingApprovals(),
		Results:            append([]ComponentDeployResult(nil), e.results...),
		EventID:            e.eventID,
		CarriedFromAttempt: e.carriedFromAttempt,
	}
	ev.BeforeVersions = map[string]string{}
	for comp, v := range e.beforeVersions {
		if v != nil {
			ev.BeforeVersions[comp] = v.String()
		} else {
			ev.BeforeVersions[comp] = ""
		}
	}
	ev.CurrentVersions = map[string]string{}
	for comp := range p.snapshot {
		if cur, ok := live[e.Name][comp]; ok {
			ev.CurrentVersions[comp] = cur.String()
		}
	}
	for _, u := range e.rollback {
		ev.Rollback = append(ev.Rollback, *u)
	}
	if e.Status == EnvDeploying && e.leaseNo > 0 {
		ev.Lease = &LeaseView{AttemptNo: a.No, LeaseNo: e.leaseNo, Worker: e.leaseWorker}
	}
	if !e.promotedAt.IsZero() {
		at := e.promotedAt
		ev.PromotedAt = &at
	}
	return ev
}

// blockingReason 计算当前尝试中某环境此刻不能继续的原因；可继续则返回空串。
func blockingReason(p *Promotion, a *Attempt, e *EnvExecution, idx int, prevDone bool) string {
	switch e.Status {
	case EnvPromoted:
		return ""
	case EnvReady:
		return ""
	case EnvDeploying:
		return fmt.Sprintf("deployment in progress under lease %d (worker %s); waiting for all component receipts",
			e.leaseNo, e.leaseWorker)
	case EnvRollingBack:
		return "deployment failed; rolling back updated components before a new attempt can be created"
	case EnvFailed:
		return "rollback complete; create a new attempt after fixing the problem"
	case EnvWaitingApproval:
		if !prevDone {
			if idx > 0 {
				return fmt.Sprintf("waiting for previous environment %q to be promoted", a.envs[idx-1].Name)
			}
		}
		if missing := e.missingApprovals(); len(missing) > 0 {
			return "approval gate not met: " + strings.Join(missing, "; ")
		}
	}
	// 前序环境未晋级优先提示。
	if !prevDone && idx > 0 && a.envs[idx-1].Status != EnvPromoted {
		return fmt.Sprintf("blocked by previous environment %q (status %s)",
			a.envs[idx-1].Name, a.envs[idx-1].Status)
	}
	return ""
}

func overallBlocking(p *Promotion, a *Attempt) string {
	for idx, e := range a.envs {
		prevDone := idx == 0 || a.envs[idx-1].Status == EnvPromoted
		if r := blockingReason(p, a, e, idx, prevDone); r != "" {
			return fmt.Sprintf("environment %q: %s", e.Name, r)
		}
		if e.Status != EnvPromoted && e.Status != EnvReady {
			return fmt.Sprintf("environment %q: %s", e.Name, e.Status)
		}
	}
	return ""
}
